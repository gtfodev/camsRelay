package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/config"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Diagnostic tool to analyze RTP flow and RTCP feedback for Cloudflare Calls
// This tool creates a minimal test pattern to identify where the video flow breaks.

type DiagnosticSession struct {
	logger      *slog.Logger
	cfClient    *cloudflare.Client
	pc          *webrtc.PeerConnection
	videoTrack  *webrtc.TrackLocalStaticRTP
	sessionID   string

	// Diagnostics counters
	rtpSent         atomic.Uint64
	rtcpReceived    atomic.Uint64
	pliReceived     atomic.Uint64
	firReceived     atomic.Uint64
	nackReceived    atomic.Uint64
	senderReports   atomic.Uint64
	receiverReports atomic.Uint64

	// State tracking
	startTime       time.Time
	lastRTCPTime    time.Time
	connectionState atomic.Value // webrtc.PeerConnectionState

	// RTCP handler
	rtcpReader *webrtc.RTPReceiver
}

func main() {
	// Initialize logger with debug level for maximum verbosity
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	logger.Info("=== Cloudflare Calls RTP Flow Diagnostic Tool ===")
	logger.Info("This tool will:")
	logger.Info("  1. Create a Cloudflare session")
	logger.Info("  2. Send test H.264 RTP packets")
	logger.Info("  3. Monitor for RTCP feedback (PLI, FIR, NACK)")
	logger.Info("  4. Verify continuous flow and log everything")
	logger.Info("")

	// Load config
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Create Cloudflare client
	cfClient := cloudflare.NewClient(
		cfg.Cloudflare.AppID,
		cfg.Cloudflare.APIToken,
		logger.With("component", "cloudflare"),
	)

	// Create diagnostic session
	session := &DiagnosticSession{
		logger:   logger,
		cfClient: cfClient,
		startTime: time.Now(),
	}
	session.connectionState.Store(webrtc.PeerConnectionStateNew)

	ctx := context.Background()

	// Create session and establish WebRTC connection
	if err := session.createSession(ctx); err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}

	// Wait for connection
	if err := session.waitForConnection(ctx); err != nil {
		log.Fatalf("Failed to wait for connection: %v", err)
	}

	logger.Info("✓ WebRTC connection established - starting test pattern")

	// Start RTCP monitoring goroutine
	go session.monitorRTCP(ctx)

	// Start sending test pattern
	go session.sendTestPattern(ctx)

	// Run for 30 seconds
	logger.Info("Running diagnostic for 30 seconds...")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-time.After(30 * time.Second):
		logger.Info("Diagnostic duration completed")
	case <-sigChan:
		logger.Info("Interrupted by user")
	}

	// Print summary
	session.printSummary()

	// Cleanup
	if session.pc != nil {
		session.pc.Close()
	}

	logger.Info("Diagnostic complete")
}

func (s *DiagnosticSession) createSession(ctx context.Context) error {
	s.logger.Info("Creating Cloudflare session...")

	// Create Cloudflare session
	sessionResp, err := s.cfClient.CreateSession(ctx)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	s.sessionID = sessionResp.SessionID
	s.logger.Info("✓ Cloudflare session created", "session_id", s.sessionID)

	// Create PeerConnection with detailed logging
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	// Create media engine with H264
	m := &webrtc.MediaEngine{}
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

	// Create API with media engine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("create peer connection: %w", err)
	}
	s.pc = pc

	// Setup connection state handler
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		s.connectionState.Store(state)
		s.logger.Info(">>> Connection state changed", "state", state.String())
	})

	// Setup ICE connection state handler
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		s.logger.Info(">>> ICE connection state changed", "state", state.String())
	})

	// Create video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeH264,
			ClockRate: 90000,
		},
		"video",
		"diagnostic-video",
	)
	if err != nil {
		return fmt.Errorf("create video track: %w", err)
	}
	s.videoTrack = videoTrack

	// Add track and capture RTPSender
	rtpSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("add track: %w", err)
	}

	s.logger.Info("✓ Video track added to peer connection")

	// Setup RTCP packet handler on the RTPSender
	// This is CRITICAL - we need to read RTCP feedback from Cloudflare
	go s.readRTCP(rtpSender)

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherComplete:
		s.logger.Info("✓ ICE gathering complete")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("ICE gathering timeout")
	}

	localSDP := pc.LocalDescription().SDP

	// Get mids
	var videoMid string
	for _, t := range pc.GetTransceivers() {
		if t.Mid() != "" && t.Kind() == webrtc.RTPCodecTypeVideo {
			videoMid = t.Mid()
			break
		}
	}

	s.logger.Info("✓ SDP offer created", "video_mid", videoMid)

	// Send to Cloudflare
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
		},
	}

	tracksResp, err := s.cfClient.AddTracksWithRetry(ctx, s.sessionID, tracksReq, 3)
	if err != nil {
		return fmt.Errorf("add tracks: %w", err)
	}

	if tracksResp.SessionDescription == nil {
		return fmt.Errorf("no SDP answer from Cloudflare")
	}

	// Set remote description
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  tracksResp.SessionDescription.SDP,
	}

	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	s.logger.Info("✓ SDP negotiation complete")
	return nil
}

