package nest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// CameraState represents the lifecycle state of a camera stream
type CameraState int

const (
	StateStarting CameraState = iota // Initial startup in progress
	StateRunning                      // Stream active and healthy
	StateFailed                       // Stream failed, attempting recovery
	StateDegraded                     // Too many failures, reduced retry frequency
	StateStopped                      // Intentionally stopped
)

// String returns human-readable state
func (s CameraState) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	case StateDegraded:
		return "degraded"
	case StateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// CameraStream tracks a single camera's stream lifecycle
type CameraStream struct {
	CameraID       string
	DeviceID       string
	State          CameraState
	Manager        *StreamManager
	FailureCount   int
	LastError      error
	LastAttempt    time.Time
	CreatedAt      time.Time
	LastExtension  time.Time
	StreamExpiry   time.Time
	RecoveryBackoff time.Duration
}

// MultiStreamManager orchestrates multiple camera streams with rate-limited coordination
type MultiStreamManager struct {
	client       *Client
	projectID    string
	queue        *CommandQueue
	logger       *slog.Logger

	mu      sync.RWMutex
	streams map[string]*CameraStream // Key: cameraID

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Configuration
	staggerInterval   time.Duration // Delay between camera startups
	maxFailures       int           // Failures before degraded state
	degradedRetry     time.Duration // Retry interval for degraded cameras
	recoveryBaseDelay time.Duration // Base delay for exponential backoff
}

// MultiStreamConfig configures the multi-stream manager
type MultiStreamConfig struct {
	QPM               float64       // Queries per minute limit (default: 10)
	StaggerInterval   time.Duration // Delay between camera startups (default: 12s)
	MaxFailures       int           // Failures before degraded (default: 5)
	DegradedRetry     time.Duration // Retry interval when degraded (default: 5min)
	RecoveryBaseDelay time.Duration // Base delay for backoff (default: 10s)
}

// DefaultMultiStreamConfig returns sensible defaults for 20 cameras at 10 QPM
func DefaultMultiStreamConfig() MultiStreamConfig {
	return MultiStreamConfig{
		QPM:               10.0,               // Google's limit
		StaggerInterval:   12 * time.Second,   // 20 cameras * 12s = 4 minutes
		MaxFailures:       5,                  // Degrade after 5 consecutive failures
		DegradedRetry:     5 * time.Minute,    // Check degraded cameras every 5 minutes
		RecoveryBaseDelay: 10 * time.Second,   // Start backoff at 10s
	}
}

// NewMultiStreamManager creates a manager for multiple camera streams
func NewMultiStreamManager(client *Client, projectID string, config MultiStreamConfig, logger *slog.Logger) *MultiStreamManager {
	ctx, cancel := context.WithCancel(context.Background())

	queue := NewCommandQueue(config.QPM, logger.With("component", "queue"))

	msm := &MultiStreamManager{
		client:            client,
		projectID:         projectID,
		queue:             queue,
		logger:            logger,
		streams:           make(map[string]*CameraStream),
		ctx:               ctx,
		cancel:            cancel,
		staggerInterval:   config.StaggerInterval,
		maxFailures:       config.MaxFailures,
		degradedRetry:     config.DegradedRetry,
		recoveryBaseDelay: config.RecoveryBaseDelay,
	}

	logger.Info("multi-stream manager created",
		"project_id", projectID,
		"qpm", config.QPM,
		"stagger_interval", config.StaggerInterval,
		"max_failures", config.MaxFailures)

	return msm
}

// Start begins the multi-stream manager and command queue
func (msm *MultiStreamManager) Start() error {
	msm.queue.Start()
	msm.logger.Info("multi-stream manager started")
	return nil
}

