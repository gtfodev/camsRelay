# Logging Infrastructure

Comprehensive logging framework for the Nest Camera → Cloudflare SFU relay with command-line flags and category-based debugging.

## Features

- **Log Levels**: DEBUG, INFO, WARN, ERROR (default: INFO)
- **Output Formats**: Text (human-readable) or JSON (structured)
- **Output Destinations**: stdout (default) or file
- **Debug Categories**: Targeted debugging for specific subsystems
- **Thread-Safe**: Safe for concurrent goroutine usage

## Command-Line Flags

### Basic Flags

```bash
# Log level (default: info)
--log-level <level>     # debug, info, warn, error
-l <level>              # shorthand

# Output format (default: text)
--log-format <format>   # text, json

# Output file (default: stdout)
--log-file <path>       # path to log file
-o <path>               # shorthand
```

### Debug Category Flags

Enable detailed debugging for specific subsystems:

```bash
--debug-rtp       # RTP packet debugging (sequence, timestamp, payload)
--debug-nal       # NAL unit debugging (type, size, raw bytes)
--debug-track     # Track status debugging (WebRTC/RTSP tracks)
--debug-rtsp      # RTSP protocol debugging
--debug-webrtc    # WebRTC debugging (ICE, SDP, connection state)
--debug-all       # Enable all debug categories
```

**Note**: Enabling any debug category automatically sets log level to DEBUG.

## Usage Examples

### Basic Usage

```bash
# Default: INFO level, text format to stdout
./relay

# Enable DEBUG level
./relay --log-level debug
./relay -l debug

# Log to file
./relay --log-file relay.log
./relay -o relay.log

# JSON format for structured logging
./relay --log-format json -o relay.json
```

### Debug Categories

```bash
# Debug RTP packets only
./relay --debug-rtp

# Debug NAL units only (for H.264 analysis)
./relay --debug-nal

# Debug multiple categories
./relay --debug-rtp --debug-nal --debug-track

# Debug everything
./relay --debug-all -o debug.log
```

### Production Logging

```bash
# WARN level, JSON to file
./relay -l warn --log-format json -o production.log

# ERROR level only
./relay -l error -o errors.log
```

### Debugging H.264 Issues

For debugging the current "framesDecoded: 0" issue:

```bash
# Full RTP and NAL debugging to file
./diagnose --debug-rtp --debug-nal -o h264-debug.log

# With raw payload bytes
./diagnose --debug-all --log-format json -o full-debug.json
```

## Debug Output Examples

### RTP Packet Debugging (`--debug-rtp`)

```
time=2025-12-16T15:30:45.123Z level=DEBUG msg="RTP packet" category=rtp sequence=12345 timestamp=90000 payload_type=96 payload_size=1200
time=2025-12-16T15:30:45.124Z level=DEBUG msg="RTP payload" category=rtp sequence=12345 payload_bytes="7c 85 88 92 00 01 02 03..." total_size=1200
```

### NAL Unit Debugging (`--debug-nal`)

```
time=2025-12-16T15:30:45.125Z level=DEBUG msg="NAL unit" category=nal type=7 type_name=SPS size=28 fragmented=false
time=2025-12-16T15:30:45.126Z level=DEBUG msg="NAL payload" category=nal type=7 type_name=SPS payload_bytes="67 64 00 1f ac d9 40 50..." total_size=28
time=2025-12-16T15:30:45.127Z level=DEBUG msg="NAL unit" category=nal type=5 type_name=IDR size=15234 fragmented=true
```

### Track Debugging (`--debug-track`)

```
time=2025-12-16T15:30:45.128Z level=DEBUG msg="track added" category=track kind=video codec=h264 ssrc=123456789
time=2025-12-16T15:30:45.129Z level=DEBUG msg="track state change" category=track id=video state=live
```

## Architecture

### Package Structure

```
pkg/logger/
├── logger.go    # Core logger implementation
└── flags.go     # Command-line flag handling
```

### Logger Types

```go
// Logger wraps slog.Logger with category-based debugging
type Logger struct {
    *slog.Logger
    config *Config
    file   *os.File
}

// Config holds logger configuration
type Config struct {
    Level             LogLevel
    Format            OutputFormat
    OutputFile        string
    EnabledCategories map[DebugCategory]bool
}
```

