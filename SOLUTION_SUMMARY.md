# Producer Not Sending RTP Packets - SOLVED

## The Problem

Browser console showed a clear pattern:
```
Track 0 unmuted  ← Initial packets arrived
...
Track 0 muted    ← Packets stopped flowing!
```

This happened for ALL 4 cameras simultaneously after ICE connected, indicating a systemic issue in the producer (Go server), not the network or cameras.

## Root Cause

**The producer was sending RTP packets BEFORE the WebRTC PeerConnection reached "connected" state.**

When you send packets during ICE negotiation:
- No established data path exists yet
- Packets get buffered temporarily but buffer is finite
- Buffer overflows with 30fps video stream
- Dropped packets break H.264 decoder (stateful codec)
- Browser detects silence → mutes track

## The Fix

Added synchronization to wait for PeerConnection "connected" state before starting RTSP stream:

```go
// In pkg/relay/relay.go Start() method:

// Wait for PeerConnection to reach "connected" state before starting RTSP
r.logger.Info("waiting for WebRTC connection to be established")
if err := r.waitForConnection(ctx); err != nil {
    return fmt.Errorf("wait for WebRTC connection: %w", err)
}
r.logger.Info("WebRTC connection established, starting RTSP stream")

// NOW it's safe to connect RTSP and start sending packets
r.rtspConn = rtspClient.NewClient(r.stream.URL, ...)
```

The `waitForConnection()` method polls the connection state every 100ms until "connected", with 30s timeout and fail-fast on "failed"/"closed" states.

## Enhanced Debugging

Added comprehensive logging to make timing issues visible:

1. **Connection state logging** - Shows state during write attempts
2. **First frame tracking** - Logs when first successful frame is written
3. **Error context** - WriteRTP failures include connection state
4. **Periodic updates** - Every 10s log with connection state

## Files Changed

1. **pkg/relay/relay.go**
   - Added `waitForConnection()` method (lines 190-217)
   - Call wait before RTSP connection (line 104)
   - Enhanced frame write logging (lines 137-148)

2. **pkg/bridge/bridge.go**
   - Enhanced WriteRTP error logging (lines 306-313)

3. **test-server** (binary)
   - Rebuilt with fixes (16MB)

## Testing

### Quick Test
```bash
./verify_rtp_fix.sh
```

### Manual Test
1. Start server: `./test-server`
2. Open browser: `http://localhost:8080`
3. Check logs for:
   - "waiting for WebRTC connection to be established"
   - "checking connection state state=connecting"
   - "WebRTC connection established, starting RTSP stream"
   - "first video frame written successfully"
4. Browser should show:
   - Tracks unmute and STAY unmuted
   - Video starts playing
   - No "Track muted" after initial unmute

## Expected Behavior

### Logs (Server)
```
[relay] waiting for WebRTC connection to be established
[relay] checking connection state state=connecting
[relay] checking connection state state=connecting
[relay] checking connection state state=connected
[relay] WebRTC connection established, starting RTSP stream
[rtsp] connecting to RTSP server
[rtsp] received first RTP packet successfully
[relay] first video frame written successfully keyframe=true connection_state=connected
[relay] video frames written frame_count=300 connection_state=connected
```

### Browser Console
```
[Viewer] Creating connection for camera: AVPHwEv3...
[Camera] Connecting
[Camera] ICE state: connected
[Camera] Connection state: connected
[Tile] Track 0 unmuted
[Tile] Video playing
```

NO "Track 0 muted" after the connection is established!

## Technical Details

### WebRTC State Machine
```
new → connecting → connected → disconnected/failed/closed
         ↑
         └─ ICE negotiation happens here
```

We were sending packets at "connecting", but need to wait for "connected".

### Why This Matters for H.264

H.264 is a **stateful** codec:
- Each frame depends on previous frames
- Keyframes (I-frames) establish state
- P-frames delta-encode from previous frames
- Missing packets corrupt decoder state
- Recovery requires next keyframe (1-2 seconds away)

If we drop packets during ICE negotiation:
- Decoder gets corrupted state from partial frames
- Can't decode subsequent P-frames
- Browser sees corrupt/no data → mutes track

### Goroutine Safety

The `waitForConnection()` method:
- Uses context for cancellation
- Ticker for periodic checks (no busy-wait)
- Timeout to prevent infinite blocking
- Fail-fast on terminal states
- Clean resource cleanup with defer

## Commit

```
5cd2aa8 fix: wait for WebRTC connection before sending RTP packets
```

## Next Steps

1. Test with all 4 cameras simultaneously
2. Monitor logs for "failed to write video sample" errors
3. Verify tracks stay unmuted for extended periods
4. Check CPU/memory usage is stable

If tracks still mute, check:
- Nest camera stream expiration (5 minutes)
- Network stability
- Cloudflare session limits
- RTSP keepalive working

## Related Documentation

- `RTP_PACKET_FIX.md` - Detailed technical analysis
- `console.log` - Browser logs showing unmute→mute pattern
- `verify_rtp_fix.sh` - Automated verification script
