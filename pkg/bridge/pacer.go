package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/rtp"
)

const (
	// Video RTP clock rate (H.264 standard)
	videoClockRate = 90000 // 90kHz

	// Audio RTP clock rate (Opus standard)
	audioClockRate = 48000 // 48kHz

	// Catch-up speed multiplier when draining accumulated packets
	// 1.1x speed allows gradual catch-up without jarring viewer
	catchupSpeedMultiplier = 1.1

	// Threshold for entering catch-up mode (number of queued packets)
	catchupThreshold = 5

	// Maximum delay to wait before sending a packet
	// Prevents infinite delays on timestamp errors
	maxPacketDelay = 200 * time.Millisecond
)

// PacedPacket wraps an RTP packet with metadata for pacing
type PacedPacket struct {
	Packet       *rtp.Packet
	Timestamp    uint32 // RTP timestamp (not wall clock)
	IsKeyframe   bool
	NALUs        []byte // For video: pre-packetized H.264 data
	TrackType    string // "video" or "audio"
	ReceivedAt   time.Time
	SourceSeqNum uint16 // Original sequence number from source (for diagnostics)
}

// Pacer implements a leaky bucket algorithm to smooth RTP packet transmission
// Absorbs TCP bursts and drains at nominal frame rate based on RTP timestamps
type Pacer struct {
	logger       *slog.Logger
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup

	// Channels for packet ingress
	videoChan chan *PacedPacket
	audioChan chan *PacedPacket

	// Write callbacks (set by Bridge)
	// Protected by callbackMu for memory visibility
	callbackMu sync.RWMutex
	writeVideo func(data []byte, timestamp uint32) error
	writeAudio func(data []byte, timestamp uint32) error

	// State tracking
	lastVideoTS      uint32
	lastVideoSendAt  time.Time
	lastAudioTS      uint32
	lastAudioSendAt  time.Time
	firstVideoPacket bool
	firstAudioPacket bool

	// Statistics
	videoPacketsSent     uint64
	audioPacketsSent     uint64
	videoBurstsAbsorbed  uint64
	audioBurstsAbsorbed  uint64
	videoCatchupEvents   uint64
	audioCatchupEvents   uint64
	totalVideoDelay      time.Duration
	totalAudioDelay      time.Duration

	// Mutex for stats
	statsMu sync.RWMutex
}

// NewPacer creates a new RTP packet pacer
func NewPacer(ctx context.Context, logger *slog.Logger) *Pacer {
	ctx, cancel := context.WithCancel(ctx)

	return &Pacer{
		logger:           logger.With("component", "pacer"),
		ctx:              ctx,
		cancel:           cancel,
		videoChan:        make(chan *PacedPacket, 10), // Small buffer to absorb micro-bursts
		audioChan:        make(chan *PacedPacket, 10),
		firstVideoPacket: true,
		firstAudioPacket: true,
	}
}

// SetWriteCallbacks configures the output functions for paced packets
// MUST be called before Start() to ensure proper initialization
func (p *Pacer) SetWriteCallbacks(
	writeVideo func(data []byte, timestamp uint32) error,
	writeAudio func(data []byte, timestamp uint32) error,
) {
	p.callbackMu.Lock()
	defer p.callbackMu.Unlock()
	p.writeVideo = writeVideo
	p.writeAudio = writeAudio
}

// Start begins the pacer goroutines
func (p *Pacer) Start() {
	p.logger.Info("starting pacer goroutines")

	// Video pacer goroutine
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.videoPacerLoop()
	}()

	// Audio pacer goroutine
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.audioPacerLoop()
	}()

	// Stats logging goroutine
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.statsLoop()
	}()
}

// Stop gracefully stops the pacer
func (p *Pacer) Stop() {
	p.logger.Info("stopping pacer")
	p.cancel()
	p.wg.Wait()
}