func (s *DiagnosticSession) waitForConnection(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			state := s.connectionState.Load().(webrtc.PeerConnectionState)
			return fmt.Errorf("timeout waiting for connection (state=%s): %w", state.String(), waitCtx.Err())
		case <-ticker.C:
			state := s.connectionState.Load().(webrtc.PeerConnectionState)

			if state == webrtc.PeerConnectionStateConnected {
				s.logger.Info("✓ Connection established")
				return nil
			}

			if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
				return fmt.Errorf("connection failed: state=%s", state.String())
			}
		}
	}
}

// readRTCP reads RTCP packets from the RTPSender
// This is where we'll see PLI, FIR, NACK, and other feedback
func (s *DiagnosticSession) readRTCP(rtpSender *webrtc.RTPSender) {
	s.logger.Info(">>> Starting RTCP read loop")

	for {
		packets, _, err := rtpSender.ReadRTCP()
		if err != nil {
			if err == io.EOF || err == io.ErrClosedPipe {
				s.logger.Info(">>> RTCP read loop ended (connection closed)")
				return
			}
			s.logger.Error(">>> RTCP read error", "error", err)
			continue
		}

		s.rtcpReceived.Add(uint64(len(packets)))
		s.lastRTCPTime = time.Now()

		for _, packet := range packets {
			s.logger.Info(">>> RTCP PACKET RECEIVED",
				"type", fmt.Sprintf("%T", packet),
				"elapsed", time.Since(s.startTime).Round(time.Millisecond))

			switch pkt := packet.(type) {
			case *rtcp.PictureLossIndication:
				s.pliReceived.Add(1)
				s.logger.Warn("!!! PLI RECEIVED !!!",
					"media_ssrc", pkt.MediaSSRC,
					"sender_ssrc", pkt.SenderSSRC,
					"description", "Cloudflare is requesting a keyframe - decoder needs IDR frame")

			case *rtcp.FullIntraRequest:
				s.firReceived.Add(1)
				s.logger.Warn("!!! FIR RECEIVED !!!",
					"media_ssrc", pkt.MediaSSRC,
					"description", "Cloudflare is requesting full intra refresh")

			case *rtcp.TransportLayerNack:
				s.nackReceived.Add(1)
				s.logger.Warn("!!! NACK RECEIVED !!!",
					"media_ssrc", pkt.MediaSSRC,
					"sender_ssrc", pkt.SenderSSRC,
					"nacks", len(pkt.Nacks),
					"description", "Cloudflare detected packet loss")
				for i, nack := range pkt.Nacks {
					s.logger.Info("    NACK detail",
						"index", i,
						"packet_id", nack.PacketID,
						"lost_packets", nack.LostPackets)
				}

			case *rtcp.ReceiverReport:
				s.receiverReports.Add(1)
				s.logger.Debug(">>> Receiver Report",
					"reports", len(pkt.Reports))
				for i, report := range pkt.Reports {
					s.logger.Debug("    RR detail",
						"index", i,
						"ssrc", report.SSRC,
						"fraction_lost", report.FractionLost,
						"total_lost", report.TotalLost,
						"last_seq", report.LastSequenceNumber,
						"jitter", report.Jitter)
				}

			case *rtcp.SenderReport:
				s.senderReports.Add(1)
				s.logger.Debug(">>> Sender Report",
					"ssrc", pkt.SSRC,
					"ntp_time", pkt.NTPTime,
					"rtp_time", pkt.RTPTime,
					"packet_count", pkt.PacketCount,
					"octet_count", pkt.OctetCount)

			default:
				s.logger.Info(">>> Other RTCP packet",
					"type", fmt.Sprintf("%T", pkt))
			}
		}
	}
}

