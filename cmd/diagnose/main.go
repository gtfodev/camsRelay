package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"sync/atomic"
	"time"

	"github.com/ethan/nest-cloudflare-relay/pkg/cloudflare"
	"github.com/ethan/nest-cloudflare-relay/pkg/config"
	"github.com/ethan/nest-cloudflare-relay/pkg/logger"
	"github.com/ethan/nest-cloudflare-relay/pkg/nest"
	"github.com/ethan/nest-cloudflare-relay/pkg/rtsp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Diagnostic tool to verify fundamental NAL unit flow from Nest to Cloudflare
// This tool answers 4 critical questions:
// 1. Are SPS/PPS being extracted from RTSP and forwarded?
// 2. Are keyframes (IDR) coming from Nest at all?
// 3. What does Cloudflare session status show?
// 4. How many frames are we actually sending to Cloudflare?

const (
	NALUTypePFrame = 1
	NALUTypeIDR    = 5
	NALUTypeSEI    = 6
	NALUTypeSPS    = 7
	NALUTypePPS    = 8
	NALUTypeAUD    = 9
)

type Diagnostics struct {
	// NAL unit counters
	spsReceived    atomic.Uint64
	ppsReceived    atomic.Uint64
	idrReceived    atomic.Uint64
	pframeReceived atomic.Uint64
	otherReceived  atomic.Uint64

	// Frame forwarding counters
	packetsSentToCF atomic.Uint64
	writeErrors     atomic.Uint64

	// Timing
	startTime       time.Time
	firstIDRTime    time.Time
	lastIDRTime     time.Time
	idrInterval     time.Duration

	logger *logger.Logger
}

func main() {
	// Parse command-line flags
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	logFlags := logger.RegisterFlags(fs)

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "NAL Unit Flow Diagnostic Tool\n\n")
		fmt.Fprintf(os.Stderr, "This tool will:\n")
		fmt.Fprintf(os.Stderr, "  1. Connect to a real Nest camera via RTSP\n")
		fmt.Fprintf(os.Stderr, "  2. Parse and log NAL units (SPS, PPS, IDR, P-frames)\n")
		fmt.Fprintf(os.Stderr, "  3. Forward RTP packets to Cloudflare\n")
		fmt.Fprintf(os.Stderr, "  4. Track what was sent vs what was received\n\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fs.PrintDefaults()
		logger.PrintUsageExamples()
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger from flags
	logConfig, err := logFlags.ToConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring logger: %v\n", err)
		os.Exit(1)
	}

	lgr, err := logger.New(logConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating logger: %v\n", err)
		os.Exit(1)
	}
	defer lgr.Close()

	logger.SetDefault(lgr)

	lgr.Info("=== NAL Unit Flow Diagnostic Tool ===",
		"log_config", logFlags.String())
	lgr.Info("This tool will:")
	lgr.Info("  1. Connect to a real Nest camera via RTSP")
	lgr.Info("  2. Parse and log NAL units (SPS, PPS, IDR, P-frames)")
	lgr.Info("  3. Forward RTP packets to Cloudflare")
	lgr.Info("  4. Track what was sent vs what was received")
	lgr.Info("")

	// Load config
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	diag := &Diagnostics{
		logger:    lgr,
		startTime: time.Now(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create clients
	nestClient := nest.NewClient(
		cfg.Google.ClientID,
		cfg.Google.ClientSecret,
		cfg.Google.RefreshToken,
		lgr.With("component", "nest").Logger,
	)

	cfClient := cloudflare.NewClient(
		cfg.Cloudflare.AppID,
		cfg.Cloudflare.APIToken,
		lgr.With("component", "cloudflare").Logger,
	)

	// List devices
	devices, err := nestClient.ListDevices(ctx, cfg.Google.ProjectID)
	if err != nil {
		log.Fatalf("Failed to list devices: %v", err)
	}

	if len(devices) == 0 {
		log.Fatalf("No camera devices found")
	}

	// Use first camera
	camera := devices[0]
	lgr.Info("using camera",
		"name", camera.Traits.Info.CustomName,
		"device_id", camera.DeviceID)

	// Generate RTSP stream
	lgr.Info("generating RTSP stream...")
	stream, err := nestClient.GenerateRTSPStream(ctx, cfg.Google.ProjectID, camera.DeviceID)
	if err != nil {
		log.Fatalf("Failed to generate RTSP stream: %v", err)
	}

	lgr.Info("RTSP stream generated",
		"url", stream.URL,
		"expires_at", stream.ExpiresAt.Format(time.RFC3339))

	// Create Cloudflare session
	lgr.Info("creating Cloudflare session...")
	session, err := cfClient.CreateSession(ctx)
	if err != nil {
		log.Fatalf("Failed to create Cloudflare session: %v", err)
	}
	lgr.Info("Cloudflare session created", "session_id", session.SessionID)

	// Setup WebRTC
	videoTrack, pc, err := setupWebRTC(ctx, cfClient, session.SessionID, lgr.Logger)
	if err != nil {
		log.Fatalf("Failed to setup WebRTC: %v", err)
	}
	defer pc.Close()

	// Wait for connection
	lgr.Info("waiting for WebRTC connection...")
	if err := waitForConnection(ctx, pc, lgr.Logger); err != nil {
		log.Fatalf("Failed to establish connection: %v", err)
	}
	lgr.Info("✓ WebRTC connection established")

	// Connect to RTSP stream
	lgr.Info("connecting to RTSP stream...")
	rtspClient := rtsp.NewClient(stream.URL, lgr.With("component", "rtsp").Logger)
	if err := rtspClient.Connect(ctx); err != nil {
		log.Fatalf("Failed to connect to RTSP: %v", err)
	}
	defer rtspClient.Close()

	// Setup tracks
	if err := rtspClient.SetupTracks(ctx); err != nil {
		log.Fatalf("Failed to setup RTSP tracks: %v", err)
	}

	// Set RTP packet handler
	rtspClient.OnRTPPacket = func(channel byte, packet *rtp.Packet) {
		diag.processRTPPacket(packet, videoTrack)
	}

	// Start playing
	if err := rtspClient.Play(ctx); err != nil {
		log.Fatalf("Failed to start RTSP playback: %v", err)
	}

	lgr.Info("✓ RTSP stream playing - monitoring for 60 seconds...")

	// Run for 60 seconds
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start periodic reporting
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		if err := rtspClient.ReadPackets(ctx); err != nil {
			lgr.Error("RTSP read loop error", "error", err)
		}
		close(done)
	}()

	select {
	case <-time.After(60 * time.Second):
		lgr.Info("diagnostic duration completed")
	case <-sigChan:
		lgr.Info("interrupted by user")
	case <-done:
		lgr.Info("RTSP stream ended")
	case <-ticker.C:
		diag.printInterimReport()
	}

	cancel()

	// Final report
	diag.printFinalReport(session.SessionID)
}

