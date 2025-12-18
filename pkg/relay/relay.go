package relay

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/bridge"
	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/nest"
	"github.com/ethan/nest-cloudflare-relay/pkg/rtp"
	rtspClient "github.com/ethan/nest-cloudflare-relay/pkg/rtsp"
	pionRTP "github.com/pion/rtp"
)

// CameraRelay manages the complete pipeline for a single camera:
// Nest RTSP stream → RTP processors → WebRTC bridge → Cloudflare
type CameraRelay struct {
	cameraID  string
	deviceID  string
	stream    *nest.RTSPStream
	cfClient  *cloudflare.Client
	logger    *slog.Logger

	// Pipeline components
	rtspConn  *rtspClient.Client
	h264Proc  *rtp.H264Processor
	aacProc   *rtp.AACProcessor
	webrtcBridge *bridge.Bridge

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Statistics
	videoPacketCount atomic.Uint64
	audioPacketCount atomic.Uint64
	videoFrameCount  atomic.Uint64
	audioFrameCount  atomic.Uint64
	startTime        time.Time

	// Callbacks for error recovery
	OnRTSPDisconnect   func(cameraID string, err error) // Trigger stream regeneration
	OnWebRTCDisconnect func(cameraID string, err error) // Trigger session recreation
}

// NewCameraRelay creates a relay for a single camera
func NewCameraRelay(
	cameraID string,
	deviceID string,
	stream *nest.RTSPStream,
	cfClient *cloudflare.Client,
	logger *slog.Logger,
) *CameraRelay {
	ctx, cancel := context.WithCancel(context.Background())

	return &CameraRelay{
		cameraID:  cameraID,
		deviceID:  deviceID,
		stream:    stream,
		cfClient:  cfClient,
		logger:    logger.With("camera_id", cameraID, "component", "relay"),
		ctx:       ctx,
		cancel:    cancel,
		startTime: time.Now(),
	}
}

// Start initializes the complete relay pipeline and begins streaming
func (r *CameraRelay) Start(ctx context.Context) error {
	r.logger.Info("starting camera relay",
		"stream_url", r.stream.URL,
		"expires_at", r.stream.ExpiresAt.Format(time.RFC3339))

	// Create WebRTC bridge to Cloudflare
	var err error
	r.webrtcBridge, err = bridge.NewBridge(r.ctx, r.cfClient, r.logger.With("component", "bridge"))
	if err != nil {
		return fmt.Errorf("create bridge: %w", err)
	}

	// Create Cloudflare session
	if err := r.webrtcBridge.CreateSession(ctx); err != nil {
		return fmt.Errorf("create session: %w", err)
	}

	// Negotiate SDP
	if err := r.webrtcBridge.Negotiate(ctx); err != nil {
		return fmt.Errorf("negotiate: %w", err)
	}

	r.logger.Info("WebRTC bridge established",
		"session_id", r.webrtcBridge.GetSessionID(),
		"state", r.webrtcBridge.GetConnectionState().String())

	// Wait for PeerConnection to reach "connected" state before starting RTSP
	// This ensures ICE connectivity is fully established before we start sending RTP packets
	// Without this, we may send packets before the peer connection is ready, causing them to be dropped
	r.logger.Info("waiting for WebRTC connection to be established")
	if err := r.waitForConnection(ctx); err != nil {
		return fmt.Errorf("wait for WebRTC connection: %w", err)
	}
	r.logger.Info("WebRTC connection established, starting RTSP stream")

	// Create RTSP client
	r.rtspConn = rtspClient.NewClient(r.stream.URL, r.logger.With("component", "rtsp"))

	// Connect to RTSP server
	if err := r.rtspConn.Connect(ctx); err != nil {
		return fmt.Errorf("connect RTSP: %w", err)
	}

	// Setup RTP processors
	r.h264Proc = rtp.NewH264Processor()
	r.aacProc = rtp.NewAACProcessor()

	// Setup H.264 frame handler
	r.h264Proc.OnFrame = func(nalus []byte, timestamp uint32, keyframe bool) {
		r.videoFrameCount.Add(1)
		frameCount := r.videoFrameCount.Load()

		// Write to WebRTC bridge with original RTSP timestamp (passthrough)
		if err := r.webrtcBridge.WriteVideoSample(nalus, timestamp); err != nil {
			r.logger.Error("failed to write video sample",
				"frame_count", frameCount,
				"timestamp", timestamp,
				"keyframe", keyframe,
				"connection_state", r.webrtcBridge.GetConnectionState().String(),
				"error", err)
			return
		}

		// Log successful writes periodically
		if frameCount == 1 {
			r.logger.Info("first video frame written successfully",
				"keyframe", keyframe,
				"timestamp", timestamp,
				"size_bytes", len(nalus),
				"connection_state", r.webrtcBridge.GetConnectionState().String())
		} else if frameCount%300 == 0 { // Log every 10 seconds @ 30fps
			r.logger.Info("video frames written",
				"frame_count", frameCount,
				"timestamp", timestamp,
				"keyframe", keyframe,
				"size_bytes", len(nalus),
				"connection_state", r.webrtcBridge.GetConnectionState().String())
		}
	}

	// Setup AAC frame handler (audio not transcoded yet)
	r.aacProc.OnFrame = func(frame []byte, timestamp uint32) {
		r.audioFrameCount.Add(1)
		// TODO: Transcode AAC to Opus for Cloudflare
		// For now, we just count the frames
		// When audio is enabled, call: r.webrtcBridge.WriteAudioSample(frame, timestamp)
	}

	// Setup RTP packet handler
	r.rtspConn.OnRTPPacket = func(channel byte, packet *pionRTP.Packet) {
		ch, ok := r.rtspConn.Channels[channel]
		if !ok {
			return
		}

		if ch.MediaType == "video" {
			r.videoPacketCount.Add(1)
			if err := r.h264Proc.ProcessPacket(packet); err != nil {
				r.logger.Warn("failed to process H.264 packet", "error", err)
			}
		} else if ch.MediaType == "audio" {
			r.audioPacketCount.Add(1)
			if err := r.aacProc.ProcessPacket(packet); err != nil {
				r.logger.Warn("failed to process AAC packet", "error", err)
			}
		}
	}

	// Setup all tracks
	if err := r.rtspConn.SetupTracks(ctx); err != nil {
		return fmt.Errorf("setup tracks: %w", err)
	}

	// Start playing
	if err := r.rtspConn.Play(ctx); err != nil {
		return fmt.Errorf("start playback: %w", err)
	}

	r.logger.Info("RTSP playback started - relay is active")

	// Start monitoring goroutines
	r.wg.Add(2)
	go r.statsLoop()
	go r.monitorLoop()

	// Start reading packets
	r.wg.Add(1)
	go r.readLoop()

	return nil
}