// monitorRTCP periodically checks if we're receiving RTCP
func (s *DiagnosticSession) monitorRTCP(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rtcpCount := s.rtcpReceived.Load()
			timeSinceLast := time.Since(s.lastRTCPTime)

			if rtcpCount == 0 {
				s.logger.Warn("⚠️  NO RTCP RECEIVED YET",
					"elapsed", time.Since(s.startTime).Round(time.Second))
			} else if timeSinceLast > 10*time.Second {
				s.logger.Warn("⚠️  No RTCP for 10+ seconds",
					"last_rtcp", timeSinceLast.Round(time.Second))
			}
		}
	}
}

// sendTestPattern sends H.264 test RTP packets
// Sends a simple pattern: IDR keyframe every 2 seconds, P-frames in between
func (s *DiagnosticSession) sendTestPattern(ctx context.Context) {
	s.logger.Info(">>> Starting test pattern transmission")

	// Create H264 payloader
	payloader := &codecs.H264Payloader{}
	const mtu = 1200

	// Sequence number and timestamp
	var seqNum uint16 = 1000
	var timestamp uint32 = 0

	// Test NALU patterns
	// IDR frame: SPS + PPS + IDR slice
	sps := []byte{0x67, 0x42, 0xe0, 0x1f, 0x8c, 0x8d, 0x40, 0x50, 0x17, 0xfc, 0xb0, 0x0f, 0x08, 0x84, 0x6a}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	idr := make([]byte, 1000) // Dummy IDR slice
	idr[0] = 0x65 // IDR NALU type
	for i := 1; i < len(idr); i++ {
		idr[i] = byte(i % 256)
	}

	// P-frame
	pframe := make([]byte, 800)
	pframe[0] = 0x41 // P-frame NALU type
	for i := 1; i < len(pframe); i++ {
		pframe[i] = byte(i % 256)
	}

	ticker := time.NewTicker(33 * time.Millisecond) // ~30fps
	defer ticker.Stop()

	frameCount := 0
	keyframeInterval := 60 // Keyframe every 2 seconds @ 30fps

	s.logger.Info(">>> Sending first keyframe (SPS + PPS + IDR)")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frameCount++
			isKeyframe := frameCount%keyframeInterval == 1

			if isKeyframe {
				// Send keyframe: SPS + PPS + IDR
				s.logger.Info(">>> Sending KEYFRAME",
					"frame", frameCount,
					"seq_start", seqNum,
					"timestamp", timestamp)

				// Send SPS
				seqNum = s.sendNALU(sps, seqNum, timestamp, payloader, mtu, false)
				// Send PPS
				seqNum = s.sendNALU(pps, seqNum, timestamp, payloader, mtu, false)
				// Send IDR
				seqNum = s.sendNALU(idr, seqNum, timestamp, payloader, mtu, true)
			} else {
				// Send P-frame
				if frameCount%300 == 0 { // Log every 10 seconds
					s.logger.Info(">>> Sending P-frame",
						"frame", frameCount,
						"seq", seqNum,
						"timestamp", timestamp)
				}
				seqNum = s.sendNALU(pframe, seqNum, timestamp, payloader, mtu, true)
			}

			// Increment timestamp (90kHz clock, 33ms = ~3000 ticks)
			timestamp += 3000
		}
	}
}

// sendNALU sends a single NALU as RTP packets
func (s *DiagnosticSession) sendNALU(nalu []byte, seqNum uint16, timestamp uint32, payloader *codecs.H264Payloader, mtu int, marker bool) uint16 {
	payloads := payloader.Payload(uint16(mtu), nalu)

	for i, payload := range payloads {
		isLast := i == len(payloads)-1
		packet := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: seqNum,
				Timestamp:      timestamp,
				Marker:         marker && isLast,
			},
			Payload: payload,
		}

		if err := s.videoTrack.WriteRTP(packet); err != nil {
			if err != io.ErrClosedPipe {
				s.logger.Error(">>> RTP write failed", "error", err, "seq", seqNum)
			}
			return seqNum
		}

		s.rtpSent.Add(1)
		seqNum++
	}

	return seqNum
}