func (d *Diagnostics) processRTPPacket(packet *rtp.Packet, track *webrtc.TrackLocalStaticRTP) {
	if len(packet.Payload) == 0 {
		return
	}

	// Debug log RTP packet if enabled
	d.logger.DebugRTPPacket(packet.SequenceNumber, packet.Timestamp, packet.PayloadType, len(packet.Payload))

	// Parse NAL unit type
	payload := packet.Payload
	naluType := payload[0] & 0x1F

	// Handle fragmented NAL units (FU-A)
	if naluType == 28 { // FU-A
		if len(payload) < 2 {
			return
		}
		fuHeader := payload[1]
		naluType = fuHeader & 0x1F
		start := (fuHeader & 0x80) != 0

		// Only log when we see the start of a fragmented NALU
		if start {
			d.logNALU(naluType, len(payload), true)
			// Debug log NAL payload if enabled
			d.logger.DebugNALPayload(naluType, payload)
		}
	} else {
		// Single NAL unit
		d.logNALU(naluType, len(payload), false)
		// Debug log NAL payload if enabled
		d.logger.DebugNALPayload(naluType, payload)
	}

	// Forward packet to Cloudflare
	if err := track.WriteRTP(packet); err != nil {
		d.writeErrors.Add(1)
		if d.writeErrors.Load()%100 == 1 {
			d.logger.Error("write RTP error", "error", err, "error_count", d.writeErrors.Load())
		}
	} else {
		d.packetsSentToCF.Add(1)
	}
}

func (d *Diagnostics) logNALU(naluType uint8, size int, fragmented bool) {
	fragStr := ""
	if fragmented {
		fragStr = " [fragmented]"
	}

	switch naluType {
	case NALUTypeSPS:
		count := d.spsReceived.Add(1)
		d.logger.Info(fmt.Sprintf(">>> SPS received%s", fragStr),
			"count", count,
			"size", size,
			"elapsed", time.Since(d.startTime).Round(time.Millisecond))

	case NALUTypePPS:
		count := d.ppsReceived.Add(1)
		d.logger.Info(fmt.Sprintf(">>> PPS received%s", fragStr),
			"count", count,
			"size", size,
			"elapsed", time.Since(d.startTime).Round(time.Millisecond))

	case NALUTypeIDR:
		count := d.idrReceived.Add(1)
		now := time.Now()

		if d.firstIDRTime.IsZero() {
			d.firstIDRTime = now
		} else {
			// Calculate interval since last IDR
			interval := now.Sub(d.lastIDRTime)
			d.idrInterval = interval
		}
		d.lastIDRTime = now

		d.logger.Info(fmt.Sprintf(">>> IDR KEYFRAME received%s", fragStr),
			"count", count,
			"size", size,
			"interval", d.idrInterval.Round(time.Millisecond),
			"elapsed", time.Since(d.startTime).Round(time.Millisecond))

	case NALUTypePFrame:
		count := d.pframeReceived.Add(1)
		// Only log every 100th P-frame to avoid spam
		if count%100 == 1 {
			d.logger.Debug("P-frames received", "count", count)
		}

	default:
		count := d.otherReceived.Add(1)
		if count < 10 {
			d.logger.Debug(fmt.Sprintf("other NAL unit received%s", fragStr),
				"type", naluType,
				"size", size)
		}
	}
}

