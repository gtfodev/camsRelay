package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/bridge"
	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/config"
	"github.com/ethan/nest-cloudflare-relay/pkg/nest"
	"github.com/ethan/nest-cloudflare-relay/pkg/rtp"
	rtspClient "github.com/ethan/nest-cloudflare-relay/pkg/rtsp"
	pionRTP "github.com/pion/rtp"
)

func main() {
	// Initialize structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	logger.Info("starting Nest Camera → Cloudflare SFU relay - Phase 2")

	// Load configuration
	cfg, err := config.Load(".env")
	if err != nil {
		logger.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}
	logger.Info("configuration loaded")

	// Create context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.Info("received shutdown signal", "signal", sig)
		cancel()
	}()

	// Initialize Nest client
	nestClient := nest.NewClient(
		cfg.Google.ClientID,
		cfg.Google.ClientSecret,
		cfg.Google.RefreshToken,
		logger.With("component", "nest"),
	)
	logger.Info("Nest client initialized")

	// List available cameras
	devices, err := nestClient.ListDevices(ctx, cfg.Google.ProjectID)
	if err != nil {
		logger.Error("failed to list devices", "error", err)
		os.Exit(1)
	}

	logger.Info("cameras discovered", "count", len(devices))
	for i, device := range devices {
		displayName := device.Traits.Info.CustomName
		if displayName == "" && len(device.Relations) > 0 {
			displayName = device.Relations[0].DisplayName
		}
		if displayName == "" {
			displayName = device.DeviceID
		}

		logger.Info("camera",
			"index", i+1,
			"name", displayName,
			"device_id", device.DeviceID,
			"protocols", device.Traits.CameraLiveStream.SupportedProtocols,
			"video_codecs", device.Traits.CameraLiveStream.VideoCodecs,
			"audio_codecs", device.Traits.CameraLiveStream.AudioCodecs,
		)
	}

	if len(devices) == 0 {
		logger.Warn("no cameras found")
		os.Exit(0)
	}

	// Initialize Cloudflare client
	cfClient := cloudflare.NewClient(
		cfg.Cloudflare.AppID,
		cfg.Cloudflare.APIToken,
		logger.With("component", "cloudflare"),
	)
	logger.Info("Cloudflare client initialized")

	// Select first camera for proof of concept
	firstCamera := devices[0]
	displayName := firstCamera.Traits.Info.CustomName
	if displayName == "" && len(firstCamera.Relations) > 0 {
		displayName = firstCamera.Relations[0].DisplayName
	}
	if displayName == "" {
		displayName = firstCamera.DeviceID
	}

	logger.Info("starting stream for camera",
		"name", displayName,
		"device_id", firstCamera.DeviceID)

	// Generate RTSP stream
	stream, err := nestClient.GenerateRTSPStream(ctx, cfg.Google.ProjectID, firstCamera.DeviceID)
	if err != nil {
		logger.Error("failed to generate RTSP stream", "error", err)
		os.Exit(1)
	}

	logger.Info("RTSP stream generated",
		"url", stream.URL,
		"expires_at", stream.ExpiresAt.Format(time.RFC3339),
		"ttl_seconds", int(time.Until(stream.ExpiresAt).Seconds()))

	// Start stream manager for automatic extension
	streamMgr := nest.NewStreamManager(
		nestClient,
		stream,
		logger.With("component", "stream-manager"),
	)
	streamMgr.Start()
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		if err := streamMgr.Stop(stopCtx); err != nil {
			logger.Error("failed to stop stream manager", "error", err)
		}
	}()

	// Create WebRTC bridge to Cloudflare
	webrtcBridge, err := bridge.NewBridge(ctx, cfClient, logger.With("component", "bridge"))
	if err != nil {
		logger.Error("failed to create bridge", "error", err)
		os.Exit(1)
	}
	defer webrtcBridge.Close()

	// Create Cloudflare session and setup WebRTC
	if err := webrtcBridge.CreateSession(ctx); err != nil {
		logger.Error("failed to create Cloudflare session", "error", err)
		os.Exit(1)
	}

	// Negotiate SDP with Cloudflare
	if err := webrtcBridge.Negotiate(ctx); err != nil {
		logger.Error("failed to negotiate with Cloudflare", "error", err)
		os.Exit(1)
	}

	logger.Info("WebRTC bridge established",
		"session_id", webrtcBridge.GetSessionID(),
		"state", webrtcBridge.GetConnectionState().String())

	// Create RTSP client
	rtspConn := rtspClient.NewClient(stream.URL, logger.With("component", "rtsp"))

	// Connect to RTSP server
	if err := rtspConn.Connect(ctx); err != nil {
		logger.Error("failed to connect to RTSP server", "error", err)
		os.Exit(1)
	}
	defer rtspConn.Close()

	// Setup RTP processors
	h264Proc := rtp.NewH264Processor()
	aacProc := rtp.NewAACProcessor()

	// Packet counters for stats
	var videoPacketCount, audioPacketCount atomic.Uint64
	var videoFrameCount, audioFrameCount atomic.Uint64

	// Setup H.264 frame handler
	h264Proc.OnFrame = func(nalus []byte, keyframe bool) {
		videoFrameCount.Add(1)

		// Write to WebRTC bridge
		// Note: For production, we'd use proper timing, but for POC we use fixed duration
		if err := webrtcBridge.WriteVideoSample(nalus, 33*time.Millisecond); err != nil {
			logger.Warn("failed to write video sample", "error", err)
		}

		if videoFrameCount.Load()%30 == 0 { // Log every 30 frames (~1 second)
			logger.Debug("video frame written",
				"frame_count", videoFrameCount.Load(),
				"keyframe", keyframe,
				"size_bytes", len(nalus))
		}
	}

	// Setup AAC frame handler
	aacProc.OnFrame = func(frame []byte) {
		audioFrameCount.Add(1)

		// Note: For production, AAC would need transcoding to Opus
		// For now, we log but don't forward (Cloudflare expects Opus)
		if audioFrameCount.Load()%100 == 0 { // Log every 100 frames
			logger.Debug("audio frame received",
				"frame_count", audioFrameCount.Load(),
				"size_bytes", len(frame))
		}

		// TODO: Transcode AAC to Opus and write to audio track
		// For Phase 2 POC, we're focusing on video only
	}

	// Setup RTP packet handler
	rtspConn.OnRTPPacket = func(channel byte, packet *pionRTP.Packet) {
		ch, ok := rtspConn.Channels[channel]
		if !ok {
			return
		}

		if ch.MediaType == "video" {
			videoPacketCount.Add(1)
			if err := h264Proc.ProcessPacket(packet); err != nil {
				logger.Warn("failed to process H.264 packet", "error", err)
			}
		} else if ch.MediaType == "audio" {
			audioPacketCount.Add(1)
			if err := aacProc.ProcessPacket(packet); err != nil {
				logger.Warn("failed to process AAC packet", "error", err)
			}
		}
	}

	// Setup all tracks
	if err := rtspConn.SetupTracks(ctx); err != nil {
		logger.Error("failed to setup tracks", "error", err)
		os.Exit(1)
	}

	// Start playing
	if err := rtspConn.Play(ctx); err != nil {
		logger.Error("failed to start RTSP playback", "error", err)
		os.Exit(1)
	}

	logger.Info("RTSP playback started - streaming to Cloudflare")

	// Start stats logger
	statsTicker := time.NewTicker(10 * time.Second)
	defer statsTicker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-statsTicker.C:
				logger.Info("streaming statistics",
					"video_packets", videoPacketCount.Load(),
					"video_frames", videoFrameCount.Load(),
					"audio_packets", audioPacketCount.Load(),
					"audio_frames", audioFrameCount.Load(),
					"webrtc_state", webrtcBridge.GetConnectionState().String(),
					"stream_ttl", streamMgr.GetTimeUntilExpiry().String())
			}
		}
	}()

	// Read packets until context cancelled
	logger.Info("ready - press Ctrl+C to stop")
	fmt.Println("\n✓ Phase 2 Complete - Full Pipeline Active:")
	fmt.Printf("  - Camera: %s\n", displayName)
	fmt.Printf("  - RTSP: %s\n", stream.URL)
	fmt.Printf("  - Cloudflare Session: %s\n", webrtcBridge.GetSessionID())
	fmt.Printf("  - Stream auto-extension: enabled\n")
	fmt.Printf("  - Pipeline: RTSP → RTP → H.264 → WebRTC → Cloudflare\n\n")

	if err := rtspConn.ReadPackets(ctx); err != nil && ctx.Err() == nil {
		logger.Error("error reading packets", "error", err)
		os.Exit(1)
	}

	logger.Info("graceful shutdown complete")
}
