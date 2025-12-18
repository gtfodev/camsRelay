package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Bridge connects RTSP streams to Cloudflare via WebRTC
type Bridge struct {
	logger      *slog.Logger
	cfClient    *cloudflare.Client
	cameraID    string // Unique camera identifier for track naming
	sessionID   string
	pc          *webrtc.PeerConnection
	videoTrack  *webrtc.TrackLocalStaticRTP
	audioTrack  *webrtc.TrackLocalStaticRTP
	videoSender *webrtc.RTPSender // RTCP reader for video track
	audioSender *webrtc.RTPSender // RTCP reader for audio track
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	// Leaky bucket pacer (Section 8.2 from report)
	pacer *Pacer

	// H.264 RTP packetization
	h264Payloader *codecs.H264Payloader
	videoSeqNum   uint16
	videoMu       sync.Mutex // Protects sequence number

	// Audio RTP packetization
	audioSeqNum uint16
	audioMu     sync.Mutex // Protects audio sequence number

	// Timestamp validation and diagnostics
	lastVideoTS uint32
	tsWarnCount uint32

	// Cached connection state (to avoid blocking on pc.ConnectionState())
	connStateMu     sync.RWMutex
	cachedConnState webrtc.PeerConnectionState
}

// NewBridge creates a new WebRTC bridge to Cloudflare
func NewBridge(ctx context.Context, cameraID string, cfClient *cloudflare.Client, logger *slog.Logger) (*Bridge, error) {
	ctx, cancel := context.WithCancel(ctx)

	b := &Bridge{
		logger:          logger,
		cfClient:        cfClient,
		cameraID:        cameraID,
		ctx:             ctx,
		cancel:          cancel,
		h264Payloader:   &codecs.H264Payloader{},
		videoSeqNum:     uint16(time.Now().UnixNano() & 0xFFFF), // Random starting sequence number
		cachedConnState: webrtc.PeerConnectionStateNew,          // Initial state
	}

	// Create pacer for smooth packet transmission (report Section 8.2)
	b.pacer = NewPacer(ctx, logger)

	return b, nil
}

// CreateSession creates a Cloudflare session and PeerConnection
func (b *Bridge) CreateSession(ctx context.Context) error {
	// Create Cloudflare session
	session, err := b.cfClient.CreateSession(ctx)
	if err != nil {
		return fmt.Errorf("create Cloudflare session: %w", err)
	}
	b.sessionID = session.SessionID

	b.logger.Info("created Cloudflare session", "session_id", b.sessionID)

	// Create Pion PeerConnection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create media engine with H264 and Opus
	m := &webrtc.MediaEngine{}

	// Register H264 codec (Main Profile to match Nest camera output)
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return fmt.Errorf("register H264 codec: %w", err)
	}

	// Register Opus codec (we'll transcode AAC to Opus or use passthrough)
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("register Opus codec: %w", err)
	}

	// Create API with custom media engine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	b.pc = pc

	// Set up connection state change handler to cache state
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		b.connStateMu.Lock()
		b.cachedConnState = state
		b.connStateMu.Unlock()
		b.logger.Info("peer connection state changed", "state", state.String())
	})

	// Create video track with unique name based on camera ID
	// This ensures viewer can map tracks back to cameras correctly
	videoTrackName := fmt.Sprintf("%s-video", b.cameraID)
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		videoTrackName,
		"nest-camera-video",
	)
	if err != nil {
		return fmt.Errorf("create video track: %w", err)
	}
	b.videoTrack = videoTrack

	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("add video track: %w", err)
	}
	b.videoSender = videoSender

	// Create audio track with unique name based on camera ID
	audioTrackName := fmt.Sprintf("%s-audio", b.cameraID)
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		audioTrackName,
		"nest-camera-audio",
	)
	if err != nil {
		return fmt.Errorf("create audio track: %w", err)
	}
	b.audioTrack = audioTrack

	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		return fmt.Errorf("add audio track: %w", err)
	}
	b.audioSender = audioSender

	b.logger.Info("WebRTC peer connection created with tracks")

	// Start RTCP reader goroutines
	b.startRTCPReaders()

	return nil
}