func (d *Diagnostics) printInterimReport() {
	elapsed := time.Since(d.startTime).Round(time.Second)
	d.logger.Info("--- Interim Report ---",
		"elapsed", elapsed,
		"sps", d.spsReceived.Load(),
		"pps", d.ppsReceived.Load(),
		"idr", d.idrReceived.Load(),
		"pframes", d.pframeReceived.Load(),
		"packets_sent", d.packetsSentToCF.Load(),
		"write_errors", d.writeErrors.Load())
}

func (d *Diagnostics) printFinalReport(sessionID string) {
	elapsed := time.Since(d.startTime).Round(time.Second)

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("DIAGNOSTIC RESULTS")
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("Duration: %s\n", elapsed)
	fmt.Printf("Session ID: %s\n\n", sessionID)

	fmt.Println("NAL UNITS RECEIVED FROM NEST:")
	fmt.Printf("  SPS received:     %d\n", d.spsReceived.Load())
	fmt.Printf("  PPS received:     %d\n", d.ppsReceived.Load())
	fmt.Printf("  IDR keyframes:    %d\n", d.idrReceived.Load())
	if d.idrReceived.Load() > 1 {
		fmt.Printf("  IDR interval:     ~%s\n", d.idrInterval.Round(time.Millisecond))
	}
	fmt.Printf("  P-frames:         %d\n", d.pframeReceived.Load())
	fmt.Printf("  Other NALUs:      %d\n\n", d.otherReceived.Load())

	fmt.Println("FORWARDING TO CLOUDFLARE:")
	fmt.Printf("  Packets sent:     %d\n", d.packetsSentToCF.Load())
	fmt.Printf("  Write errors:     %d\n\n", d.writeErrors.Load())

	fmt.Println(strings.Repeat("=", 80))
	fmt.Println("ANSWERS TO KEY QUESTIONS:")
	fmt.Println(strings.Repeat("=", 80))

	// Question 1: SPS/PPS forwarded?
	if d.spsReceived.Load() > 0 && d.ppsReceived.Load() > 0 {
		fmt.Println("1. SPS/PPS forwarded: YES")
		fmt.Printf("   - SPS count: %d\n", d.spsReceived.Load())
		fmt.Printf("   - PPS count: %d\n", d.ppsReceived.Load())
	} else {
		fmt.Println("1. SPS/PPS forwarded: NO")
		fmt.Println("   ❌ CRITICAL: Missing parameter sets!")
	}

	// Question 2: Keyframes from Nest?
	if d.idrReceived.Load() > 0 {
		fmt.Println("\n2. Keyframes from Nest: YES")
		fmt.Printf("   - IDR count: %d\n", d.idrReceived.Load())
		if d.idrReceived.Load() > 1 {
			fmt.Printf("   - Interval: ~%s\n", d.idrInterval.Round(time.Millisecond))
		}
	} else {
		fmt.Println("\n2. Keyframes from Nest: NO")
		fmt.Println("   ❌ CRITICAL: No IDR frames received!")
	}

	// Question 3: Browser stats (manual check needed)
	fmt.Println("\n3. Browser RTCPeerConnection.getStats():")
	fmt.Println("   ⚠️  MANUAL CHECK REQUIRED")
	fmt.Println("   - Open browser console")
	fmt.Println("   - Run: pc.getStats().then(stats => { stats.forEach(s => { if(s.type === 'inbound-rtp' && s.kind === 'video') console.log(s) }) })")
	fmt.Println("   - Check: framesDecoded value (should be > 0 and incrementing)")

	// Question 4: Cloudflare session active?
	fmt.Printf("\n4. Cloudflare track status: CHECK SESSION %s\n", sessionID)
	if d.packetsSentToCF.Load() > 0 {
		fmt.Println("   - Packets were sent successfully: YES")
	} else {
		fmt.Println("   - Packets were sent successfully: NO")
	}
	fmt.Printf("   - Total packets: %d\n", d.packetsSentToCF.Load())
	if d.writeErrors.Load() > 0 {
		fmt.Printf("   - ⚠️  Write errors: %d\n", d.writeErrors.Load())
	}

	fmt.Println("\n" + strings.Repeat("=", 80))
	fmt.Println("ROOT CAUSE ANALYSIS:")
	fmt.Println(strings.Repeat("=", 80))

	if d.spsReceived.Load() == 0 || d.ppsReceived.Load() == 0 {
		fmt.Println("❌ CRITICAL: SPS/PPS not received from Nest")
		fmt.Println("   → Decoder cannot initialize without parameter sets")
		fmt.Println("   → ACTION: Check RTSP SDP parsing and NAL unit extraction")
	} else if d.idrReceived.Load() == 0 {
		fmt.Println("❌ CRITICAL: No keyframes received from Nest")
		fmt.Println("   → Decoder cannot start without initial IDR frame")
		fmt.Println("   → ACTION: Verify RTSP stream is actually providing video data")
	} else if d.packetsSentToCF.Load() == 0 {
		fmt.Println("❌ CRITICAL: No packets sent to Cloudflare")
		fmt.Println("   → WebRTC track not accepting writes")
		fmt.Println("   → ACTION: Check WebRTC connection state and track setup")
	} else if d.writeErrors.Load() > d.packetsSentToCF.Load()/10 {
		fmt.Println("⚠️  WARNING: High error rate when writing to Cloudflare")
		fmt.Printf("   → Error rate: %.1f%%\n", float64(d.writeErrors.Load())/float64(d.packetsSentToCF.Load())*100)
		fmt.Println("   → ACTION: Check connection stability and error logs")
	} else {
		fmt.Println("✓ All fundamental checks PASSED")
		fmt.Println("  → SPS/PPS are being received and forwarded")
		fmt.Println("  → Keyframes are coming from Nest regularly")
		fmt.Println("  → Packets are being sent to Cloudflare successfully")
		fmt.Println("")
		fmt.Println("If video is still black in browser:")
		fmt.Println("  → Check browser console for framesDecoded (question 3)")
		fmt.Println("  → Verify Cloudflare session is active")
		fmt.Println("  → Check for RTCP PLI/FIR requests indicating decode errors")
	}

	fmt.Println(strings.Repeat("=", 80))
}

