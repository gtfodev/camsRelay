package nest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// StreamManager manages RTSP stream lifecycle and automatic extension
type StreamManager struct {
	client *Client
	stream *RTSPStream
	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Extension configuration
	extensionInterval time.Duration // Time before expiry to extend
}

// NewStreamManager creates a new stream manager
func NewStreamManager(client *Client, stream *RTSPStream, logger *slog.Logger) *StreamManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &StreamManager{
		client:            client,
		stream:            stream,
		logger:            logger,
		ctx:               ctx,
		cancel:            cancel,
		extensionInterval: 60 * time.Second, // Extend 60 seconds before expiry
	}
}

// Start begins the automatic extension loop
func (m *StreamManager) Start() {
	m.wg.Add(1)
	go m.extensionLoop()

	m.logger.Info("stream manager started",
		"device_id", m.stream.DeviceID,
		"expires_at", m.stream.ExpiresAt.Format(time.RFC3339))
}

// Stop stops the extension loop and waits for cleanup
func (m *StreamManager) Stop(ctx context.Context) error {
	m.logger.Info("stopping stream manager", "device_id", m.stream.DeviceID)

	m.cancel()
	m.wg.Wait()

	// Stop the RTSP stream
	if err := m.client.StopRTSPStream(ctx, m.stream); err != nil {
		m.logger.Error("failed to stop RTSP stream", "error", err)
		return fmt.Errorf("stop RTSP stream: %w", err)
	}

	m.logger.Info("stream manager stopped", "device_id", m.stream.DeviceID)
	return nil
}

// extensionLoop runs the automatic stream extension timer
func (m *StreamManager) extensionLoop() {
	defer m.wg.Done()

	for {
		// Calculate time until next extension
		now := time.Now()
		expiresAt := m.stream.ExpiresAt
		timeUntilExpiry := expiresAt.Sub(now)

		// Extend when we're within the extension interval of expiry
		timeUntilExtension := timeUntilExpiry - m.extensionInterval

		// Ensure we don't have a negative or zero duration
		if timeUntilExtension < 1*time.Second {
			timeUntilExtension = 1 * time.Second
		}

		m.logger.Debug("scheduling next extension",
			"device_id", m.stream.DeviceID,
			"time_until_extension", timeUntilExtension.String(),
			"current_expiry", expiresAt.Format(time.RFC3339))

		select {
		case <-m.ctx.Done():
			return

		case <-time.After(timeUntilExtension):
			// Time to extend the stream
			if err := m.extendWithRetry(); err != nil {
				m.logger.Error("failed to extend stream after retries",
					"device_id", m.stream.DeviceID,
					"error", err)
				// Continue trying - don't exit the loop
			}
		}
	}
}

// extendWithRetry attempts to extend the stream with exponential backoff
func (m *StreamManager) extendWithRetry() error {
	const maxRetries = 3
	backoff := 1 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Create context with timeout for this extension attempt
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)

		err := m.client.ExtendRTSPStream(ctx, m.stream)
		cancel()

		if err == nil {
			m.logger.Info("stream extended successfully",
				"device_id", m.stream.DeviceID,
				"new_expiry", m.stream.ExpiresAt.Format(time.RFC3339),
				"attempt", attempt+1)
			return nil
		}

		m.logger.Warn("stream extension attempt failed",
			"device_id", m.stream.DeviceID,
			"attempt", attempt+1,
			"max_retries", maxRetries,
			"error", err)

		// If this isn't the last attempt, wait before retrying
		if attempt < maxRetries-1 {
			select {
			case <-m.ctx.Done():
				return m.ctx.Err()
			case <-time.After(backoff):
				backoff *= 2 // Exponential backoff
			}
		}
	}

	return fmt.Errorf("max retries exceeded for stream extension")
}

// GetStream returns the current stream
func (m *StreamManager) GetStream() *RTSPStream {
	return m.stream
}

// GetExpiresAt returns when the stream will expire
func (m *StreamManager) GetExpiresAt() time.Time {
	return m.stream.ExpiresAt
}

// GetTimeUntilExpiry returns how long until the stream expires
func (m *StreamManager) GetTimeUntilExpiry() time.Duration {
	return time.Until(m.stream.ExpiresAt)
}
