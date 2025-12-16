# RTP Flow Diagnosis and Critical Issue

## Summary

After analyzing the codebase, I've identified **the root cause of the black video issue**: **The bridge has no RTCP feedback handler.**

## Critical Finding

### Missing RTCP Handler in `pkg/bridge/bridge.go`

The current implementation:
```go
// In pkg/bridge/bridge.go
if _, err = pc.AddTrack(videoTrack); err != nil {
    return fmt.Errorf("add video track: %w", err)
}
```

**Problem**: The `RTPSender` returned by `AddTrack()` is discarded. This means:
1. No RTCP read loop exists
2. PLI/FIR requests from Cloudflare are never read
3. The server has no idea Cloudflare is requesting keyframes

### What Cloudflare Documentation Says

From the user's research:
- Browser joins stream → sends PLI (Picture Loss Indication)
- PLI = "I need a keyframe to start decoding"
- If server ignores PLI → browser gets RTP headers (unmute) but no decodable video (black screen)

### The Symptom Matches Perfectly

```
Track unmuted → brief data → Track muted
```

This sequence means:
1. Browser receives RTP packets (unmute event fires)
2. Browser cannot decode (no keyframe)
3. After timeout, browser gives up (mute event)

## How to Fix

### 1. Add RTCP Reader to Bridge

In `pkg/bridge/bridge.go`, replace:
```go
if _, err = pc.AddTrack(videoTrack); err != nil {
    return fmt.Errorf("add video track: %w", err)
}
```

With:
```go
rtpSender, err := pc.AddTrack(videoTrack)
if err != nil {
    return fmt.Errorf("add video track: %w", err)
}

// Start RTCP feedback handler
b.wg.Add(1)
go b.readRTCP(rtpSender)
```

### 2. Implement RTCP Handler

Add to `pkg/bridge/bridge.go`:
```go
// readRTCP handles RTCP feedback from Cloudflare
// This is CRITICAL for responding to PLI/FIR keyframe requests
func (b *Bridge) readRTCP(rtpSender *webrtc.RTPSender) {
    defer b.wg.Done()

    b.logger.Info("RTCP read loop started")

    for {
        packets, _, err := rtpSender.ReadRTCP()
        if err != nil {
            if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
                b.logger.Info("RTCP read loop ended")
                return
            }
            b.logger.Error("RTCP read error", "error", err)
            continue
        }

        for _, packet := range packets {
            switch pkt := packet.(type) {
            case *rtcp.PictureLossIndication:
                b.logger.Info("PLI received - keyframe requested",
                    "media_ssrc", pkt.MediaSSRC)
                // TODO: Signal RTSP client to request keyframe from camera
                // For Nest cameras: send RTCP PLI upstream to camera

            case *rtcp.FullIntraRequest:
                b.logger.Info("FIR received - full refresh requested")
                // TODO: Same as PLI

            case *rtcp.TransportLayerNack:
                b.logger.Debug("NACK received",
                    "media_ssrc", pkt.MediaSSRC,
                    "nacks", len(pkt.Nacks))
                // Packet loss detected - may need to retransmit or request keyframe

            case *rtcp.ReceiverReport:
                // Statistics - useful for monitoring
                for _, report := range pkt.Reports {
                    if report.FractionLost > 10 {
                        b.logger.Warn("high packet loss detected",
                            "fraction_lost", report.FractionLost,
                            "total_lost", report.TotalLost)
                    }
                }
            }
        }
    }
}
```

### 3. Keyframe Request Propagation

The RTCP handler should trigger keyframe requests upstream to the RTSP source.

For Nest cameras, this means:
1. Bridge receives PLI from Cloudflare
2. Bridge sends RTCP PLI to Nest camera via RTSP
3. Nest camera sends IDR frame
4. Bridge forwards IDR frame to Cloudflare
5. Browser can decode

**Implementation in bridge:**
```go
// Add to Bridge struct
OnKeyframeRequest func() error  // Callback to request keyframe from source

// In PLI handler:
if b.OnKeyframeRequest != nil {
    if err := b.OnKeyframeRequest(); err != nil {
        b.logger.Error("failed to request keyframe", "error", err)
    }
}
```

**Implementation in relay:**
```go
// In pkg/relay/relay.go, when creating bridge:
webrtcBridge.OnKeyframeRequest = func() error {
    // Send RTCP PLI to RTSP source
    return r.rtspConn.RequestKeyframe()
}
```

### 4. RTSP Client Enhancement

Add to `pkg/rtsp/client.go`:
```go
func (c *Client) RequestKeyframe() error {
    // Send RTCP PLI to camera
    // Nest cameras should respond with IDR frame

    c.logger.Info("requesting keyframe from camera via RTCP PLI")

    // Build RTCP PLI packet and send via interleaved channel
    // Channel 1 (odd) is RTCP for video

    return nil // TODO: Implement RTCP sending
}
```

## Testing with Diagnostic Tool

Run the diagnostic:
```bash
go run ./cmd/diagnose
```

Expected output if RTCP is missing:
```
❌ WARNING: No RTCP feedback received from Cloudflare
   - This could indicate RTCP read loop not working
```

Expected output if RTCP works but keyframes missing:
```
⚠️  IMPORTANT: Cloudflare sent 5 PLI requests for keyframes
   - This means Cloudflare received packets but couldn't decode them
   - Likely cause: Missing keyframe or SPS/PPS
```

## Root Cause Chain

```
User joins stream in browser
    ↓
Browser sends PLI to Cloudflare SFU
    ↓
Cloudflare forwards PLI to Go server
    ↓
Go server has NO RTCP reader → PLI is NEVER READ
    ↓
No keyframe sent in response
    ↓
Browser receives RTP packets (unmute) but cannot decode
    ↓
After timeout, browser gives up → track muted
    ↓
Video stays black
```

## Solution Summary

1. **Add RTCP reader** to bridge (highest priority)
2. **Implement PLI/FIR handlers** to log when keyframes are requested
3. **Propagate keyframe requests** upstream to RTSP source
4. **Send initial keyframe** on connection (SPS+PPS+IDR as first frames)

## Files to Modify

1. `pkg/bridge/bridge.go` - Add RTCP reader and handlers
2. `pkg/relay/relay.go` - Wire up keyframe request callback
3. `pkg/rtsp/client.go` - Add RequestKeyframe() method

## Expected Behavior After Fix

```
Browser joins
    ↓
Browser sends PLI
    ↓
Go server RECEIVES PLI (logged)
    ↓
Go server requests keyframe from Nest camera
    ↓
Nest camera sends IDR frame
    ↓
Go server forwards to Cloudflare
    ↓
Browser decodes and displays video
    ↓
SUCCESS: Video visible
```

## Diagnostic Tool Results

The diagnostic tool (`cmd/diagnose/main.go`) will:
- ✅ Prove whether RTCP can be received (current code: NO)
- ✅ Show exactly when PLI requests arrive
- ✅ Verify RTP sending works
- ✅ Identify timing of "unmute → mute" cycle
- ✅ Confirm root cause hypothesis

Run it before and after implementing the fix to verify improvement.