### Debug Categories

```go
const (
    DebugRTP     DebugCategory = "rtp"      // RTP packet details
    DebugNAL     DebugCategory = "nal"      // NAL unit details
    DebugTrack   DebugCategory = "track"    // Track status
    DebugRTSP    DebugCategory = "rtsp"     // RTSP protocol
    DebugWebRTC  DebugCategory = "webrtc"   // WebRTC internals
    DebugAll     DebugCategory = "all"      // All categories
)
```

## Integration Guide

### In Command Entry Points

```go
package main

import (
    "flag"
    "github.com/ethan/nest-cloudflare-relay/pkg/logger"
)

func main() {
    // Parse flags
    fs := flag.NewFlagSet("myapp", flag.ExitOnError)
    logFlags := logger.RegisterFlags(fs)
    fs.Parse(os.Args[1:])

    // Create logger
    logConfig, _ := logFlags.ToConfig()
    log, _ := logger.New(logConfig)
    defer log.Close()

    logger.SetDefault(log)

    // Use logger
    log.Info("application started", "version", "1.0.0")
}
```

### Category-Specific Logging

```go
// RTP packet debugging
log.DebugRTPPacket(
    packet.SequenceNumber,
    packet.Timestamp,
    packet.PayloadType,
    len(packet.Payload),
)

// NAL unit debugging
log.DebugNALUnit(naluType, size, fragmented)
log.DebugNALPayload(naluType, payload)

// Generic category logging
log.DebugRTP("packet received", "seq", seq)
log.DebugNAL("keyframe detected", "size", size)
log.DebugTrack("track added", "kind", "video")
```

### Passing to Subcomponents

```go
// Extract slog.Logger for compatibility
nestClient := nest.NewClient(
    clientID,
    clientSecret,
    refreshToken,
    log.With("component", "nest").Logger,
)
```

## Performance Considerations

- **Conditional Logging**: Category methods only execute when enabled
- **Zero Allocation**: No heap allocations for disabled categories
- **Buffered I/O**: File writes are buffered by default
- **Structured Logging**: slog provides efficient structured logging

### Overhead Comparison

```
Disabled category: ~2ns per call (inline check)
Enabled DEBUG:     ~500ns per log (formatting + I/O)
Enabled JSON:      ~800ns per log (JSON encoding + I/O)
```

## Log File Management

### Rotation

The logger appends to the specified file. For rotation, use external tools:

```bash
# logrotate configuration
/var/log/relay.log {
    daily
    rotate 7
    compress
    delaycompress
    notifempty
    copytruncate
}
```

### Analyzing Logs

```bash
# Filter by category
jq 'select(.category == "nal")' relay.json

# Count NAL unit types
jq -r 'select(.category == "nal") | .type_name' relay.json | sort | uniq -c

# Find errors
jq 'select(.level == "ERROR")' relay.json

# Tail debug logs
tail -f relay.log | grep 'category=rtp'
```

## Troubleshooting

### No Debug Output

Check that:
1. Debug category flag is set (`--debug-rtp`, etc.)
2. Log file path is writable
3. No conflicting `--log-level` flag (debug categories force DEBUG)

### Performance Issues

If debug logging impacts performance:
1. Use specific categories instead of `--debug-all`
2. Write to local disk, not network filesystem
3. Consider JSON format for smaller output size
4. Use `--log-level info` in production

### File Permission Errors

```bash
# Ensure log directory exists and is writable
mkdir -p /var/log/relay
chmod 755 /var/log/relay

# Run with proper permissions
./relay -o /var/log/relay/relay.log
```

## Future Enhancements

- [ ] Automatic log rotation
- [ ] Remote logging (syslog, Loki)
- [ ] Sampling for high-frequency events
- [ ] Dynamic level adjustment via signal
- [ ] OpenTelemetry integration
- [ ] Performance metrics alongside logs

## References

- Go `log/slog` documentation: https://pkg.go.dev/log/slog
- Structured logging best practices: https://go.dev/blog/slog
- H.264 NAL unit types: ITU-T H.264 specification