// waitForConnection waits for the WebRTC peer connection to reach "connected" state
func (r *CameraRelay) waitForConnection(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timeout waiting for connection (state=%s): %w",
				r.webrtcBridge.GetConnectionState().String(), waitCtx.Err())
		case <-ticker.C:
			state := r.webrtcBridge.GetConnectionState()
			r.logger.Debug("checking connection state", "state", state.String())

			if state.String() == "connected" {
				return nil
			}

			// Fail fast if connection failed
			if state.String() == "failed" || state.String() == "closed" {
				return fmt.Errorf("peer connection failed: state=%s", state.String())
			}
		}
	}
}

// Stop gracefully stops the relay
func (r *CameraRelay) Stop() error {
	r.logger.Info("stopping camera relay")

	// Cancel context to signal all goroutines
	r.cancel()

	// Close RTSP connection (stops packet reading)
	if r.rtspConn != nil {
		if err := r.rtspConn.Close(); err != nil {
			r.logger.Error("error closing RTSP connection", "error", err)
		}
	}

	// Wait for goroutines to exit
	r.wg.Wait()

	// Close WebRTC bridge
	if r.webrtcBridge != nil {
		if err := r.webrtcBridge.Close(); err != nil {
			r.logger.Error("error closing bridge", "error", err)
		}
	}

	r.logger.Info("camera relay stopped",
		"duration", time.Since(r.startTime),
		"video_packets", r.videoPacketCount.Load(),
		"video_frames", r.videoFrameCount.Load())

	return nil
}

// readLoop reads RTP packets from RTSP connection
func (r *CameraRelay) readLoop() {
	defer r.wg.Done()

	r.logger.Info("starting packet read loop")

	if err := r.rtspConn.ReadPackets(r.ctx); err != nil && r.ctx.Err() == nil {
		r.logger.Error("RTSP read error", "error", err)

		// Notify about RTSP disconnect for recovery
		if r.OnRTSPDisconnect != nil {
			r.OnRTSPDisconnect(r.cameraID, err)
		}
	}

	r.logger.Info("packet read loop exited")
}

// statsLoop periodically logs relay statistics
func (r *CameraRelay) statsLoop() {
	defer r.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.logger.Info("relay statistics",
				"uptime", time.Since(r.startTime).Round(time.Second),
				"video_packets", r.videoPacketCount.Load(),
				"video_frames", r.videoFrameCount.Load(),
				"audio_packets", r.audioPacketCount.Load(),
				"audio_frames", r.audioFrameCount.Load(),
				"webrtc_state", r.webrtcBridge.GetConnectionState().String(),
			)
		}
	}
}

// monitorLoop monitors WebRTC connection state
func (r *CameraRelay) monitorLoop() {
	defer r.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	lastState := r.webrtcBridge.GetConnectionState()

	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			currentState := r.webrtcBridge.GetConnectionState()

			// Detect state changes
			if currentState != lastState {
				r.logger.Info("WebRTC state changed",
					"from", lastState.String(),
					"to", currentState.String())

				// Handle disconnections
				if currentState.String() == "failed" || currentState.String() == "disconnected" {
					r.logger.Error("WebRTC connection lost", "state", currentState.String())

					if r.OnWebRTCDisconnect != nil {
						r.OnWebRTCDisconnect(r.cameraID, fmt.Errorf("WebRTC state: %s", currentState.String()))
					}
				}

				lastState = currentState
			}
		}
	}
}

// GetStats returns current relay statistics
func (r *CameraRelay) GetStats() RelayStats {
	return RelayStats{
		CameraID:         r.cameraID,
		DeviceID:         r.deviceID,
		SessionID:        r.webrtcBridge.GetSessionID(),
		Uptime:           time.Since(r.startTime),
		VideoPackets:     r.videoPacketCount.Load(),
		VideoFrames:      r.videoFrameCount.Load(),
		AudioPackets:     r.audioPacketCount.Load(),
		AudioFrames:      r.audioFrameCount.Load(),
		WebRTCState:      r.webrtcBridge.GetConnectionState().String(),
		StreamExpiresAt:  r.stream.ExpiresAt,
	}
}

// RelayStats contains statistics for a single relay
type RelayStats struct {
	CameraID         string
	DeviceID         string
	SessionID        string
	Uptime           time.Duration
	VideoPackets     uint64
	VideoFrames      uint64
	AudioPackets     uint64
	AudioFrames      uint64
	WebRTCState      string
	StreamExpiresAt  time.Time
}