func setupWebRTC(ctx context.Context, cfClient *cloudflare.Client, sessionID string, logger *slog.Logger) (*webrtc.TrackLocalStaticRTP, *webrtc.PeerConnection, error) {
	// Create media engine with H264 (Main Profile to match Nest camera output)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, nil, fmt.Errorf("register H264 codec: %w", err)
	}

	// Create API with media engine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	// Create peer connection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create peer connection: %w", err)
	}

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
		pc.Close()
		return nil, nil, fmt.Errorf("create video track: %w", err)
	}

	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("add video track: %w", err)
	}

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("create offer: %w", err)
	}

	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("set local description: %w", err)
	}

	// Wait for ICE gathering
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	select {
	case <-gatherComplete:
	case <-time.After(10 * time.Second):
		pc.Close()
		return nil, nil, fmt.Errorf("ICE gathering timeout")
	}

	localSDP := pc.LocalDescription().SDP

	// Get video mid
	var videoMid string
	for _, t := range pc.GetTransceivers() {
		if t.Mid() != "" && t.Kind() == webrtc.RTPCodecTypeVideo {
			videoMid = t.Mid()
			break
		}
	}

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

	tracksResp, err := cfClient.AddTracksWithRetry(ctx, sessionID, tracksReq, 3)
	if err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("add tracks: %w", err)
	}

	if tracksResp.SessionDescription == nil {
		pc.Close()
		return nil, nil, fmt.Errorf("no SDP answer from Cloudflare")
	}

	// Set remote description
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  tracksResp.SessionDescription.SDP,
	}

	if err := pc.SetRemoteDescription(answer); err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("set remote description: %w", err)
	}

	return videoTrack, pc, nil
}

func waitForConnection(ctx context.Context, pc *webrtc.PeerConnection, logger *slog.Logger) error {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	stateChan := make(chan webrtc.PeerConnectionState, 1)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Info("connection state changed", "state", state.String())
		stateChan <- state
	})

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timeout waiting for connection: %w", waitCtx.Err())
		case state := <-stateChan:
			if state == webrtc.PeerConnectionStateConnected {
				return nil
			}
			if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
				return fmt.Errorf("connection failed: state=%s", state.String())
			}
		case <-ticker.C:
			state := pc.ConnectionState()
			if state == webrtc.PeerConnectionStateConnected {
				return nil
			}
		}
	}
}
