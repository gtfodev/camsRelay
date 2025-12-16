package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/api"
	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/config"
	"github.com/ethan/nest-cloudflare-relay/pkg/nest"
	"github.com/ethan/nest-cloudflare-relay/pkg/relay"
)

// Multi-camera relay example: Full pipeline for multiple cameras
// Nest cameras → RTSP streams → RTP processing → WebRTC → Cloudflare
func main() {
	// Initialize logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("starting multi-camera Nest → Cloudflare relay")

	// Load credentials from .env file
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create Nest API client
	nestClient := nest.NewClient(
		cfg.Google.ClientID,
		cfg.Google.ClientSecret,
		cfg.Google.RefreshToken,
		logger.With("component", "nest"),
	)

	// Create Cloudflare client
	cfClient := cloudflare.NewClient(
		cfg.Cloudflare.AppID,
		cfg.Cloudflare.APIToken,
		logger.With("component", "cloudflare"),
	)

	// List available cameras
	ctx := context.Background()
	devices, err := nestClient.ListDevices(ctx, cfg.Google.ProjectID)
	if err != nil {
		log.Fatalf("Failed to list devices: %v", err)
	}

	logger.Info("discovered cameras", "count", len(devices))

	// Extract camera IDs (limit to first 20 for rate limiting)
	cameraIDs := make([]string, 0, 20)
	cameraNames := make(map[string]string) // Map device ID to display name
	for i, device := range devices {
		if i >= 20 {
			break
		}
		cameraIDs = append(cameraIDs, device.DeviceID)

		displayName := device.Traits.Info.CustomName
		if displayName == "" && len(device.Relations) > 0 {
			displayName = device.Relations[0].DisplayName
		}
		if displayName == "" {
			displayName = device.DeviceID
		}
		cameraNames[device.DeviceID] = displayName

		logger.Info("camera available",
			"index", i+1,
			"device_id", device.DeviceID,
			"name", displayName,
			"protocols", device.Traits.CameraLiveStream.SupportedProtocols,
			"video_codecs", device.Traits.CameraLiveStream.VideoCodecs,
			"audio_codecs", device.Traits.CameraLiveStream.AudioCodecs,
		)
	}

	if len(cameraIDs) == 0 {
		log.Fatal("No cameras found")
	}

	// Configure multi-stream manager with defaults for 20 cameras @ 10 QPM
	msmConfig := nest.DefaultMultiStreamConfig()

	// Create multi-stream manager
	streamMgr := nest.NewMultiStreamManager(
		nestClient,
		cfg.Google.ProjectID,
		msmConfig,
		logger.With("component", "stream_manager"),
	)

	// Create multi-camera relay orchestrator
	multiRelay := relay.NewMultiCameraRelay(
		streamMgr,
		cfClient,
		logger.With("component", "multi_relay"),
	)

	logger.Info("multi-camera relay initialized",
		"cameras", len(cameraIDs),
		"qpm_limit", msmConfig.QPM,
		"stagger_interval", msmConfig.StaggerInterval)

	// Create and start HTTP API server for viewer FIRST (before camera init)
	apiServer := api.NewServer(
		multiRelay,
		cfClient,
		cfg.Cloudflare.AppID,
		logger.With("component", "api"),
	)

	// Set camera display names in the API server
	for deviceID, name := range cameraNames {
		apiServer.SetCameraName(deviceID, name)
	}

	// Start HTTP server before cameras so viewer is available immediately
	if err := apiServer.Start(ctx, ":8080"); err != nil {
		log.Fatalf("Failed to start API server: %v", err)
	}
	logger.Info("API server started", "address", "http://localhost:8080")

	// Start the multi-relay (starts stream manager internally)
	if err := multiRelay.Start(ctx); err != nil {
		log.Fatalf("Failed to start multi-relay: %v", err)
	}

	// Start all cameras with staggered initialization
	// This will take ~4 minutes for 20 cameras (20 * 12s stagger)
	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer startCancel()

	logger.Info("starting cameras with staggered initialization")
	if err := streamMgr.StartCameras(startCtx, cameraIDs); err != nil {
		log.Fatalf("Failed to start cameras: %v", err)
	}

	logger.Info("all cameras initialization triggered - relays will be created as streams become ready")

	// Start monitoring goroutine
	go monitorStatus(multiRelay, streamMgr, logger)

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	logger.Info("running... press Ctrl+C to stop")
	<-sigChan

	logger.Info("shutdown signal received, stopping all relays")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Stop API server
	if err := apiServer.Stop(shutdownCtx); err != nil {
		logger.Error("error stopping API server", "error", err)
	}

	// Stop relay
	if err := multiRelay.Stop(); err != nil {
		logger.Error("error during shutdown", "error", err)
	}

	logger.Info("shutdown complete")
}