// Negotiate performs SDP negotiation with Cloudflare
func (b *Bridge) Negotiate(ctx context.Context) error {
	// Create offer
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}

	// Set local description
	if err := b.pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering
	gatherComplete := webrtc.GatheringCompletePromise(b.pc)
	select {
	case <-gatherComplete:
	case <-time.After(10 * time.Second):
		return fmt.Errorf("ICE gathering timeout")
	case <-ctx.Done():
		return ctx.Err()
	}

	localSDP := b.pc.LocalDescription().SDP

	b.logger.Debug("created SDP offer", "sdp", localSDP)

	// Get mids from transceivers (assigned after SetLocalDescription)
	var videoMid, audioMid string
	for _, t := range b.pc.GetTransceivers() {
		if t.Mid() == "" {
			continue
		}
		switch t.Kind() {
		case webrtc.RTPCodecTypeVideo:
			videoMid = t.Mid()
		case webrtc.RTPCodecTypeAudio:
			audioMid = t.Mid()
		}
	}

	b.logger.Info("transceivers ready", "video_mid", videoMid, "audio_mid", audioMid)

	// Send offer to Cloudflare via AddTracks
	// Use unique track names so viewer can map tracks back to cameras
	tracksReq := &cloudflare.TracksRequest{
		SessionDescription: &cloudflare.SessionDescription{
			SDP:  localSDP,
			Type: "offer",
		},
		Tracks: []cloudflare.TrackObject{
			{
				Location:  "local",
				Mid:       videoMid,
				TrackName: fmt.Sprintf("%s-video", b.cameraID),
			},
			{
				Location:  "local",
				Mid:       audioMid,
				TrackName: fmt.Sprintf("%s-audio", b.cameraID),
			},
		},
	}

	tracksResp, err := b.cfClient.AddTracksWithRetry(ctx, b.sessionID, tracksReq, 3)
	if err != nil {
		return fmt.Errorf("add tracks to Cloudflare: %w", err)
	}

	if tracksResp.SessionDescription == nil {
		return fmt.Errorf("Cloudflare did not return SDP answer")
	}

	// Set remote description (answer from Cloudflare)
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  tracksResp.SessionDescription.SDP,
	}

	if err := b.pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	b.logger.Info("SDP negotiation complete",
		"session_id", b.sessionID,
		"tracks", len(tracksResp.Tracks))

	// Start pacer now that WebRTC session is established
	// Configure pacer callbacks to write to our tracks
	b.pacer.SetWriteCallbacks(
		b.writeVideoSampleDirect,   // Video write function
		b.writeAudioSampleDirect,   // Audio write function
	)
	b.pacer.Start()
	b.logger.Info("pacer started - TCP bursts will be smoothed")

	return nil
}

// WriteVideoRTP writes a video RTP packet to the WebRTC track
func (b *Bridge) WriteVideoRTP(packet *rtp.Packet) error {
	if b.videoTrack == nil {
		return fmt.Errorf("video track not initialized")
	}

	if err := b.videoTrack.WriteRTP(packet); err != nil {
		if err == io.ErrClosedPipe {
			return nil // Track closed gracefully
		}
		return err
	}

	return nil
}

// WriteVideoSample writes H.264 video data as a sample with proper RTP packetization
// The input data is expected to be in AVC format (4-byte length prefix per NAL unit)
// sourceTimestamp is the original RTP timestamp from the RTSP source (90kHz clock)
//
// NEW: This now enqueues to the pacer instead of writing directly (Section 8.2)
func (b *Bridge) WriteVideoSample(data []byte, sourceTimestamp uint32) error {
	if b.videoTrack == nil {
		return fmt.Errorf("video track not initialized")
	}

	b.videoMu.Lock()
	defer b.videoMu.Unlock()

	// Timestamp validation and diagnostics
	if b.lastVideoTS > 0 {
		// Detect timestamp going backwards (smoking gun for boomerang issue)
		if sourceTimestamp < b.lastVideoTS {
			b.tsWarnCount++
			b.logger.Warn("TIMESTAMP WENT BACKWARDS - BOOMERANG DETECTED",
				"last_ts", b.lastVideoTS,
				"current_ts", sourceTimestamp,
				"delta", int64(sourceTimestamp)-int64(b.lastVideoTS),
				"occurrence_count", b.tsWarnCount)
		}

		// Detect large timestamp gaps (potential issue)
		delta := sourceTimestamp - b.lastVideoTS
		expectedDelta := uint32(90000 / 30) // ~3000 for 30fps
		if delta > expectedDelta*3 {        // More than 3x expected
			b.logger.Warn("large timestamp gap detected",
				"delta", delta,
				"expected", expectedDelta,
				"delta_ms", delta/90)
		}
	}

	b.lastVideoTS = sourceTimestamp

	// Enqueue to pacer for smooth transmission (prevents TCP burst forwarding)
	// The pacer will calculate delays based on RTP timestamp deltas
	packet := &PacedPacket{
		Timestamp:  sourceTimestamp,
		NALUs:      data, // Keep in AVC format for now
		TrackType:  "video",
		ReceivedAt: time.Now(),
	}

	return b.pacer.EnqueueVideo(packet)
}

