# RTSP Investigation Checklist

Use this checklist when running the investigation with a fresh URL.

## Prerequisites
- [ ] Fresh RTSP URL from Nest (not expired)
- [ ] URL is for the same camera we tested ffmpeg with
- [ ] Running from `/home/ethan/cams` directory
- [ ] Have sudo access for tcpdump

## Step 1: Quick Header Test (2 min)

```bash
./scripts/ffmpeg_headers_test.sh '<rtsp_url>'
```

**Check for:**
- [ ] ffmpeg's User-Agent (different from ours?)
- [ ] Range header on PLAY (we don't send it)
- [ ] Any headers on DESCRIBE we're missing
- [ ] Any headers on SETUP we're missing
- [ ] Any headers on PLAY we're missing

**Document findings:**
```
ffmpeg User-Agent: _______________________
Range header on PLAY: Yes / No
Other differences: _______________________
```

## Step 2: Full Wire Protocol Capture (1 min)

```bash
sudo ./scripts/investigate_rtsp.sh '<rtsp_url>'
```

**Verify outputs created:**
- [ ] `/tmp/ffmpeg_rtsp.pcap` exists
- [ ] `/tmp/relay_rtsp.pcap` exists
- [ ] `/tmp/ffmpeg_debug.log` exists
- [ ] `/tmp/relay_debug.log` exists

**Quick packet count:**
```bash
echo "ffmpeg packets:"
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -n | wc -l

echo "relay packets:"
sudo tcpdump -r /tmp/relay_rtsp.pcap -n | wc -l
```

- [ ] ffmpeg has significantly more packets (indicates RTP reception)
- [ ] relay has fewer packets (confirms no RTP received)

## Step 3: Analyze RTSP Conversation (5 min)

### Compare PLAY requests:
```bash
# ffmpeg's PLAY
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n 2>/dev/null | grep -A 10 "PLAY"

# Our PLAY
sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n 2>/dev/null | grep -A 10 "PLAY"
```

**Document differences:**
- [ ] Headers present in ffmpeg but not in ours: _______________________
- [ ] Headers present in ours but not in ffmpeg: _______________________
- [ ] Different header values: _______________________

### Look for RTP packets:
```bash
# ffmpeg should have '$' (0x24) for RTP packets
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n 2>/dev/null | grep -c '\$'

# Ours should be 0
sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n 2>/dev/null | grep -c '\$'
```

- [ ] ffmpeg has RTP packets (count > 0)
- [ ] Our client has no RTP packets (count = 0)

## Step 4: Check Buffered Data (Critical)

```bash
grep "buffered_bytes" /tmp/relay_debug.log
grep "play_response_received" /tmp/relay_debug.log
```

**Critical question:**
- [ ] buffered_bytes = 0 after PLAY response (server not sending)
- [ ] buffered_bytes > 0 after PLAY response (server sending, parsing broken)

**If buffered_bytes = 0:**
→ Server is NOT sending RTP packets to us
→ We're missing something in our request
→ Focus on header/socket option differences

**If buffered_bytes > 0:**
→ Server IS sending data
→ Our parsing logic is broken
→ Focus on packet parsing in ReadPackets()

## Step 5: Socket Option Comparison (Optional, 3 min)

```bash
./scripts/socket_comparison.sh '<rtsp_url>'
```

**Check for:**
- [ ] TCP_NODELAY differences
- [ ] SO_KEEPALIVE differences
- [ ] Window size differences
- [ ] Buffer size differences

## Step 6: Detailed RTSP Analysis (5 min)

Open both captures side by side:
```bash
# Terminal 1
sudo tcpdump -r /tmp/ffmpeg_rtsp.pcap -A -n | less

# Terminal 2
sudo tcpdump -r /tmp/relay_rtsp.pcap -A -n | less
```

**Compare request by request:**
- [ ] OPTIONS - Any differences?
- [ ] DESCRIBE - Any differences?
- [ ] SETUP (track 0) - Any differences?
- [ ] SETUP (track 1) - Any differences?
- [ ] PLAY - Any differences?

**Search patterns in less:**
- Press `/` then type `PLAY` to find PLAY request
- Press `/` then type `Range` to find Range headers
- Press `/` then type `\$` to find RTP packets
- Press `n` for next match, `N` for previous

## Decision Tree

```
Start
  ↓
Check buffered_bytes
  ↓
┌─────────────────┬─────────────────┐
│ = 0             │ > 0             │
│ Server not      │ Server sending  │
│ sending         │                 │
└─────────────────┴─────────────────┘
  ↓                 ↓
Compare headers   Fix parsing
  ↓                 ↓
Missing header?   Check Discard(4)
  ↓                 ↓
Add header        Check buf4[0]
  ↓                 ↓
Different socket  Verify packet
options?          format
  ↓
Add options
  ↓
Test fix
```

## Common Findings and Fixes

### Finding: ffmpeg sends Range header, we don't
**Fix:**
```go
// In Play() method
req.Header["Range"] = "npt=0.000-"
```

### Finding: ffmpeg has different User-Agent
**Fix:**
```go
// In writeRequest()
buf.WriteString("User-Agent: ffmpeg/X.Y.Z\r\n")
```

### Finding: buffered_bytes > 0 but no packets parsed
**Fix:**
Check ReadPackets() parsing logic, likely issue with Discard(4)

### Finding: No TCP_NODELAY in ffmpeg
**Fix:**
Remove TCP_NODELAY code we added

## After Fix Implementation

1. Rebuild:
```bash
go build -o bin/relay cmd/relay/main.go
```

2. Test:
```bash
./bin/relay
```

3. Verify:
```bash
# Should see "received first RTP packet successfully"
# Should see increasing packet counts
```

## Success Criteria
- [ ] "received first RTP packet successfully" in logs
- [ ] Packet count increasing
- [ ] No read timeouts
- [ ] Video frames being processed
- [ ] WebRTC bridge receiving samples

## Document Your Findings
When you identify the fix, document it in:
- New file: `/home/ethan/cams/BUGFIX_RTSP_PACKETS.md`
- Include: What was different, why it matters, the fix
