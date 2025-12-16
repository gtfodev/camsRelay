# Debug Implementation Summary

## Problem Statement

Despite all API endpoints working correctly via curl tests:
- `/api/cameras` returns 18 cameras with sessionIds
- `/api/cf/sessions/new` creates viewer sessions
- `/api/cf/sessions/{id}/tracks/new` returns valid SDP offers
- `/api/cf/sessions/{id}/renegotiate` completes successfully

The web viewer still shows infinite spinning with no video.

## Root Cause Analysis Approach

Since curl works but browser doesn't, the issue must be in:
1. How the browser sends requests (different from curl)
2. How the browser parses responses (different expectations)
3. WebRTC connection establishment (not tested by curl)
4. Media flow (not visible in curl tests)

## Solution: Comprehensive Debug Logging

Rather than guessing, I've instrumented the entire WebRTC flow with detailed logging to reveal exactly where it fails.

## Changes Made

### File: `/home/ethan/cams/pkg/api/web/static/js/viewer.js`

Added logging at every critical step:

**1. Track Pull (API Call)**
```javascript
console.log('Pulling tracks:', { url, tracks });
console.log('Received tracks response:', data);
console.log('Extracted SDP offer, length:', data.sessionDescription.sdp.length);
```
- Reveals if API response structure matches expectations
- Shows if SDP offer exists and is non-empty
- Catches any parsing errors

**2. WebRTC Setup**
```javascript
console.log('Setting remote description, SDP length:', offer.length);
console.log('Creating answer');
console.log('Set local description, answer SDP length:', answer.sdp.length);
```
- Shows if browser can parse offer SDP
- Confirms answer creation succeeds
- Validates local description is set

**3. Renegotiation**
```javascript
console.log('Sending renegotiate request, answer SDP length:', answerSdp.length);
console.log('Renegotiate response:', data);
console.log('Renegotiation complete');
```
- Shows answer is sent to Cloudflare
- Confirms Cloudflare accepts it
- Validates complete flow

**4. ICE Connection**
```javascript
pc.onicecandidate = (event) => {
    if (event.candidate) {
        console.log('ICE candidate:', event.candidate.candidate);
    } else {
        console.log('ICE gathering complete');
    }
};

pc.oniceconnectionstatechange = () => {
    console.log('ICE state:', this.pc.iceConnectionState);
};
```
- Shows ICE candidate generation
- Tracks ICE state progression (new → checking → connected)
- Reveals if ICE negotiation succeeds

**5. Connection State**
```javascript
pc.onconnectionstatechange = () => {
    console.log('Connection state:', this.pc.connectionState);
};
```
- Tracks overall connection health
- Shows when connection establishes or fails

**6. Track Reception**
```javascript
pc.ontrack = (event) => {
    console.log('Received track:', event.track.kind);
};
```
- Confirms media track arrives
- This is the final step before video displays

## How to Use

### 1. Start the Relay
```bash
cd /home/ethan/cams
./cams-relay
```

### 2. Open Browser
- Navigate to: http://localhost:8080
- Open Developer Console: F12 → Console tab

### 3. Observe Logs
Watch the console for log messages from each camera. Follow the flow:
```
[Camera XXX] Connecting
[Camera XXX] Pulling tracks: ...
[Camera XXX] Received tracks response: ...
[Camera XXX] Extracted SDP offer, length: ...
[Camera XXX] Setting remote description...
[Camera XXX] Creating answer
[Camera XXX] Set local description...
[Camera XXX] Sending renegotiate...
[Camera XXX] Renegotiate response: ...
[Camera XXX] ICE state: checking
[Camera XXX] ICE state: connected
[Camera XXX] Connection state: connected
[Camera XXX] Received track: video
```

### 4. Identify Break Point
The logs will show exactly where the flow breaks. See `LOG_INTERPRETATION_GUIDE.md` for detailed analysis.

## Diagnostic Decision Tree

**If logs stop after "Pulling tracks"**:
→ API call failed (check Network tab)

**If logs show "No SDP offer" error**:
→ Response structure mismatch (check response format)

**If logs stop after "Setting remote description"**:
→ Invalid SDP offer (browser rejected it)

**If logs stop after "Renegotiate response"**:
→ Renegotiation failed (check backend logs)

**If ICE stays at "checking"**:
→ Network connectivity issue (firewall/NAT)

**If ICE goes to "failed"**:
→ All ICE candidate pairs failed

**If connection succeeds but no track**:
→ Media flow issue (backend camera streams)

## Expected Results

For a working camera, you should see this complete sequence:
1. Track pull succeeds with valid SDP
2. Answer creation succeeds
3. Renegotiation completes
4. ICE candidates generated
5. ICE state: checking → connected
6. Connection state: connected
7. Track received
8. Video displays

Any deviation from this sequence indicates the failure point.

## Files Created

1. **VIEWER_DEBUG_SUMMARY.md** - Comprehensive overview of changes and expected behavior
2. **LOG_INTERPRETATION_GUIDE.md** - Step-by-step guide for analyzing console logs
3. **DEBUG_IMPLEMENTATION.md** - This file, explains what was done and why
4. **test_viewer_debug.sh** - Quick reference for testing

## Next Steps

1. Run the relay and observe console logs
2. Follow the log interpretation guide to identify failure point
3. Based on failure point, take appropriate action:
   - API issue → Fix backend proxy
   - SDP issue → Validate SDP format
   - ICE issue → Check network/firewall
   - Media issue → Check backend streams

## Architecture Understanding

The flow is:
```
Browser                Backend Proxy              Cloudflare
   |                        |                          |
   |--POST /cf/sessions/new->|                          |
   |                        |--POST /sessions/new------>|
   |                        |<-sessionId + SDP---------|
   |<-sessionId-------------|                          |
   |                        |                          |
   |--POST /tracks/new----->|                          |
   |   {tracks: [...]}      |--POST /tracks/new-------->|
   |                        |   {tracks: [...]}         |
   |                        |<-SDP offer + ICE---------|
   |<-SDP offer + ICE-------|                          |
   |                        |                          |
   |--PUT /renegotiate----->|                          |
   |   {sdp: answer}        |--PUT /renegotiate-------->|
   |                        |   {sdp: answer}           |
   |                        |<-200/204------------------|
   |<-200-------------------|                          |
   |                        |                          |
   |<====== WebRTC Media Connection (ICE/RTP) ========>|
```

Curl tests validated steps 1-4 (API calls).
Debug logging will reveal if steps 5-6 (WebRTC/media) work.

## Why This Approach

Rather than making blind changes, this diagnostic approach:
1. Adds no functionality changes (no risk)
2. Reveals actual failure point (not guessed)
3. Provides data-driven debugging
4. Can be removed once issue is found
5. Helps understand WebRTC flow for future issues

## Commit

Changes committed with message:
```
debug: add comprehensive logging to viewer WebRTC flow

Add detailed console logging throughout viewer.js to diagnose
why video isn't showing despite working curl tests
```

## Binary Location

Updated binary: `/home/ethan/cams/cams-relay`

This binary includes the debug logging embedded in the web assets.
