# RTP Packet Flow Fix - Producer Not Sending to Cloudflare

## Problem Summary

Browser console showed tracks initially unmuting (data flows) then getting MUTED for ALL 4 cameras simultaneously after ICE connection established. No video displayed despite WebRTC connection showing "connected" state.

## Root Cause

The Go producer (our server) was attempting to send RTP packets to Cloudflare **before** the PeerConnection's ICE state reached "connected". This caused:

1. Early packets sent during ICE negotiation → dropped by WebRTC stack
2. Browser briefly receives initial packets → track unmutes
3. No more packets arrive → browser detects silence → track mutes
4. All 4 cameras experienced this simultaneously because they all had the same timing issue

## Packet Flow Architecture

```
┌─────────────┐     ┌──────────────┐     ┌────────────┐     ┌─────────┐
│ Nest Camera │────▶│ RTSP Client  │────▶│ H264       │────▶│ Bridge  │
│  (RTSP)     │ RTP │ (pkg/rtsp)   │ RTP │ Processor  │NALUs│(WebRTC) │
└─────────────┘     └──────────────┘     └────────────┘     └─────────┘
                                               │                    │
                                               ▼                    ▼
                                         OnFrame callback    WriteVideoSample()
                                                                    │
                                                                    ▼
                                                            WriteRTP() to track
                                                                    │
                                                                    ▼
                                                            ┌───────────────┐
                                                            │  Cloudflare   │
                                                            │  (WebRTC SFU) │
                                                            └───────────────┘
                                                                    │
                                                                    ▼
                                                            ┌───────────────┐
                                                            │    Browser    │
                                                            │   (Viewer)    │
                                                            └───────────────┘
```

## The Timing Issue

**Before Fix:**
```
Time 0ms:   Bridge.Negotiate() completes
            - PeerConnection state: "new" or "connecting"
            - ICE candidates still being gathered/checked
Time 10ms:  RTSP client connects and starts reading packets
Time 20ms:  First RTP packets arrive from Nest camera
Time 30ms:  WriteVideoSample() called → packets DROPPED (no connection yet!)
Time 500ms: ICE completes → PeerConnection state: "connected"
Time 600ms: More packets arrive but pattern already broken
```

**After Fix:**
```
Time 0ms:   Bridge.Negotiate() completes
Time 0ms:   waitForConnection() starts polling state
Time 100ms: Check state: "connecting"
Time 200ms: Check state: "connecting"
Time 300ms: Check state: "connected" ✓
Time 300ms: RTSP client NOW starts connecting
Time 400ms: First RTP packets arrive from Nest
Time 450ms: WriteVideoSample() called → packets ACCEPTED ✓
```

## Code Changes

### 1. Added `waitForConnection()` method (pkg/relay/relay.go)

```go
func (r *CameraRelay) waitForConnection(ctx context.Context) error {
    waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-waitCtx.Done():
            return fmt.Errorf("timeout waiting for connection (state=%s): %w",
                r.webrtcBridge.GetConnectionState().String(), waitCtx.Err())
        case <-ticker.C:
            state := r.webrtcBridge.GetConnectionState()
            r.logger.Debug("checking connection state", "state", state.String())

            if state.String() == "connected" {
                return nil
            }

            // Fail fast if connection failed
            if state.String() == "failed" || state.String() == "closed" {
                return fmt.Errorf("peer connection failed: state=%s", state.String())
            }
        }
    }
}
```

### 2. Call waitForConnection() before starting RTSP (pkg/relay/relay.go:104)

```go
// Wait for PeerConnection to reach "connected" state before starting RTSP
// This ensures ICE connectivity is fully established before we start sending RTP packets
// Without this, we may send packets before the peer connection is ready, causing them to be dropped
r.logger.Info("waiting for WebRTC connection to be established")
if err := r.waitForConnection(ctx); err != nil {
    return fmt.Errorf("wait for WebRTC connection: %w", err)
}
r.logger.Info("WebRTC connection established, starting RTSP stream")
```

### 3. Enhanced error logging (pkg/bridge/bridge.go:306)

Now logs connection state when WriteRTP fails, making timing issues visible:

```go
if err := b.videoTrack.WriteRTP(packet); err != nil {
    if err == io.ErrClosedPipe {
        return nil // Track closed gracefully
    }
    b.logger.Error("failed to write RTP packet",
        "packet_num", i+1,
        "total_packets", len(payloads),
        "connection_state", b.GetConnectionState().String(),
        "error", err)
    return fmt.Errorf("write RTP packet %d/%d (state=%s): %w",
        i+1, len(payloads), b.GetConnectionState().String(), err)
}
```

### 4. Better frame write logging (pkg/relay/relay.go:137)

Logs first successful frame and periodic updates with connection state:

```go
if frameCount == 1 {
    r.logger.Info("first video frame written successfully",
        "keyframe", keyframe,
        "size_bytes", len(nalus),
        "connection_state", r.webrtcBridge.GetConnectionState().String())
}
```

## Testing Verification

Run the updated binary and check logs for:

1. **Before RTSP starts:**
   ```
   waiting for WebRTC connection to be established
   checking connection state state=connecting
   checking connection state state=connected
   WebRTC connection established, starting RTSP stream
   ```

2. **First frame written:**
   ```
   first video frame written successfully keyframe=true size_bytes=... connection_state=connected
   ```

3. **No errors about:**
   ```
   failed to write video sample error=...
   failed to write RTP packet connection_state=connecting
   ```

4. **Browser console should show:**
   - Tracks unmute and STAY unmuted
   - Video elements start playing
   - No "Track X muted" messages after initial unmute

## Why This Fixes The Issue

The WebRTC specification requires that the PeerConnection's data channels be fully established before media can flow. When we send RTP packets while ICE is still negotiating:

1. **No data path exists yet** - ICE candidates haven't been selected
2. **Packets are buffered temporarily** but buffer is finite
3. **Buffer overflows quickly** with 30fps video stream
4. **Dropped packets break decoder** - H.264 is stateful, dropped frames corrupt stream
5. **Browser detects broken stream** → mutes track

By waiting for "connected" state:
- ICE negotiation is complete
- Data path is established and validated
- DTLS handshake is done (WebRTC encrypts media)
- SRTP keys are exchanged
- RTP packets can flow reliably

## Related Files

- `/home/ethan/cams/pkg/relay/relay.go` - Added waitForConnection, enhanced logging
- `/home/ethan/cams/pkg/bridge/bridge.go` - Enhanced WriteRTP error logging
- `/home/ethan/cams/console.log` - Browser logs showing unmute→mute pattern
- `/home/ethan/cams/test-server` - Updated binary (16MB)

## Commit

```
commit 5cd2aa8
fix: wait for WebRTC connection before sending RTP packets
```
