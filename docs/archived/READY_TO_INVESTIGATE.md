# Ready to Investigate: RTSP Packet Reception Mystery

## Quick Start

You're all set! When you have a fresh RTSP URL from Nest, run:

```bash
cd /home/ethan/cams

# Quick test - see ffmpeg's exact headers
./scripts/ffmpeg_headers_test.sh '<your_rtsp_url>'

# Full investigation - capture wire protocol
sudo ./scripts/investigate_rtsp.sh '<your_rtsp_url>'

# Socket-level comparison (optional)
./scripts/socket_comparison.sh '<your_rtsp_url>'
```

## What Was Done

### 1. Investigation Scripts Created
Three scripts to systematically compare ffmpeg vs our Go client:

- **`scripts/investigate_rtsp.sh`** - Captures full TCP conversations with tcpdump
- **`scripts/ffmpeg_headers_test.sh`** - Quick header analysis
- **`scripts/socket_comparison.sh`** - TCP socket option comparison

### 2. Enhanced RTSP Client Logging
Modified `/home/ethan/cams/pkg/rtsp/client.go` with:

- Buffer inspection before each read (shows if ANY data arrives)
- Connection details logging (local/remote addr, TLS status)
- TCP socket optimization (TCP_NODELAY, KeepAlive)

### 3. Documentation
Created `/home/ethan/cams/INVESTIGATION_PLAN.md` with:

- Detailed investigation steps
- Questions to answer
- Expected outcomes
- Next steps based on findings

## Key Things to Look For

### In ffmpeg headers test:
1. Does ffmpeg send `Range: npt=0.000-` on PLAY?
2. What User-Agent does ffmpeg use?
3. Any headers we're missing?

### In wire protocol capture:
1. Compare PLAY requests byte-by-byte
2. Check timing between requests
3. Look for RTP packets (start with `$` 0x24 magic byte)

### In relay logs:
Watch for `buffered_bytes` after PLAY response:
- If `buffered_bytes = 0` → Server not sending (missing header/option)
- If `buffered_bytes > 0` → Server sending, our parsing broken

## Investigation Workflow

```
1. Run ffmpeg_headers_test.sh
   ↓
   Compare headers with our client
   ↓
2. Run investigate_rtsp.sh
   ↓
   Analyze pcap files for differences
   ↓
3. Check buffered_bytes in relay logs
   ↓
   Determine if server sends data
   ↓
4. Implement fix based on findings
```

## Common Issues to Check

### Missing Headers
```
Our client sends:
- User-Agent: nest-cloudflare-relay/1.0
- Accept: application/sdp (DESCRIBE only)
- NO Range header on PLAY (explicitly removed)

Check if ffmpeg sends:
- Different User-Agent?
- Range header?
- Other headers?
```

### TCP Socket Options
```
We now set:
- TCP_NODELAY (disable Nagle's algorithm)
- KeepAlive: 30s

Does ffmpeg set different options?
```

### Timing Issues
```
We send:
OPTIONS → DESCRIBE → SETUP → PLAY (immediate)

Does ffmpeg wait between requests?
```

## After Investigation

Based on findings, implement one of:

1. **Add missing header** - If ffmpeg sends header we don't
2. **Adjust socket options** - If ffmpeg configures differently
3. **Fix timing** - If ffmpeg waits between requests
4. **Add Range header back** - If that's what's needed (currently removed)

## Quick Diagnosis Commands

```bash
# View ffmpeg's full RTSP conversation
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n | less
# Press '/' to search for 'PLAY', 'Range', '$'

# View our client's full RTSP conversation
sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n | less

# Check buffered bytes in relay log
grep "buffered_bytes" /tmp/relay_debug.log

# Compare packet counts
echo "ffmpeg packets:"
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -n | wc -l
echo "relay packets:"
sudo tcpdump -r /tmp/relay_rtsp.pcap -n | wc -l
```

## Current Status

### Working:
- OPTIONS request/response ✓
- DESCRIBE request/response ✓
- SETUP all tracks ✓
- PLAY request sent ✓
- PLAY 200 OK response received ✓
- Keepalive goroutine running ✓

### Not Working:
- No RTP packets received after PLAY ✗
- Read timeouts after PLAY response ✗

### Recent Changes:
- Added TCP_NODELAY socket option
- Added KeepAlive to dialer
- Enhanced buffer logging
- Removed Range header from PLAY (like go2rtc)

## Expected Investigation Time
- Scripts run: 15-30 seconds each
- Analysis: 5-10 minutes
- Fix implementation: 5-30 minutes (depends on finding)

## Files Modified
- `/home/ethan/cams/pkg/rtsp/client.go` - Enhanced logging, socket options
- `/home/ethan/cams/scripts/investigate_rtsp.sh` - New investigation script
- `/home/ethan/cams/scripts/ffmpeg_headers_test.sh` - New header test
- `/home/ethan/cams/scripts/socket_comparison.sh` - New socket comparison

## Ready!
All tools are in place. Just get a fresh RTSP URL and run the scripts.
