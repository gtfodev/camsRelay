# Log Interpretation Guide

## Quick Start

1. Run: `./cams-relay`
2. Open: http://localhost:8080
3. Open Console: F12 → Console tab
4. Observe log flow for each camera

## Log Analysis Decision Tree

### Step 1: Check Track Pull

Look for:
```
[Camera XXX] Received tracks response: {...}
```

**✓ If you see valid response with `sessionDescription.sdp`**:
- API communication works
- Backend proxy works
- Cloudflare API works
→ Go to Step 2

**✗ If you see error or invalid structure**:
- API call failed
- Check: Network tab for actual HTTP response
- Check: Backend logs for proxy errors
- Compare: Response structure with curl test
→ **Root cause**: Backend proxy issue

### Step 2: Check SDP Extraction

Look for:
```
[Camera XXX] Extracted SDP offer, length: XXXX
```

**✓ If you see non-zero length**:
- SDP offer parsed successfully
- Response structure matches expected format
→ Go to Step 3

**✗ If you see error "No SDP offer"**:
- Response structure doesn't match
- Check previous log line for actual response
- Compare: `data.sessionDescription` exists and has `sdp` field
→ **Root cause**: Response format mismatch

### Step 3: Check Answer Creation

Look for:
```
[Camera XXX] Set local description, answer SDP length: XXXX
```

**✓ If you see non-zero length**:
- Browser successfully parsed offer
- Answer created successfully
→ Go to Step 4

**✗ If creation fails with error**:
- Invalid SDP format
- Browser rejected offer
- Check: Full error message
→ **Root cause**: Malformed SDP offer

### Step 4: Check Renegotiation

Look for:
```
[Camera XXX] Renegotiate response: {}
[Camera XXX] Renegotiation complete
```

**✓ If you see both lines**:
- Answer sent to Cloudflare
- Cloudflare accepted answer
→ Go to Step 5

**✗ If you see error**:
- Renegotiation API failed
- Check: Network tab for actual response
- Check: Backend logs
→ **Root cause**: Renegotiation API failure

### Step 5: Check ICE Connection

Look for progression:
```
[Camera XXX] ICE state: new
[Camera XXX] ICE candidate: candidate:...
[Camera XXX] ICE candidate: candidate:...
[Camera XXX] ICE gathering complete
[Camera XXX] ICE state: checking
[Camera XXX] ICE state: connected
```

**✓ If progresses to 'connected'**:
- ICE negotiation successful
- Network path established
→ Go to Step 6

**✗ If stuck at 'new'**:
- ICE candidates not generating
- Check: STUN server configuration
→ **Root cause**: STUN server unreachable

**✗ If stuck at 'checking'**:
- Candidates generated but connection failing
- All candidate pairs failing
- Check: Firewall blocking UDP
- Check: NAT traversal issues
→ **Root cause**: Network connectivity

**✗ If goes to 'failed'**:
- All ICE checks failed
- No valid candidate pairs
→ **Root cause**: Complete ICE failure

### Step 6: Check Track Reception

Look for:
```
[Camera XXX] Connection state: connected
[Camera XXX] Received track: video
```

**✓ If you see track received**:
- Everything works!
- Video should appear
→ **SUCCESS**

**✗ If no track received**:
- Connection established but no media
- Check: Backend logs for packet flow
- Check: Are cameras actually streaming?
- Check: Is Cloudflare forwarding media?
→ **Root cause**: Media flow issue

## Common Patterns

### Pattern: "Infinite Spinning"

Usually means stuck at one of these steps:
- Step 5 (ICE connection)
- Step 6 (waiting for track)

Check where in the log flow it stops progressing.

### Pattern: "Immediate Failure"

Usually means:
- Step 1 or 2 (API/parsing)
- Step 3 (malformed SDP)

Check for error messages early in flow.

### Pattern: "Connects Then Fails"

Usually means:
- Step 5 succeeds briefly then ICE goes to 'failed'
- Network instability
- Check ICE state changes

## Expected Timing

Normal flow should complete in:
- Steps 1-4: < 2 seconds
- Step 5 (ICE): 2-10 seconds
- Step 6 (track): < 1 second after connected

If any step takes significantly longer, that's where the problem is.

## Side-by-Side Comparison

Run curl test in one terminal while watching browser console:

**Curl shows**:
```json
{
  "sessionDescription": {
    "sdp": "v=0\r\no=...",
    "type": "offer"
  },
  "tracks": [...],
  "requiresImmediateRenegotiation": true
}
```

**Browser should show same**:
```
[Camera XXX] Received tracks response: {sessionDescription: {sdp: "v=0...", type: "offer"}, ...}
```

If different → Backend serving different response to browser vs curl.

## Next Action Matrix

| Failure Point | Root Cause | Next Action |
|---------------|------------|-------------|
| Step 1 | API call | Check network tab, backend logs |
| Step 2 | Response parse | Check response structure |
| Step 3 | SDP parse | Validate SDP format |
| Step 4 | Renegotiate | Check backend/CF API logs |
| Step 5 (new) | STUN | Check STUN server config |
| Step 5 (checking) | Network | Check firewall/NAT |
| Step 5 (failed) | ICE | Check ICE logs, candidates |
| Step 6 | Media flow | Check backend camera streams |

## Debugging Commands

While browser is running:

```bash
# Check backend logs
journalctl -f -u cams-relay

# Check network connectivity
nc -vz stun.l.google.com 19302

# Check if cameras are streaming
curl http://localhost:8080/api/cameras | jq

# Check firewall rules
sudo iptables -L -n | grep -i udp
```

## Success Indicators

All 18 cameras should show:
- Complete log flow through all 6 steps
- ICE state: connected
- Track received: video
- Video element playing
- No errors in console

## Failure Indicators

Any camera showing:
- Stuck at any step
- ICE state: failed
- No track received
- Errors in console
- Tile shows "connecting" forever
