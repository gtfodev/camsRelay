# RTP Flow Diagnostic Tool

## Quick Start

```bash
# From project root
go run ./cmd/diagnose

# Or build first
go build -o diagnose ./cmd/diagnose
./diagnose
```

## Purpose

Standalone diagnostic to identify where RTP video flow breaks between your Go server and Cloudflare Calls.

## What It Tests

1. **Cloudflare session creation** - Same as production
2. **WebRTC connection** - ICE/DTLS handshake
3. **RTP packet sending** - H.264 test pattern at 30fps
4. **RTCP feedback reception** - PLI, FIR, NACK from Cloudflare
5. **Connection stability** - 30 second test run

## Expected Output

### If RTCP is Missing (Current State)

```
❌ WARNING: No RTCP feedback received from Cloudflare
   - This could indicate RTCP read loop not working

ROOT CAUSE HYPOTHESIS:
  → No RTCP received - RTCP read loop may not be set up
  → ACTION NEEDED: Add RTCP reader to main app's bridge.go
```

### If Keyframe is Missing

```
!!! PLI RECEIVED !!!
    media_ssrc=123456
    description: Cloudflare is requesting a keyframe

⚠️  IMPORTANT: Cloudflare sent 5 PLI requests for keyframes
   - This means Cloudflare received packets but couldn't decode them
   - Likely cause: Missing keyframe or SPS/PPS
```

### If Everything Works

```
✓ RTP sending appears to be working (900 packets)
✓ RTCP feedback is being received (6 packets)
✓ Connection and flow appear healthy
```

## What Each Metric Means

- **RTP packets sent**: Should be ~900 for 30 seconds
- **PLI received**: Keyframe requests (should be 0 if working)
- **Receiver Reports**: Normal feedback (good sign)
- **NACK**: Packet loss detected (investigate if high)

## Integration with Main App

After identifying issues, apply fixes to:

1. `pkg/bridge/bridge.go` - Add RTCP reader
2. `pkg/relay/relay.go` - Add keyframe handling
3. Test with web viewer at `http://localhost:8080`

## Documentation

- Usage guide: `/DIAGNOSTIC_TOOL_GUIDE.md`
- Root cause analysis: `/BLACK_SCREEN_ROOT_CAUSE.md`
- Fix implementation: `/BRIDGE_RTCP_FIX.md`

## Prerequisites

- `.env` file with Cloudflare credentials
- Network access to Cloudflare Calls API
- Go 1.21 or later

## Duration

30 seconds (interruptible with Ctrl+C)

## Cleanup

Session is automatically closed when tool exits.