func (s *DiagnosticSession) printSummary() {
	duration := time.Since(s.startTime)

	separator := strings.Repeat("=", 80)
	fmt.Println("\n" + separator)
	fmt.Println("DIAGNOSTIC SUMMARY")
	fmt.Println(separator)
	fmt.Printf("Duration:             %s\n", duration.Round(time.Second))
	fmt.Printf("Session ID:           %s\n", s.sessionID)
	fmt.Printf("Final State:          %s\n", s.connectionState.Load().(webrtc.PeerConnectionState).String())
	fmt.Println()

	fmt.Println("RTP PACKETS SENT:")
	fmt.Printf("  Total:              %d packets\n", s.rtpSent.Load())
	fmt.Printf("  Rate:               %.1f pps\n", float64(s.rtpSent.Load())/duration.Seconds())
	fmt.Println()

	fmt.Println("RTCP FEEDBACK RECEIVED:")
	fmt.Printf("  Total RTCP packets: %d\n", s.rtcpReceived.Load())
	fmt.Printf("  PLI (keyframe req): %d\n", s.pliReceived.Load())
	fmt.Printf("  FIR (full refresh): %d\n", s.firReceived.Load())
	fmt.Printf("  NACK (packet loss): %d\n", s.nackReceived.Load())
	fmt.Printf("  Receiver Reports:   %d\n", s.receiverReports.Load())
	fmt.Printf("  Sender Reports:     %d\n", s.senderReports.Load())
	fmt.Println()

	// Analysis
	fmt.Println("ANALYSIS:")

	if s.rtpSent.Load() == 0 {
		fmt.Println("  ❌ CRITICAL: No RTP packets sent - sending goroutine may have exited")
	} else {
		fmt.Printf("  ✓ RTP sending appears to be working (%d packets)\n", s.rtpSent.Load())
	}

	if s.rtcpReceived.Load() == 0 {
		fmt.Println("  ❌ WARNING: No RTCP feedback received from Cloudflare")
		fmt.Println("     - This could indicate RTCP read loop not working")
		fmt.Println("     - Or Cloudflare not sending feedback")
	} else {
		fmt.Printf("  ✓ RTCP feedback is being received (%d packets)\n", s.rtcpReceived.Load())
	}

	if s.pliReceived.Load() > 0 {
		fmt.Printf("  ⚠️  IMPORTANT: Cloudflare sent %d PLI requests for keyframes\n", s.pliReceived.Load())
		fmt.Println("     - This means Cloudflare received packets but couldn't decode them")
		fmt.Println("     - Likely cause: Missing keyframe or SPS/PPS")
		fmt.Println("     - ACTION: Ensure first frame is complete IDR (SPS+PPS+IDR)")
	}

	if s.firReceived.Load() > 0 {
		fmt.Printf("  ⚠️  IMPORTANT: Cloudflare sent %d FIR requests\n", s.firReceived.Load())
		fmt.Println("     - Similar to PLI - decoder needs full refresh")
	}

	if s.nackReceived.Load() > 0 {
		fmt.Printf("  ⚠️  Cloudflare detected packet loss (%d NACKs)\n", s.nackReceived.Load())
		fmt.Println("     - Network issues or sequence number problems")
	}

	state := s.connectionState.Load().(webrtc.PeerConnectionState)
	if state != webrtc.PeerConnectionStateConnected {
		fmt.Printf("  ❌ Connection not in 'connected' state: %s\n", state.String())
	}

	fmt.Println(separator)

	// Root cause hypothesis
	fmt.Println("\nROOT CAUSE HYPOTHESIS:")
	if s.pliReceived.Load() > 0 {
		fmt.Println("  → Cloudflare IS receiving RTP packets (hence PLI requests)")
		fmt.Println("  → But decoder CANNOT decode them (hence PLI)")
		fmt.Println("  → Most likely: First packet sent was NOT a complete keyframe")
		fmt.Println("  → Or: SPS/PPS not sent before IDR")
		fmt.Println("  → ACTION NEEDED: Verify main app sends SPS+PPS+IDR as first frames")
	} else if s.rtcpReceived.Load() == 0 {
		fmt.Println("  → No RTCP received - RTCP read loop may not be set up")
		fmt.Println("  → ACTION NEEDED: Add RTCP reader to main app's bridge.go")
	} else if s.rtpSent.Load() == 0 {
		fmt.Println("  → RTP sending goroutine failed - check goroutine lifecycle")
	} else {
		fmt.Println("  → Connection and flow appear healthy")
		fmt.Println("  → If video still black in production, check:")
		fmt.Println("     1. Is first frame a complete keyframe?")
		fmt.Println("     2. Are we responding to PLI with keyframes?")
	}

	fmt.Println()
}