// writeVideoSampleDirect is the actual write function called by the pacer
// This performs the packetization and WriteRTP after pacing delay
// Note: Mutex must NOT be locked here as this is called from pacer goroutine
func (b *Bridge) writeVideoSampleDirect(data []byte, sourceTimestamp uint32) error {
	if b.videoTrack == nil {
		return fmt.Errorf("video track not initialized")
	}

	// Extract NAL units from AVC format (4-byte length prefix per NALU)
	// The H264Payloader expects raw NAL units without length prefixes
	nalus, err := extractNALUs(data)
	if err != nil {
		return fmt.Errorf("extract NAL units: %w", err)
	}

	// Lock only for sequence number access (minimize lock contention)
	b.videoMu.Lock()
	seqNum := b.videoSeqNum
	b.videoMu.Unlock()

	// Use source timestamp from RTSP (passthrough - DO NOT synthesize)
	timestamp := sourceTimestamp

	// Packetize and send each NAL unit
	const mtu = 1200 // Safe MTU for WebRTC
	for naluIdx, nalu := range nalus {
		// Use H264Payloader to fragment NAL unit into MTU-sized RTP packets
		payloads := b.h264Payloader.Payload(mtu, nalu)

		// Write each fragmented payload as a separate RTP packet
		for i, payload := range payloads {
			// Create RTP packet
			packet := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    96, // H.264 payload type
					SequenceNumber: seqNum,
					Timestamp:      timestamp, // PASSTHROUGH from source
					// Mark last packet of last NAL unit in frame
					Marker: (naluIdx == len(nalus)-1) && (i == len(payloads)-1),
				},
				Payload: payload,
			}

			// Write packet to track
			if err := b.videoTrack.WriteRTP(packet); err != nil {
				if err == io.ErrClosedPipe {
					return nil // Track closed gracefully
				}
				b.logger.Error("failed to write RTP packet",
					"nalu", naluIdx+1,
					"total_nalus", len(nalus),
					"packet_num", i+1,
					"total_packets", len(payloads),
					"timestamp", timestamp,
					"connection_state", b.GetConnectionState().String(),
					"error", err)
				return fmt.Errorf("write RTP packet NALU %d/%d pkt %d/%d (state=%s): %w",
					naluIdx+1, len(nalus), i+1, len(payloads), b.GetConnectionState().String(), err)
			}

			// Increment sequence number for next packet
			seqNum++
		}
	}

	// Update sequence number
	b.videoMu.Lock()
	b.videoSeqNum = seqNum
	b.videoMu.Unlock()

	return nil
}

// extractNALUs extracts individual NAL units from AVC format data
// AVC format: [4-byte length][NAL data][4-byte length][NAL data]...
// Returns slice of raw NAL units (without length prefixes)
func extractNALUs(data []byte) ([][]byte, error) {
	var nalus [][]byte
	offset := 0

	for offset < len(data) {
		// Need at least 4 bytes for length prefix
		if offset+4 > len(data) {
			return nil, fmt.Errorf("incomplete NAL unit at offset %d: need 4 bytes for length, have %d", offset, len(data)-offset)
		}

		// Read 4-byte big-endian length
		naluLen := int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
		offset += 4

		// Validate length
		if offset+naluLen > len(data) {
			return nil, fmt.Errorf("invalid NAL unit length %d at offset %d: exceeds data bounds", naluLen, offset-4)
		}

		// Extract NAL unit (without length prefix)
		nalu := data[offset : offset+naluLen]
		nalus = append(nalus, nalu)

		offset += naluLen
	}

	return nalus, nil
}

// WriteAudioRTP writes an audio RTP packet to the WebRTC track
func (b *Bridge) WriteAudioRTP(packet *rtp.Packet) error {
	if b.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}

	if err := b.audioTrack.WriteRTP(packet); err != nil {
		if err == io.ErrClosedPipe {
			return nil
		}
		return err
	}

	return nil
}

