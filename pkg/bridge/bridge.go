package bridge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Bridge connects RTSP streams to Cloudflare via WebRTC
type Bridge struct {
	logger      *slog.Logger
	cfClient    *cloudflare.Client
	sessionID   string
	pc          *webrtc.PeerConnection
	videoTrack  *webrtc.TrackLocalStaticRTP
	audioTrack  *webrtc.TrackLocalStaticRTP
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	// H.264 RTP packetization
	h264Payloader *codecs.H264Payloader
	videoSeqNum   uint16
	videoTS       uint32
	videoMu       sync.Mutex // Protects sequence number and timestamp
}

// NewBridge creates a new WebRTC bridge to Cloudflare
func NewBridge(ctx context.Context, cfClient *cloudflare.Client, logger *slog.Logger) (*Bridge, error) {
	ctx, cancel := context.WithCancel(ctx)

	b := &Bridge{
		logger:        logger,
		cfClient:      cfClient,
		ctx:           ctx,
		cancel:        cancel,
		h264Payloader: &codecs.H264Payloader{},
		videoSeqNum:   uint16(time.Now().UnixNano() & 0xFFFF), // Random starting sequence number
	}

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

	// Register H264 codec
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
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

	// Create video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		"video",
		"nest-camera-video",
	)
	if err != nil {
		return fmt.Errorf("create video track: %w", err)
	}
	b.videoTrack = videoTrack

	if _, err = pc.AddTrack(videoTrack); err != nil {
		return fmt.Errorf("add video track: %w", err)
	}

	// Create audio track
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		"audio",
		"nest-camera-audio",
	)
	if err != nil {
		return fmt.Errorf("create audio track: %w", err)
	}
	b.audioTrack = audioTrack

	if _, err = pc.AddTrack(audioTrack); err != nil {
		return fmt.Errorf("add audio track: %w", err)
	}

	b.logger.Info("WebRTC peer connection created with tracks")

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
	tracksReq := &cloudflare.TracksRequest{
		SessionDescription: &cloudflare.SessionDescription{
			SDP:  localSDP,
			Type: "offer",
		},
		Tracks: []cloudflare.TrackObject{
			{
				Location:  "local",
				Mid:       videoMid,
				TrackName: "video",
			},
			{
				Location:  "local",
				Mid:       audioMid,
				TrackName: "audio",
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
func (b *Bridge) WriteVideoSample(data []byte, duration time.Duration) error {
	if b.videoTrack == nil {
		return fmt.Errorf("video track not initialized")
	}

	b.videoMu.Lock()
	defer b.videoMu.Unlock()

	// Use H264Payloader to fragment NAL unit into MTU-sized RTP packets
	// MTU is set to 1200 bytes (safe for WebRTC with overhead for RTP/UDP/IP headers)
	const mtu = 1200
	payloads := b.h264Payloader.Payload(mtu, data)

	// Get current timestamp and sequence number
	timestamp := b.videoTS
	seqNum := b.videoSeqNum

	// Write each fragmented payload as a separate RTP packet
	for i, payload := range payloads {
		// Create RTP packet
		packet := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96, // H.264 payload type
				SequenceNumber: seqNum,
				Timestamp:      timestamp,
				Marker:         i == len(payloads)-1, // Mark last packet in frame
			},
			Payload: payload,
		}

		// Write packet to track
		if err := b.videoTrack.WriteRTP(packet); err != nil {
			if err == io.ErrClosedPipe {
				return nil // Track closed gracefully
			}
			return fmt.Errorf("write RTP packet %d/%d: %w", i+1, len(payloads), err)
		}

		// Increment sequence number for next packet
		seqNum++
	}

	// Update sequence number and timestamp for next sample
	b.videoSeqNum = seqNum

	// Increment timestamp based on duration (90kHz clock for H.264)
	// duration is in nanoseconds, convert to 90kHz ticks
	timestampIncrement := uint32(duration.Nanoseconds() * 90000 / 1e9)
	b.videoTS += timestampIncrement

	return nil
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

// WriteAudioSample writes audio data as a sample
func (b *Bridge) WriteAudioSample(data []byte, duration time.Duration) error {
	if b.audioTrack == nil {
		return fmt.Errorf("audio track not initialized")
	}

	// For StaticRTP, we need to packetize ourselves
	packet := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    111,
			SequenceNumber: uint16(time.Now().UnixNano() & 0xFFFF),
			Timestamp:      uint32(time.Now().UnixNano() / 1000000),
		},
		Payload: data,
	}

	return b.WriteAudioRTP(packet)
}

// GetSessionID returns the Cloudflare session ID
func (b *Bridge) GetSessionID() string {
	return b.sessionID
}

// GetConnectionState returns the peer connection state
func (b *Bridge) GetConnectionState() webrtc.PeerConnectionState {
	if b.pc == nil {
		return webrtc.PeerConnectionStateNew
	}
	return b.pc.ConnectionState()
}

// Close closes the bridge and all resources
func (b *Bridge) Close() error {
	b.logger.Info("closing bridge")

	b.cancel()
	b.wg.Wait()

	if b.pc != nil {
		if err := b.pc.Close(); err != nil {
			b.logger.Error("error closing peer connection", "error", err)
		}
	}

	return nil
}
