# Viewer Debug Summary

## Changes Made

### Added Comprehensive Debug Logging to viewer.js

**Location**: `/home/ethan/cams/pkg/api/web/static/js/viewer.js`

**What was added**:

1. **Track Pulling Logs** (pullRemoteTracks method):
   - Log request payload being sent to `/api/cf/sessions/{id}/tracks/new`
   - Log complete response received from backend
   - Log extracted SDP offer length
   - Log error if response structure is invalid

2. **Renegotiation Logs** (renegotiate method):
   - Log answer SDP length being sent
   - Log complete renegotiate response
   - Log success confirmation

3. **WebRTC Connection Logs** (connect method):
   - Log when setting remote description with SDP length
   - Log answer creation
   - Log local description setting with answer SDP length
   - Log ICE candidates as they're generated
   - Log ICE gathering completion

4. **ICE Candidate Handler**:
   - New `onicecandidate` handler to see ICE candidate generation
   - Logs each candidate and when gathering completes

## How to Test

1. **Rebuild** (already done):
   ```bash
   go build -o cams-relay cmd/multi-relay/main.go
   ```

2. **Start relay**:
   ```bash
   ./cams-relay
   ```

3. **Open browser**:
   - Navigate to http://localhost:8080
   - Open Developer Console (F12)
   - Watch console output

## What to Look For

### Expected Log Flow (per camera):

```
[Camera XXX] Connecting
[Camera XXX] Created viewer session: abc123
[Camera XXX] Pulling tracks: {url: ..., tracks: [...]}
[Camera XXX] Received tracks response: {sessionDescription: {...}, ...}
[Camera XXX] Extracted SDP offer, length: 1234
[Camera XXX] Received offer from Cloudflare
[Camera XXX] Setting remote description, SDP length: 1234
[Camera XXX] Creating answer
[Camera XXX] ICE state: new
[Camera XXX] ICE candidate: candidate:...
[Camera XXX] ICE candidate: candidate:...
[Camera XXX] ICE gathering complete
[Camera XXX] Set local description, answer SDP length: 5678
[Camera XXX] Sending renegotiate request, answer SDP length: 5678
[Camera XXX] Renegotiate response: {}
[Camera XXX] Renegotiation complete
[Camera XXX] WebRTC negotiation complete
[Camera XXX] ICE state: checking
[Camera XXX] Connection state: connecting
[Camera XXX] ICE state: connected
[Camera XXX] Connection state: connected
[Camera XXX] Received track: video
```

### Diagnostic Checkpoints

**Checkpoint 1: Track Pull Response**
```javascript
[Camera XXX] Received tracks response: {...}
```
- **Should contain**: `sessionDescription` object with `sdp` and `type` fields
- **If missing**: Backend proxy isn't forwarding Cloudflare response correctly
- **Compare with curl**: Your curl test returned valid SDP, so this should work

**Checkpoint 2: SDP Extraction**
```javascript
[Camera XXX] Extracted SDP offer, length: 1234
```
- **Should show**: Non-zero SDP length
- **If missing**: Response structure doesn't match expected format
- **If you see error**: "No SDP offer received from Cloudflare"

**Checkpoint 3: Answer Creation**
```javascript
[Camera XXX] Set local description, answer SDP length: 5678
```
- **Should show**: Non-zero answer SDP length
- **If fails here**: Browser can't parse offer SDP

**Checkpoint 4: Renegotiate Response**
```javascript
[Camera XXX] Renegotiate response: {}
```
- **Should show**: Empty object `{}` (Cloudflare returns 204 No Content)
- **If error**: Check backend logs for renegotiate failures

**Checkpoint 5: ICE Connection**
```javascript
[Camera XXX] ICE state: checking
[Camera XXX] ICE state: connected
```
- **Should progress**: new → checking → connected
- **If stuck at 'checking'**: ICE connectivity issue (firewall/NAT)
- **If stays 'new'**: ICE candidates not being generated
- **If goes to 'failed'**: ICE negotiation failed

**Checkpoint 6: Track Reception**
```javascript
[Camera XXX] Received track: video
```
- **Should trigger**: After connection state becomes 'connected'
- **If missing**: Connection successful but no media flowing
- **Check**: Backend logs should show packets being sent to Cloudflare

## Common Failure Scenarios

### Scenario A: Invalid Response Structure
**Symptoms**: Error at checkpoint 1 or 2
**Logs show**: "No SDP offer received from Cloudflare"
**Cause**: Backend not properly proxying Cloudflare response
**Fix**: Check backend proxy implementation in server.go

### Scenario B: ICE Stuck at 'checking'
**Symptoms**: Checkpoint 5 doesn't progress beyond 'checking'
**Logs show**: Many ICE candidates generated but connection doesn't establish
**Cause**: Network connectivity issue (firewall blocking UDP)
**Fix**: Check firewall rules, STUN server accessibility

### Scenario C: Connection 'failed'
**Symptoms**: ICE state goes directly to 'failed'
**Logs show**: ICE state changes from 'checking' to 'failed'
**Cause**: All ICE candidate pairs failed
**Fix**: Check STUN server configuration, NAT traversal issues

### Scenario D: No Track Event
**Symptoms**: Connection successful but no video appears
**Logs show**: Connection state is 'connected' but no "Received track" message
**Cause**: Media not flowing despite connection established
**Check**:
- Backend logs: Are cameras actually streaming?
- Are packets being sent to Cloudflare sessions?
- Is Cloudflare forwarding media to viewer session?

## Comparison with Curl Tests

Your curl tests showed:

1. **GET /api/cameras** → 18 cameras with sessionIds ✓
2. **POST /api/cf/sessions/new** → Returns sessionId ✓
3. **POST /api/cf/sessions/{id}/tracks/new** → Returns SDP offer with ICE ✓
4. **PUT /api/cf/sessions/{id}/renegotiate** → Returns 200/empty ✓

Since curl works, the backend proxy is correct. The issue must be in:
- How viewer.js constructs requests
- How viewer.js parses responses
- WebRTC connection establishment
- ICE candidate exchange

## Next Steps Based on Logs

**If logs show valid SDP at checkpoint 2**:
- Problem is in WebRTC connection, not API
- Focus on ICE connection logs
- Check if candidates are being generated
- Verify STUN server is reachable

**If logs fail at checkpoint 1 or 2**:
- Problem is in request/response format
- Compare request body with what curl sent
- Check response structure matches expected format
- Add backend logging to see what's being proxied

**If logs show connection succeeds but no track**:
- Problem is in media flow
- Check backend camera streams are active
- Verify packets are being sent to Cloudflare
- Check Cloudflare is forwarding to viewer session

## Binary Location

Updated binary: `/home/ethan/cams/cams-relay`

This binary has the embedded updated viewer.js with all debug logging.