// WriteAudioSample writes audio data as a sample with source timestamp
// sourceTimestamp is the original RTP timestamp from the RTSP source (48kHz clock for AAC)
//
// NEW: This now enqueues to the pacer instead of writing directly (Section 8.2)
func (b *Bridge) WriteAudioSample(data []byte, sourceTimestamp uint32) error {
	if b.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}

	// Enqueue to pacer for smooth transmission
	packet := &PacedPacket{
		Timestamp:  sourceTimestamp,
		NALUs:      data,
		TrackType:  "audio",
		ReceivedAt: time.Now(),
	}

	return b.pacer.EnqueueAudio(packet)
}

// writeAudioSampleDirect is the actual write function called by the pacer
func (b *Bridge) writeAudioSampleDirect(data []byte, sourceTimestamp uint32) error {
	if b.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}

	b.audioMu.Lock()
	defer b.audioMu.Unlock()

	// For StaticRTP, we need to packetize ourselves
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: b.audioSeqNum,
			Timestamp:      sourceTimestamp, // PASSTHROUGH from source (48kHz clock)
		},
		Payload: data,
	}

	b.audioSeqNum++

	return b.WriteAudioRTP(packet)
}

// GetSessionID returns the Cloudflare session ID
func (b *Bridge) GetSessionID() string {
	return b.sessionID
}

// GetConnectionState returns the cached peer connection state
// This uses the cached value to avoid blocking on pc.ConnectionState()
func (b *Bridge) GetConnectionState() webrtc.PeerConnectionState {
	b.connStateMu.RLock()
	defer b.connStateMu.RUnlock()
	return b.cachedConnState
}

// startRTCPReaders spawns goroutines to read RTCP feedback from Cloudflare
func (b *Bridge) startRTCPReaders() {
	// Video track RTCP reader
	if b.videoSender != nil {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.readRTCP(b.videoSender, "video")
		}()
	}

	// Audio track RTCP reader
	if b.audioSender != nil {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.readRTCP(b.audioSender, "audio")
		}()
	}
}

// readRTCP reads RTCP packets from an RTPSender and logs feedback
func (b *Bridge) readRTCP(sender *webrtc.RTPSender, trackType string) {
	b.logger.Info("[rtcp:reader] started", "track", trackType)

	for {
		// Read RTCP packets with context cancellation check
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			select {
			case <-b.ctx.Done():
				b.logger.Info("[rtcp:reader] stopped (context cancelled)", "track", trackType)
				return
			default:
				if err == io.EOF || err == io.ErrClosedPipe {
					b.logger.Info("[rtcp:reader] stopped (track closed)", "track", trackType)
					return
				}
				b.logger.Error("[rtcp:reader] read error", "track", trackType, "error", err)
				return
			}
		}

		// Process received RTCP packets
		for _, packet := range packets {
			switch pkt := packet.(type) {
			case *rtcp.PictureLossIndication:
				b.logger.Warn("RTCP PLI received - viewer requesting keyframe",
					"track", trackType,
					"media_ssrc", pkt.MediaSSRC,
					"sender_ssrc", pkt.SenderSSRC)

			case *rtcp.FullIntraRequest:
				b.logger.Warn("RTCP FIR received - viewer requesting keyframe",
					"track", trackType,
					"media_ssrc", pkt.MediaSSRC)

			case *rtcp.ReceiverEstimatedMaximumBitrate:
				b.logger.Debug("RTCP REMB received",
					"track", trackType,
					"bitrate_bps", pkt.Bitrate)

			case *rtcp.ReceiverReport:
				b.logger.Debug("RTCP RR received",
					"track", trackType,
					"ssrc", pkt.SSRC,
					"reports", len(pkt.Reports))

			default:
				b.logger.Debug("RTCP packet received",
					"track", trackType,
					"type", fmt.Sprintf("%T", packet))
			}
		}
	}
}

// Close closes the bridge and all resources
func (b *Bridge) Close() error {
	b.logger.Info("closing bridge")

	// Stop pacer first to drain queued packets
	if b.pacer != nil {
		b.pacer.Stop()
	}

	b.cancel()
	b.wg.Wait()

	if b.pc != nil {
		if err := b.pc.Close(); err != nil {
			b.logger.Error("error closing peer connection", "error", err)
		}
	}

	return nil
}
