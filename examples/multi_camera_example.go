package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/nest"
)

// Example demonstrating Phase 3: Rate-limited multi-camera coordination for 20 Nest cameras
func main() {
	// Initialize logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Load credentials from environment
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	refreshToken := os.Getenv("GOOGLE_REFRESH_TOKEN")
	projectID := os.Getenv("GOOGLE_PROJECT_ID")

	if clientID == "" || clientSecret == "" || refreshToken == "" || projectID == "" {
		log.Fatal("Missing required environment variables")
	}

	// Create Nest API client
	client := nest.NewClient(clientID, clientSecret, refreshToken, logger)

	// List available cameras
	ctx := context.Background()
	devices, err := client.ListDevices(ctx, projectID)
	if err != nil {
		log.Fatalf("Failed to list devices: %v", err)
	}

	logger.Info("discovered cameras", "count", len(devices))

	// Extract camera IDs (limit to 20 for this example)
	cameraIDs := make([]string, 0, 20)
	for i, device := range devices {
		if i >= 20 {
			break
		}
		cameraIDs = append(cameraIDs, device.DeviceID)
		logger.Info("camera available",
			"index", i+1,
			"device_id", device.DeviceID,
			"name", device.Traits.Info.CustomName)
	}

	if len(cameraIDs) == 0 {
		log.Fatal("No cameras found")
	}

	// Configure multi-stream manager with defaults for 20 cameras @ 10 QPM
	config := nest.DefaultMultiStreamConfig()

	// Create multi-stream manager
	manager := nest.NewMultiStreamManager(client, projectID, config, logger)

	// Start command queue
	if err := manager.Start(); err != nil {
		log.Fatalf("Failed to start manager: %v", err)
	}

	logger.Info("multi-stream manager started",
		"cameras", len(cameraIDs),
		"qpm_limit", config.QPM,
		"stagger_interval", config.StaggerInterval)

	// Start all cameras with staggered initialization
	// This will take ~4 minutes for 20 cameras (20 * 12s stagger)
	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer startCancel()

	if err := manager.StartCameras(startCtx, cameraIDs); err != nil {
		log.Fatalf("Failed to start cameras: %v", err)
	}

	logger.Info("all cameras initialization triggered")

	// Start status monitoring goroutine
	go monitorStatus(manager, logger)

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	logger.Info("running... press Ctrl+C to stop")
	<-sigChan

	logger.Info("shutdown signal received, stopping all streams")

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := manager.Stop(); err != nil {
		logger.Error("error during shutdown", "error", err)
	}

	// Wait for shutdown to complete
	<-shutdownCtx.Done()
	logger.Info("shutdown complete")
}

// monitorStatus periodically logs stream and queue status
func monitorStatus(manager *nest.MultiStreamManager, logger *slog.Logger) {
	ticker := time.NewTicker(60 * time.Second) // Report every minute
	defer ticker.Stop()

	for range ticker.C {
		// Get stream statuses
		statuses := manager.GetStreamStatus()

		// Count by state
		stateCounts := make(map[nest.CameraState]int)
		for _, status := range statuses {
			stateCounts[status.State]++
		}

		// Get queue stats
		queueStats := manager.GetQueueStats()

		logger.Info("status report",
			"total_cameras", len(statuses),
			"state_starting", stateCounts[nest.StateStarting],
			"state_running", stateCounts[nest.StateRunning],
			"state_failed", stateCounts[nest.StateFailed],
			"state_degraded", stateCounts[nest.StateDegraded],
			"state_stopped", stateCounts[nest.StateStopped],
			"queue_depth", queueStats.QueueDepth,
			"total_executed", queueStats.TotalExecuted,
			"total_failed", queueStats.TotalFailed,
			"extend_count", queueStats.ExtendCount,
			"generate_count", queueStats.GenerateCount,
			"avg_wait_time_ms", queueStats.AvgWaitTime.Milliseconds(),
		)

		// Log individual camera issues
		for _, status := range statuses {
			if status.State == nest.StateFailed || status.State == nest.StateDegraded {
				logger.Warn("camera issue detected",
					"camera_id", status.CameraID,
					"state", status.State.String(),
					"failure_count", status.FailureCount,
					"last_error", status.LastError,
					"time_since_attempt", time.Since(status.LastAttempt),
				)
			} else if status.State == nest.StateRunning {
				logger.Debug("camera healthy",
					"camera_id", status.CameraID,
					"time_until_expiry", status.TimeUntilExpiry,
					"last_extension", time.Since(status.LastExtension),
				)
			}
		}
	}
}

// QPM Budget Analysis for 20 Cameras:
//
// STEADY STATE (all cameras running):
// - Each camera extends every ~4 minutes (240s stream lifetime, 60s buffer)
// - 20 cameras ÷ 4 minutes = 5 QPM for extensions
// - Reserve ~4 QPM for recovery operations
// - 1 QPM safety margin
// - Total: 10 QPM (at limit)
//
// STARTUP:
// - Staggered initialization: 12 seconds between cameras
// - 20 cameras × 12s = 240 seconds (4 minutes total)
// - Spread across 4 minutes = 5 QPM during startup
// - Well within 10 QPM limit
//
// RECOVERY SCENARIOS:
// - Failed streams marked degraded after 5 failures
// - Degraded cameras retry every 5 minutes
// - Priority queue ensures extensions always take precedence
// - "Save the Living Before Resurrecting the Dead"
//
// Example Timeline (3 cameras):
// T=0s:    Camera1 generates (queue: 1, executed: 0)
// T=12s:   Camera2 generates (queue: 1, executed: 1)
// T=24s:   Camera3 generates (queue: 1, executed: 2)
// T=240s:  Camera1 extends   (queue: 1, executed: 3) [HIGH priority]
// T=252s:  Camera2 extends   (queue: 1, executed: 4) [HIGH priority]
// T=264s:  Camera3 extends   (queue: 1, executed: 5) [HIGH priority]
// T=480s:  Camera1 extends   (queue: 1, executed: 6) [HIGH priority]
// ...continues indefinitely
//
// If Camera2 fails to extend at T=252s:
// - Marked as failed, retry scheduled with backoff
// - Recovery attempt uses LOW priority (CmdGenerate)
// - Camera1 and Camera3 extensions still get HIGH priority
// - After 5 failures, Camera2 marked degraded (5min retry interval)
