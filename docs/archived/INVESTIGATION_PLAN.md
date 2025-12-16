# RTSP Packet Reception Investigation

## Problem Summary
ffmpeg successfully receives RTP packets after PLAY 200 OK, but our Go RTSP client does not - using the same URL and getting the same PLAY response.

## Investigation Tools Created

### 1. `/home/ethan/cams/scripts/investigate_rtsp.sh`
Comprehensive investigation script that:
- Captures ffmpeg's TCP conversation with tcpdump
- Captures our Go client's TCP conversation with tcpdump
- Extracts and compares RTSP protocol text
- Generates comparison logs

**Usage:**
```bash
cd /home/ethan/cams
./scripts/investigate_rtsp.sh '<your_rtsp_url>'
```

**Output files:**
- `/tmp/ffmpeg_rtsp.pcap` - ffmpeg packet capture
- `/tmp/relay_rtsp.pcap` - Go client packet capture
- `/tmp/ffmpeg_debug.log` - ffmpeg debug output
- `/tmp/relay_debug.log` - relay debug output

### 2. `/home/ethan/cams/scripts/ffmpeg_headers_test.sh`
Quick header analysis script that:
- Runs ffmpeg with trace-level logging
- Extracts all RTSP headers
- Shows exactly what ffmpeg sends

**Usage:**
```bash
./scripts/ffmpeg_headers_test.sh '<your_rtsp_url>'
```

## Code Changes Made

### Enhanced RTSP Client Logging
Added detailed diagnostics to `/home/ethan/cams/pkg/rtsp/client.go`:

1. **Connection details** (line 116-119):
   - Logs local_addr, remote_addr, and TLS status
   - Helps verify connection establishment

2. **Buffer inspection** (line 220-227):
   - Logs buffered bytes BEFORE each peek attempt
   - Shows if data arrives after PLAY response
   - Only logs first few iterations to avoid spam

## Investigation Steps

### Phase 1: Quick Header Comparison
Run the quick test to see ffmpeg's exact headers:
```bash
./scripts/ffmpeg_headers_test.sh '<rtsp_url>'
```

Look for:
- Does ffmpeg send `Range: npt=0.000-` on PLAY? (We explicitly don't)
- What User-Agent does ffmpeg use?
- Any headers we're missing?

### Phase 2: Full Wire Protocol Capture
Run the comprehensive investigation:
```bash
sudo ./scripts/investigate_rtsp.sh '<rtsp_url>'
```

This will:
1. Capture ffmpeg's full TCP conversation
2. Capture our client's full TCP conversation
3. Display side-by-side comparison

### Phase 3: Detailed Analysis
Manually inspect the pcap files:
```bash
# View ffmpeg's RTSP conversation
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n | less

# View our client's RTSP conversation
sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n | less
```

Search for (press `/` in less):
- `PLAY` - Find the PLAY request/response
- `\$` - Find RTP packets (start with $ magic byte)
- `Range` - Check Range header usage

### Phase 4: Socket-Level Diagnostics
Run the relay with enhanced logging:
```bash
cd /home/ethan/cams
go build -o bin/relay cmd/relay/main.go
./bin/relay 2>&1 | tee /tmp/relay_detailed.log
```

Check the log for:
- `buffered_bytes` - Shows if ANY data arrives after PLAY
- `read loop iteration` - Shows each attempt to read
- `no buffered data after PLAY response` - Critical indicator

## Key Questions to Answer

1. **Headers**: Does ffmpeg send headers we don't?
   - User-Agent difference?
   - Range header on PLAY?
   - Accept or other headers?

2. **TCP Socket Options**: Does ffmpeg set socket options we don't?
   - TCP_NODELAY?
   - SO_KEEPALIVE?
   - Different buffer sizes?

3. **Timing**: Does ffmpeg wait/delay between requests?
   - Check timestamps in pcap
   - Look for gaps between SETUP and PLAY

4. **TLS/Connection**: Is the TLS handshake different?
   - TLS version?
   - Cipher suite negotiation?

5. **Data Flow**: Does ANY data arrive after PLAY?
   - Check `buffered_bytes` in logs
   - If 0, server isn't sending
   - If >0, our parsing logic is wrong

## Expected Outcomes

### If buffered_bytes > 0
Server IS sending data, our parsing is broken:
- Check for unexpected RTSP response before RTP
- Verify we're not missing bytes
- Check the `Discard(4)` fix is working

### If buffered_bytes = 0
Server is NOT sending data:
- We're missing a critical header/request
- Socket options matter
- Server expects something after PLAY

## Next Steps After Investigation

Based on findings, we'll implement one of:

1. **Missing Header Fix**: Add the header ffmpeg uses
2. **Socket Option Fix**: Configure TCP socket like ffmpeg
3. **Protocol Sequence Fix**: Adjust request timing/order
4. **Range Header Fix**: Add back Range header (currently removed)

## Current RTSP Client State

**Working:**
- OPTIONS ✓
- DESCRIBE ✓
- SETUP (all tracks) ✓
- PLAY request ✓
- PLAY response 200 OK ✓

**Not Working:**
- RTP packet reception ✗
- No `$` (0x24) magic bytes received
- Timeouts on read

**Current Implementation:**
- TCP transport (interleaved)
- TLS (rtsps://)
- Keepalive enabled (25s interval)
- 65KB read buffer
- 10s read timeout