// Stop gracefully stops all streams and the command queue
func (msm *MultiStreamManager) Stop() error {
	msm.logger.Info("stopping multi-stream manager")

	msm.cancel()

	// Stop all stream managers
	msm.mu.Lock()
	var stopWg sync.WaitGroup
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()

	for cameraID, stream := range msm.streams {
		if stream.Manager != nil {
			stopWg.Add(1)
			go func(id string, mgr *StreamManager) {
				defer stopWg.Done()
				if err := mgr.Stop(stopCtx); err != nil {
					msm.logger.Error("failed to stop stream manager", "camera_id", id, "error", err)
				}
			}(cameraID, stream.Manager)
		}
		stream.State = StateStopped
	}
	msm.mu.Unlock()

	// Wait for all streams to stop
	stopWg.Wait()

	// Wait for any ongoing operations
	msm.wg.Wait()

	// Stop the command queue
	if err := msm.queue.Stop(); err != nil {
		msm.logger.Error("failed to stop command queue", "error", err)
	}

	msm.logger.Info("multi-stream manager stopped")
	return nil
}

// StartCameras initiates streaming for multiple cameras with staggered startup
func (msm *MultiStreamManager) StartCameras(ctx context.Context, cameraIDs []string) error {
	msm.logger.Info("starting cameras with staggered initialization",
		"count", len(cameraIDs),
		"stagger_interval", msm.staggerInterval)

	for i, cameraID := range cameraIDs {
		// Check context before starting each camera
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Initialize camera stream tracking
		msm.mu.Lock()
		msm.streams[cameraID] = &CameraStream{
			CameraID:  cameraID,
			DeviceID:  extractCameraDeviceID(cameraID),
			State:     StateStarting,
			CreatedAt: time.Now(),
		}
		msm.mu.Unlock()

		// Start stream asynchronously
		msm.wg.Add(1)
		go msm.startCameraStream(cameraID)

		// Stagger startup (except for last camera)
		if i < len(cameraIDs)-1 {
			msm.logger.Debug("waiting before next camera startup",
				"current", i+1,
				"total", len(cameraIDs),
				"wait", msm.staggerInterval)

			select {
			case <-time.After(msm.staggerInterval):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	msm.logger.Info("all cameras initialization triggered", "count", len(cameraIDs))
	return nil
}

// startCameraStream initializes and manages a single camera stream lifecycle
func (msm *MultiStreamManager) startCameraStream(cameraID string) {
	defer msm.wg.Done()

	logger := msm.logger.With("camera_id", cameraID)
	logger.Info("starting camera stream")

	// Generate initial stream via command queue (LOW priority)
	err := msm.queue.SubmitGenerate(cameraID, 0, func() error {
		return msm.generateStream(cameraID)
	})

	if err != nil {
		msm.updateStreamState(cameraID, func(cs *CameraStream) {
			cs.State = StateFailed
			cs.FailureCount = 1
			cs.LastError = err
			cs.LastAttempt = time.Now()
		})
		logger.Error("initial stream generation failed", "error", err)

		// Start recovery loop
		msm.wg.Add(1)
		go msm.recoveryLoop(cameraID)
		return
	}

	// Stream generated successfully, start extension loop
	msm.updateStreamState(cameraID, func(cs *CameraStream) {
		cs.State = StateRunning
		cs.FailureCount = 0
		cs.LastError = nil
		cs.LastExtension = time.Now()
	})

	logger.Info("camera stream started successfully")

	// Monitor stream health
	msm.wg.Add(1)
	go msm.monitorStream(cameraID)
}

// generateStream creates a new RTSP stream for a camera
func (msm *MultiStreamManager) generateStream(cameraID string) error {
	ctx, cancel := context.WithTimeout(msm.ctx, 30*time.Second)
	defer cancel()

	deviceID := extractCameraDeviceID(cameraID)
	stream, err := msm.client.GenerateRTSPStream(ctx, msm.projectID, deviceID)
	if err != nil {
		return fmt.Errorf("generate RTSP stream: %w", err)
	}

	// Create stream manager
	manager := NewStreamManager(msm.client, stream,
		msm.logger.With("camera_id", cameraID, "component", "stream_manager"))

	msm.updateStreamState(cameraID, func(cs *CameraStream) {
		cs.Manager = manager
		cs.StreamExpiry = stream.ExpiresAt
	})

	// Start manager (will handle extensions via queue integration)
	manager.Start()

	return nil
}

// monitorStream watches for stream extension needs and failures
func (msm *MultiStreamManager) monitorStream(cameraID string) {
	defer msm.wg.Done()

	logger := msm.logger.With("camera_id", cameraID)
	ticker := time.NewTicker(30 * time.Second) // Check every 30s
	defer ticker.Stop()

	for {
		select {
		case <-msm.ctx.Done():
			return

		case <-ticker.C:
			msm.mu.RLock()
			stream, exists := msm.streams[cameraID]
			msm.mu.RUnlock()

			if !exists || stream.Manager == nil {
				logger.Warn("stream no longer exists, stopping monitor")
				return
			}

			// Check if stream needs extension
			timeUntilExpiry := stream.Manager.GetTimeUntilExpiry()
			if timeUntilExpiry < 90*time.Second {
				// Time to extend via queue (HIGH priority)
				logger.Debug("submitting extension command", "time_until_expiry", timeUntilExpiry)

				err := msm.queue.SubmitExtend(cameraID, func() error {
					return msm.extendStream(cameraID)
				})

				if err != nil {
					logger.Error("extension command failed", "error", err)
					msm.handleExtensionFailure(cameraID, err)
				} else {
					msm.updateStreamState(cameraID, func(cs *CameraStream) {
						cs.LastExtension = time.Now()
						cs.FailureCount = 0 // Reset on success
						cs.StreamExpiry = cs.Manager.GetExpiresAt()
					})
				}
			}
		}
	}
}

// extendStream extends an existing RTSP stream
func (msm *MultiStreamManager) extendStream(cameraID string) error {
	ctx, cancel := context.WithTimeout(msm.ctx, 30*time.Second)
	defer cancel()

	msm.mu.RLock()
	stream, exists := msm.streams[cameraID]
	msm.mu.RUnlock()

	if !exists || stream.Manager == nil {
		return errors.New("stream manager not found")
	}

	return msm.client.ExtendRTSPStream(ctx, stream.Manager.GetStream())
}

// handleExtensionFailure processes extension failures and triggers recovery
func (msm *MultiStreamManager) handleExtensionFailure(cameraID string, err error) {
	msm.updateStreamState(cameraID, func(cs *CameraStream) {
		cs.FailureCount++
		cs.LastError = err
		cs.LastAttempt = time.Now()

		// Check for 404 / stream expired - need to regenerate
		if isStreamExpiredError(err) {
			cs.State = StateFailed
			msm.logger.Warn("stream expired, marking for regeneration",
				"camera_id", cameraID,
				"failure_count", cs.FailureCount)
		}

		// Too many failures - mark as degraded
		if cs.FailureCount >= msm.maxFailures {
			cs.State = StateDegraded
			cs.RecoveryBackoff = msm.degradedRetry
			msm.logger.Error("camera marked as degraded",
				"camera_id", cameraID,
				"failure_count", cs.FailureCount,
				"retry_interval", cs.RecoveryBackoff)
		}
	})

	// Start recovery loop if needed
	msm.wg.Add(1)
	go msm.recoveryLoop(cameraID)
}

// recoveryLoop attempts to recover failed/degraded streams
func (msm *MultiStreamManager) recoveryLoop(cameraID string) {
	defer msm.wg.Done()

	logger := msm.logger.With("camera_id", cameraID)

	for {
		msm.mu.RLock()
		stream, exists := msm.streams[cameraID]
		msm.mu.RUnlock()

		if !exists {
			logger.Warn("stream removed, stopping recovery")
			return
		}

		// Stop if no longer failed/degraded
		if stream.State != StateFailed && stream.State != StateDegraded {
			return
		}

		// Calculate backoff delay
		var delay time.Duration
		if stream.State == StateDegraded {
			delay = msm.degradedRetry
		} else {
			// Exponential backoff: baseDelay * 2^attempt (capped at 5 minutes)
			delay = msm.recoveryBaseDelay * time.Duration(1<<uint(stream.FailureCount))
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
		}

		logger.Info("scheduling recovery attempt",
			"state", stream.State.String(),
			"failure_count", stream.FailureCount,
			"delay", delay)

		select {
		case <-msm.ctx.Done():
			return
		case <-time.After(delay):
		}

		// Attempt recovery via queue (LOW priority for regeneration)
		attempt := stream.FailureCount
		err := msm.queue.SubmitGenerate(cameraID, attempt, func() error {
			// Clean up old manager if exists
			msm.mu.Lock()
			if stream.Manager != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = stream.Manager.Stop(ctx)
				cancel()
			}
			msm.mu.Unlock()

			return msm.generateStream(cameraID)
		})

		if err == nil {
			logger.Info("stream recovery successful", "attempt", attempt)
			msm.updateStreamState(cameraID, func(cs *CameraStream) {
				cs.State = StateRunning
				cs.FailureCount = 0
				cs.LastError = nil
			})

			// Restart monitoring
			msm.wg.Add(1)
			go msm.monitorStream(cameraID)
			return
		}

		logger.Error("recovery attempt failed",
			"attempt", attempt,
			"error", err)

		msm.updateStreamState(cameraID, func(cs *CameraStream) {
			cs.FailureCount++
			cs.LastError = err
			cs.LastAttempt = time.Now()

			if cs.FailureCount >= msm.maxFailures {
				cs.State = StateDegraded
			}
		})

		// Continue loop for next retry
	}
}

// GetStreamStatus returns the current status of all streams
func (msm *MultiStreamManager) GetStreamStatus() []StreamStatus {
	msm.mu.RLock()
	defer msm.mu.RUnlock()

	statuses := make([]StreamStatus, 0, len(msm.streams))
	for _, stream := range msm.streams {
		status := StreamStatus{
			CameraID:       stream.CameraID,
			DeviceID:       stream.DeviceID,
			State:          stream.State,
			FailureCount:   stream.FailureCount,
			LastError:      stream.LastError,
			LastAttempt:    stream.LastAttempt,
			CreatedAt:      stream.CreatedAt,
			LastExtension:  stream.LastExtension,
		}

		if stream.Manager != nil {
			status.StreamExpiry = stream.Manager.GetExpiresAt()
			status.TimeUntilExpiry = stream.Manager.GetTimeUntilExpiry()
		}

		statuses = append(statuses, status)
	}

	return statuses
}

// StreamStatus contains current state of a camera stream
type StreamStatus struct {
	CameraID        string
	DeviceID        string
	State           CameraState
	FailureCount    int
	LastError       error
	LastAttempt     time.Time
	CreatedAt       time.Time
	LastExtension   time.Time
	StreamExpiry    time.Time
	TimeUntilExpiry time.Duration
}

// GetQueueStats returns command queue statistics
func (msm *MultiStreamManager) GetQueueStats() QueueStats {
	return msm.queue.GetStats()
}

// updateStreamState safely updates stream state with a mutation function
func (msm *MultiStreamManager) updateStreamState(cameraID string, fn func(*CameraStream)) {
	msm.mu.Lock()
	defer msm.mu.Unlock()

	if stream, exists := msm.streams[cameraID]; exists {
		fn(stream)
	}
}

// extractCameraDeviceID extracts device ID from camera ID
// Format: enterprises/{project}/devices/{deviceId}
func extractCameraDeviceID(cameraID string) string {
	// If already just the device ID, return as-is
	if len(cameraID) < 30 {
		return cameraID
	}
	// Otherwise extract from full name
	return extractDeviceID(cameraID)
}

// isStreamExpiredError checks if error indicates stream expiration (404)
func isStreamExpiredError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return contains(errStr, "404") || contains(errStr, "not found") || contains(errStr, "expired")
}

// contains checks if a string contains a substring (case-insensitive helper)
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		(len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
		findInString(s, substr))))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