// monitorStatus periodically logs stream and relay status
func monitorStatus(multiRelay *relay.MultiCameraRelay, streamMgr *nest.MultiStreamManager, logger *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Get stream statuses
		streamStatuses := streamMgr.GetStreamStatus()

		// Count streams by state
		streamStates := make(map[nest.CameraState]int)
		for _, status := range streamStatuses {
			streamStates[status.State]++
		}

		// Get relay stats
		relayStats := multiRelay.GetRelayStats()
		aggStats := multiRelay.GetAggregateStats()

		// Get queue stats
		queueStats := streamMgr.GetQueueStats()

		logger.Info("status report",
			// Stream states
			"total_cameras", len(streamStatuses),
			"streams_starting", streamStates[nest.StateStarting],
			"streams_running", streamStates[nest.StateRunning],
			"streams_failed", streamStates[nest.StateFailed],
			"streams_degraded", streamStates[nest.StateDegraded],
			"streams_stopped", streamStates[nest.StateStopped],
			// Relay states
			"total_relays", aggStats.TotalRelays,
			"relays_connected", aggStats.ConnectedRelays,
			"relays_connecting", aggStats.ConnectingRelays,
			"relays_failed", aggStats.FailedRelays,
			"relays_disconnected", aggStats.DisconnectedRelays,
			// Aggregate statistics
			"total_video_packets", aggStats.TotalVideoPackets,
			"total_video_frames", aggStats.TotalVideoFrames,
			"total_audio_packets", aggStats.TotalAudioPackets,
			"total_audio_frames", aggStats.TotalAudioFrames,
			// Queue statistics
			"queue_depth", queueStats.QueueDepth,
			"total_executed", queueStats.TotalExecuted,
			"total_failed", queueStats.TotalFailed,
			"extend_count", queueStats.ExtendCount,
			"generate_count", queueStats.GenerateCount,
			"avg_wait_time_ms", queueStats.AvgWaitTime.Milliseconds(),
		)

		// Log individual camera issues
		for _, streamStatus := range streamStatuses {
			if streamStatus.State == nest.StateFailed || streamStatus.State == nest.StateDegraded {
				logger.Warn("camera stream issue",
					"camera_id", streamStatus.CameraID,
					"state", streamStatus.State.String(),
					"failure_count", streamStatus.FailureCount,
					"last_error", streamStatus.LastError,
					"time_since_attempt", time.Since(streamStatus.LastAttempt),
				)
			}
		}

		// Log individual relay statistics
		for _, stat := range relayStats {
			if stat.WebRTCState != "connected" {
				logger.Warn("relay connection issue",
					"camera_id", stat.CameraID,
					"session_id", stat.SessionID,
					"webrtc_state", stat.WebRTCState,
					"uptime", stat.Uptime,
				)
			}
		}
	}
}

// Architecture Notes:
//
// COMPONENT HIERARCHY:
// MultiCameraRelay (orchestrator)
//   ├─ MultiStreamManager (Nest stream lifecycle)
//   │   ├─ CommandQueue (10 QPM rate limiting)
//   │   └─ StreamManager per camera (auto-extension)
//   │
//   ├─ CameraRelay per camera (media pipeline)
//   │   ├─ RTSP client (TCP interleaved)
//   │   ├─ RTP processors (H.264, AAC)
//   │   └─ WebRTC bridge (Cloudflare - producer)
//   │
//   └─ API Server (HTTP endpoints + web viewer)
//       ├─ GET /api/cameras (session discovery)
//       ├─ GET /api/config (Cloudflare app ID)
//       └─ Viewer (browser) → Cloudflare (consumer)
//
// LIFECYCLE:
// 1. MultiStreamManager starts cameras with 12s stagger
// 2. Each stream → StateStarting → StateRunning
// 3. MultiCameraRelay detects StateRunning → creates CameraRelay
// 4. CameraRelay connects RTSP → processes RTP → sends to Cloudflare
// 5. MultiStreamManager auto-extends streams every 180s
// 6. If RTSP disconnect → MultiStreamManager regenerates stream
// 7. If WebRTC disconnect → MultiCameraRelay recreates relay
//
// RATE LIMITING:
// - 20 cameras × 1 generate = 20 queries (staggered over 4 minutes = 5 QPM)
// - 20 cameras ÷ 4 minutes = 5 extensions per minute (steady state)
// - Total: ~10 QPM (at Google's limit)
// - Priority queue: extensions (HIGH) > generates (LOW)
//
// ERROR RECOVERY:
// - Stream expired → regenerate (exponential backoff, max 5 retries)
// - Too many failures → degraded state (5 minute retry interval)
// - RTSP disconnect → stream manager handles regeneration
// - WebRTC disconnect → relay recreates Cloudflare session
//
// GRACEFUL SHUTDOWN:
// 1. Stop MultiCameraRelay (stops all relays + stream manager)
// 2. Each CameraRelay: cancel context → close RTSP → wait goroutines → close WebRTC
// 3. MultiStreamManager: stop all stream managers → stop queue
// 4. Clean exit with no goroutine leaks
