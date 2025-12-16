package relay

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/nest"
)

// MultiCameraRelay orchestrates relays for multiple cameras with rate-limited coordination
type MultiCameraRelay struct {
	streamMgr  *nest.MultiStreamManager
	cfClient   *cloudflare.Client
	logger     *slog.Logger

	mu     sync.RWMutex
	relays map[string]*CameraRelay // Key: cameraID

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewMultiCameraRelay creates a multi-camera relay orchestrator
func NewMultiCameraRelay(
	streamMgr *nest.MultiStreamManager,
	cfClient *cloudflare.Client,
	logger *slog.Logger,
) *MultiCameraRelay {
	ctx, cancel := context.WithCancel(context.Background())

	return &MultiCameraRelay{
		streamMgr: streamMgr,
		cfClient:  cfClient,
		logger:    logger,
		relays:    make(map[string]*CameraRelay),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// Start initializes relays for all cameras managed by the stream manager
func (mcr *MultiCameraRelay) Start(ctx context.Context) error {
	mcr.logger.Info("starting multi-camera relay")

	// Start the stream manager first (handles stream generation/extension)
	if err := mcr.streamMgr.Start(); err != nil {
		return fmt.Errorf("start stream manager: %w", err)
	}

	// Start monitoring loop to create relays for active streams
	mcr.wg.Add(1)
	go mcr.monitorStreamsLoop()

	mcr.logger.Info("multi-camera relay started")
	return nil
}

// Stop gracefully stops all relays and the stream manager
func (mcr *MultiCameraRelay) Stop() error {
	mcr.logger.Info("stopping multi-camera relay")

	// Cancel context to stop monitoring loop
	mcr.cancel()

	// Stop all active relays
	mcr.mu.Lock()
	var stopWg sync.WaitGroup
	for cameraID, relay := range mcr.relays {
		stopWg.Add(1)
		go func(id string, r *CameraRelay) {
			defer stopWg.Done()
			if err := r.Stop(); err != nil {
				mcr.logger.Error("failed to stop relay", "camera_id", id, "error", err)
			}
		}(cameraID, relay)
	}
	mcr.mu.Unlock()

	// Wait for all relays to stop
	stopWg.Wait()

	// Wait for monitoring loop to exit
	mcr.wg.Wait()

	// Stop the stream manager last
	if err := mcr.streamMgr.Stop(); err != nil {
		mcr.logger.Error("failed to stop stream manager", "error", err)
		return err
	}

	mcr.logger.Info("multi-camera relay stopped")
	return nil
}

// monitorStreamsLoop periodically checks stream statuses and creates/removes relays
func (mcr *MultiCameraRelay) monitorStreamsLoop() {
	defer mcr.wg.Done()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	mcr.logger.Info("stream monitoring loop started")

	for {
		select {
		case <-mcr.ctx.Done():
			return
		case <-ticker.C:
			mcr.reconcileRelays()
		}
	}
}

// reconcileRelays ensures relays match the stream manager's active streams
func (mcr *MultiCameraRelay) reconcileRelays() {
	statuses := mcr.streamMgr.GetStreamStatus()

	// First pass: identify relays to create (without holding lock for long)
	var toCreate []struct {
		cameraID string
		deviceID string
	}

	mcr.mu.Lock()
	for _, status := range statuses {
		cameraID := status.CameraID

		// Skip if stream is not running
		if status.State != nest.StateRunning {
			// If relay exists, stop it
			if relay, exists := mcr.relays[cameraID]; exists {
				mcr.logger.Info("stream no longer running, stopping relay",
					"camera_id", cameraID,
					"state", status.State.String())

				go func(r *CameraRelay) {
					if err := r.Stop(); err != nil {
						mcr.logger.Error("failed to stop relay", "camera_id", cameraID, "error", err)
					}
				}(relay)

				delete(mcr.relays, cameraID)
			}
			continue
		}

		// If relay doesn't exist for running stream, mark for creation
		if _, exists := mcr.relays[cameraID]; !exists {
			toCreate = append(toCreate, struct {
				cameraID string
				deviceID string
			}{cameraID, status.DeviceID})
		}
	}

	// Remove relays for cameras no longer managed by stream manager
	for cameraID, relay := range mcr.relays {
		found := false
		for _, status := range statuses {
			if status.CameraID == cameraID {
				found = true
				break
			}
		}

		if !found {
			mcr.logger.Info("camera removed from stream manager, stopping relay", "camera_id", cameraID)

			go func(r *CameraRelay) {
				if err := r.Stop(); err != nil {
					mcr.logger.Error("failed to stop relay", "camera_id", cameraID, "error", err)
				}
			}(relay)

			delete(mcr.relays, cameraID)
		}
	}
	mcr.mu.Unlock()

	// Second pass: create relays (without holding lock - slow operation)
	for _, item := range toCreate {
		mcr.logger.Info("creating relay for running stream", "camera_id", item.cameraID)
		if err := mcr.createRelayForStream(item.cameraID, item.deviceID); err != nil {
			mcr.logger.Error("failed to create relay", "camera_id", item.cameraID, "error", err)
		}
	}
}

// createRelayForStream creates and starts a relay for a specific camera
func (mcr *MultiCameraRelay) createRelayForStream(cameraID, deviceID string) error {
	// Get stream from stream manager
	stream := mcr.streamMgr.GetStream(cameraID)
	if stream == nil {
		return fmt.Errorf("no stream found for camera %s", cameraID)
	}

	// Create relay
	relay := NewCameraRelay(
		cameraID,
		deviceID,
		stream,
		mcr.cfClient,
		mcr.logger.With("camera_id", cameraID),
	)

	// Setup error handlers
	relay.OnRTSPDisconnect = func(camID string, err error) {
		mcr.logger.Error("RTSP disconnect detected",
			"camera_id", camID,
			"error", err)
		// Stream manager will handle regeneration via its monitoring loop
	}

	relay.OnWebRTCDisconnect = func(camID string, err error) {
		mcr.logger.Error("WebRTC disconnect detected",
			"camera_id", camID,
			"error", err)

		// Recreate the relay (new Cloudflare session)
		mcr.mu.Lock()
		if existingRelay, exists := mcr.relays[camID]; exists {
			delete(mcr.relays, camID)
			mcr.mu.Unlock()

			// Stop old relay
			if err := existingRelay.Stop(); err != nil {
				mcr.logger.Error("failed to stop old relay", "camera_id", camID, "error", err)
			}

			// Recreate relay (will be done in next reconciliation loop)
		} else {
			mcr.mu.Unlock()
		}
	}

	// Start relay
	startCtx, cancel := context.WithTimeout(mcr.ctx, 30*time.Second)
	defer cancel()

	if err := relay.Start(startCtx); err != nil {
		return fmt.Errorf("start relay: %w", err)
	}

	// Store relay (acquire lock for map write)
	mcr.mu.Lock()
	mcr.relays[cameraID] = relay
	mcr.mu.Unlock()

	mcr.logger.Info("relay created and started", "camera_id", cameraID)
	return nil
}

// GetRelayStats returns statistics for all active relays
func (mcr *MultiCameraRelay) GetRelayStats() []RelayStats {
	mcr.mu.RLock()
	defer mcr.mu.RUnlock()

	stats := make([]RelayStats, 0, len(mcr.relays))
	for _, relay := range mcr.relays {
		stats = append(stats, relay.GetStats())
	}

	return stats
}

// GetAggregateStats returns aggregate statistics across all relays
func (mcr *MultiCameraRelay) GetAggregateStats() AggregateStats {
	mcr.mu.RLock()
	defer mcr.mu.RUnlock()

	agg := AggregateStats{
		TotalRelays: len(mcr.relays),
	}

	for _, relay := range mcr.relays {
		stats := relay.GetStats()
		agg.TotalVideoPackets += stats.VideoPackets
		agg.TotalVideoFrames += stats.VideoFrames
		agg.TotalAudioPackets += stats.AudioPackets
		agg.TotalAudioFrames += stats.AudioFrames

		// Count by WebRTC state
		switch stats.WebRTCState {
		case "connected":
			agg.ConnectedRelays++
		case "connecting":
			agg.ConnectingRelays++
		case "failed":
			agg.FailedRelays++
		case "disconnected":
			agg.DisconnectedRelays++
		}
	}

	return agg
}

// AggregateStats contains aggregate statistics across all relays
type AggregateStats struct {
	TotalRelays         int
	ConnectedRelays     int
	ConnectingRelays    int
	FailedRelays        int
	DisconnectedRelays  int
	TotalVideoPackets   uint64
	TotalVideoFrames    uint64
	TotalAudioPackets   uint64
	TotalAudioFrames    uint64
}