// EnqueueVideo queues a video packet for paced transmission
func (p *Pacer) EnqueueVideo(packet *PacedPacket) error {
	select {
	case p.videoChan <- packet:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
		// Channel full - log burst absorption
		p.statsMu.Lock()
		p.videoBurstsAbsorbed++
		p.statsMu.Unlock()

		p.logger.Warn("video channel full - burst detected, blocking until space available",
			"queue_depth", len(p.videoChan),
			"bursts_absorbed", p.videoBurstsAbsorbed)

		// Block until space available (backpressure to RTSP reader)
		select {
		case p.videoChan <- packet:
			return nil
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
}

// EnqueueAudio queues an audio packet for paced transmission
func (p *Pacer) EnqueueAudio(packet *PacedPacket) error {
	select {
	case p.audioChan <- packet:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	default:
		// Channel full - log burst absorption
		p.statsMu.Lock()
		p.audioBurstsAbsorbed++
		p.statsMu.Unlock()

		p.logger.Warn("audio channel full - burst detected, blocking until space available",
			"queue_depth", len(p.audioChan),
			"bursts_absorbed", p.audioBurstsAbsorbed)

		// Block until space available
		select {
		case p.audioChan <- packet:
			return nil
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
}

// videoPacerLoop is the main video pacing goroutine
// Implements the leaky bucket algorithm from Section 8.2
func (p *Pacer) videoPacerLoop() {
	p.logger.Info("[pacer:video] started")

	for {
		select {
		case <-p.ctx.Done():
			p.logger.Info("[pacer:video] stopped (context cancelled)")
			return

		case packet := <-p.videoChan:
			if err := p.paceVideoPacket(packet); err != nil {
				p.logger.Error("[pacer:video] failed to pace packet",
					"timestamp", packet.Timestamp,
					"keyframe", packet.IsKeyframe,
					"error", err)
			}
		}
	}
}

// paceVideoPacket implements the core pacing logic for a single video packet
func (p *Pacer) paceVideoPacket(packet *PacedPacket) error {
	now := time.Now()

	// First packet - send immediately to establish timeline
	if p.firstVideoPacket {
		p.firstVideoPacket = false
		p.lastVideoTS = packet.Timestamp
		p.lastVideoSendAt = now

		p.logger.Info("[pacer:video] first packet - establishing timeline",
			"timestamp", packet.Timestamp,
			"keyframe", packet.IsKeyframe)

		// Get callback with proper synchronization
		p.callbackMu.RLock()
		writeVideoFn := p.writeVideo
		p.callbackMu.RUnlock()

		// Check for nil callback (should never happen, but defensive)
		if writeVideoFn == nil {
			return fmt.Errorf("writeVideo callback not set")
		}

		if err := writeVideoFn(packet.NALUs, packet.Timestamp); err != nil {
			return fmt.Errorf("write first video packet: %w", err)
		}

		p.statsMu.Lock()
		p.videoPacketsSent++
		p.statsMu.Unlock()

		return nil
	}

	// Calculate delay based on RTP timestamp delta
	// This is the CRITICAL pacing calculation from Section 2.2.2
	delay := p.calculateVideoDelay(packet.Timestamp)

	// Check for catch-up mode
	queueDepth := len(p.videoChan)
	if queueDepth >= catchupThreshold {
		// Enter catch-up mode: drain at 1.1x speed
		delay = time.Duration(float64(delay) / catchupSpeedMultiplier)

		p.statsMu.Lock()
		p.videoCatchupEvents++
		p.statsMu.Unlock()

		if p.videoCatchupEvents%10 == 1 {
			originalDelay := time.Duration(float64(delay) * catchupSpeedMultiplier)
			p.logger.Info("[pacer:video] catch-up mode activated",
				"queue_depth", queueDepth,
				"original_delay_ms", originalDelay/time.Millisecond,
				"catchup_delay_ms", delay/time.Millisecond,
				"total_catchup_events", p.videoCatchupEvents)
		}
	}

	// Cap delay to prevent infinite waits on timestamp errors
	if delay > maxPacketDelay {
		p.logger.Warn("[pacer:video] capping excessive delay",
			"calculated_delay_ms", delay/time.Millisecond,
			"max_delay_ms", maxPacketDelay/time.Millisecond,
			"timestamp_delta", packet.Timestamp-p.lastVideoTS)
		delay = maxPacketDelay
	}

	// Negative delay means timestamp went backwards - log but send immediately
	if delay < 0 {
		p.logger.Warn("[pacer:video] negative delay - timestamp went backwards",
			"last_ts", p.lastVideoTS,
			"current_ts", packet.Timestamp,
			"delta", int64(packet.Timestamp)-int64(p.lastVideoTS))
		delay = 0
	}

	// Track total delay for statistics
	p.statsMu.Lock()
	p.totalVideoDelay += delay
	p.statsMu.Unlock()

	// CRITICAL: Sleep to pace the packet transmission
	// This smooths out TCP bursts by restoring timing based on RTP timestamps
	if delay > 0 {
		select {
		case <-time.After(delay):
			// Delay completed
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}

	// Send the packet
	sendStart := time.Now()

	// Get callback with proper synchronization
	p.callbackMu.RLock()
	writeVideoFn := p.writeVideo
	p.callbackMu.RUnlock()

	// Check for nil callback (should never happen, but defensive)
	if writeVideoFn == nil {
		return fmt.Errorf("writeVideo callback not set")
	}

	if err := writeVideoFn(packet.NALUs, packet.Timestamp); err != nil {
		return fmt.Errorf("write video packet: %w", err)
	}
	sendDuration := time.Since(sendStart)

	// Update state
	p.lastVideoTS = packet.Timestamp
	p.lastVideoSendAt = time.Now()

	p.statsMu.Lock()
	p.videoPacketsSent++
	packetsSent := p.videoPacketsSent
	p.statsMu.Unlock()

	// Log periodically
	if packetsSent == 1 {
		p.logger.Info("[pacer:video] first paced packet sent",
			"delay_ms", delay/time.Millisecond,
			"send_duration_ms", sendDuration/time.Millisecond,
			"keyframe", packet.IsKeyframe)
	} else if packetsSent%300 == 0 {
		p.logger.Info("[pacer:video] pacing statistics",
			"packets_sent", packetsSent,
			"delay_ms", delay/time.Millisecond,
			"send_duration_ms", sendDuration/time.Millisecond,
			"queue_depth", queueDepth,
			"keyframe", packet.IsKeyframe)
	}

	return nil
}

// calculateVideoDelay calculates the delay before sending the next video packet
// Based on RTP timestamp delta (90kHz clock for H.264)
func (p *Pacer) calculateVideoDelay(currentTS uint32) time.Duration {
	// Calculate timestamp delta (handling uint32 wraparound)
	var tsDelta uint32
	if currentTS >= p.lastVideoTS {
		tsDelta = currentTS - p.lastVideoTS
	} else {
		// Wraparound case (rare but possible)
		tsDelta = (0xFFFFFFFF - p.lastVideoTS) + currentTS + 1
	}

	// Convert RTP timestamp delta to wall clock duration
	// RTP timestamp is in 90kHz units (video clock rate)
	// Duration = (tsDelta / 90000) seconds = (tsDelta * 1000) / 90000 milliseconds
	timestampDelay := time.Duration(tsDelta) * time.Second / videoClockRate

	// Calculate time elapsed since last send
	actualElapsed := time.Since(p.lastVideoSendAt)

	// Delay = timestamp_delay - actual_elapsed
	// If we're ahead of schedule, delay to catch up to nominal rate
	// If we're behind schedule, send immediately (delay will be negative, capped to 0)
	delay := timestampDelay - actualElapsed

	return delay
}

// audioPacerLoop is the main audio pacing goroutine
func (p *Pacer) audioPacerLoop() {
	p.logger.Info("[pacer:audio] started")

	for {
		select {
		case <-p.ctx.Done():
			p.logger.Info("[pacer:audio] stopped (context cancelled)")
			return

		case packet := <-p.audioChan:
			if err := p.paceAudioPacket(packet); err != nil {
				p.logger.Error("[pacer:audio] failed to pace packet",
					"timestamp", packet.Timestamp,
					"error", err)
			}
		}
	}
}

// paceAudioPacket implements the core pacing logic for a single audio packet
func (p *Pacer) paceAudioPacket(packet *PacedPacket) error {
	now := time.Now()

	// First packet - send immediately
	if p.firstAudioPacket {
		p.firstAudioPacket = false
		p.lastAudioTS = packet.Timestamp
		p.lastAudioSendAt = now

		p.logger.Info("[pacer:audio] first packet - establishing timeline",
			"timestamp", packet.Timestamp)

		// Get callback with proper synchronization
		p.callbackMu.RLock()
		writeAudioFn := p.writeAudio
		p.callbackMu.RUnlock()

		// Check for nil callback (should never happen, but defensive)
		if writeAudioFn == nil {
			return fmt.Errorf("writeAudio callback not set")
		}

		if err := writeAudioFn(packet.NALUs, packet.Timestamp); err != nil {
			return fmt.Errorf("write first audio packet: %w", err)
		}

		p.statsMu.Lock()
		p.audioPacketsSent++
		p.statsMu.Unlock()

		return nil
	}

	// Calculate delay based on RTP timestamp delta
	delay := p.calculateAudioDelay(packet.Timestamp)

	// Check for catch-up mode
	queueDepth := len(p.audioChan)
	if queueDepth >= catchupThreshold {
		delay = time.Duration(float64(delay) / catchupSpeedMultiplier)

		p.statsMu.Lock()
		p.audioCatchupEvents++
		p.statsMu.Unlock()
	}

	// Cap delay
	if delay > maxPacketDelay {
		p.logger.Warn("[pacer:audio] capping excessive delay",
			"calculated_delay_ms", delay/time.Millisecond,
			"max_delay_ms", maxPacketDelay/time.Millisecond)
		delay = maxPacketDelay
	}

	if delay < 0 {
		delay = 0
	}

	p.statsMu.Lock()
	p.totalAudioDelay += delay
	p.statsMu.Unlock()

	// Sleep to pace transmission
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}

	// Send the packet
	// Get callback with proper synchronization
	p.callbackMu.RLock()
	writeAudioFn := p.writeAudio
	p.callbackMu.RUnlock()

	// Check for nil callback (should never happen, but defensive)
	if writeAudioFn == nil {
		return fmt.Errorf("writeAudio callback not set")
	}

	if err := writeAudioFn(packet.NALUs, packet.Timestamp); err != nil {
		return fmt.Errorf("write audio packet: %w", err)
	}

	// Update state
	p.lastAudioTS = packet.Timestamp
	p.lastAudioSendAt = time.Now()

	p.statsMu.Lock()
	p.audioPacketsSent++
	p.statsMu.Unlock()

	return nil
}

// calculateAudioDelay calculates the delay before sending the next audio packet
// Based on RTP timestamp delta (48kHz clock for Opus)
func (p *Pacer) calculateAudioDelay(currentTS uint32) time.Duration {
	// Calculate timestamp delta (handling wraparound)
	var tsDelta uint32
	if currentTS >= p.lastAudioTS {
		tsDelta = currentTS - p.lastAudioTS
	} else {
		tsDelta = (0xFFFFFFFF - p.lastAudioTS) + currentTS + 1
	}

	// Convert RTP timestamp delta to wall clock duration
	// Audio clock rate is 48kHz
	timestampDelay := time.Duration(tsDelta) * time.Second / audioClockRate

	// Calculate time elapsed since last send
	actualElapsed := time.Since(p.lastAudioSendAt)

	// Delay to maintain nominal rate
	delay := timestampDelay - actualElapsed

	return delay
}

// statsLoop periodically logs pacer statistics
func (p *Pacer) statsLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.logStats()
		}
	}
}

// logStats logs current pacer statistics
func (p *Pacer) logStats() {
	p.statsMu.RLock()
	defer p.statsMu.RUnlock()

	var avgVideoDelay, avgAudioDelay time.Duration
	if p.videoPacketsSent > 0 {
		avgVideoDelay = p.totalVideoDelay / time.Duration(p.videoPacketsSent)
	}
	if p.audioPacketsSent > 0 {
		avgAudioDelay = p.totalAudioDelay / time.Duration(p.audioPacketsSent)
	}

	p.logger.Info("pacer statistics",
		"video_packets_sent", p.videoPacketsSent,
		"audio_packets_sent", p.audioPacketsSent,
		"video_bursts_absorbed", p.videoBurstsAbsorbed,
		"audio_bursts_absorbed", p.audioBurstsAbsorbed,
		"video_catchup_events", p.videoCatchupEvents,
		"audio_catchup_events", p.audioCatchupEvents,
		"avg_video_delay_ms", avgVideoDelay/time.Millisecond,
		"avg_audio_delay_ms", avgAudioDelay/time.Millisecond,
		"video_queue_depth", len(p.videoChan),
		"audio_queue_depth", len(p.audioChan))
}

// GetStats returns current pacer statistics
func (p *Pacer) GetStats() PacerStats {
	p.statsMu.RLock()
	defer p.statsMu.RUnlock()

	return PacerStats{
		VideoPacketsSent:    p.videoPacketsSent,
		AudioPacketsSent:    p.audioPacketsSent,
		VideoBurstsAbsorbed: p.videoBurstsAbsorbed,
		AudioBurstsAbsorbed: p.audioBurstsAbsorbed,
		VideoCatchupEvents:  p.videoCatchupEvents,
		AudioCatchupEvents:  p.audioCatchupEvents,
		VideoQueueDepth:     len(p.videoChan),
		AudioQueueDepth:     len(p.audioChan),
	}
}

// PacerStats contains pacer statistics
type PacerStats struct {
	VideoPacketsSent    uint64
	AudioPacketsSent    uint64
	VideoBurstsAbsorbed uint64
	AudioBurstsAbsorbed uint64
	VideoCatchupEvents  uint64
	AudioCatchupEvents  uint64
	VideoQueueDepth     int
	AudioQueueDepth     int
}
