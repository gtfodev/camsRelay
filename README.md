# Nest Camera → Cloudflare SFU Relay

RTSP-based relay system for streaming Nest camera feeds to Cloudflare Calls SFU, enabling WebRTC distribution to browsers.

## Architecture

```
Nest Cameras (20x) → RTSP → Relay Server → Cloudflare Calls SFU → WebRTC → Browsers
                      (This Project)
```

### Phase 1: Foundation (Complete)

The foundation provides authentication, device discovery, and API client implementations:

- **Google Nest Integration**: OAuth2 authentication, device listing, RTSP stream generation/management
- **Cloudflare Calls Integration**: Session management, track operations, renegotiation handling
- **Configuration Management**: Environment-based credential loading with validation

### Phase 2: RTSP Ingestion & RTP Bridge (Upcoming)

- RTSP client for H264/AAC stream consumption
- RTP packet processing and forwarding
- Automatic stream extension (5-minute Nest limit)
- Cloudflare session lifecycle management

## Project Structure

```
cams/
├── pkg/
│   ├── config/       # Configuration loading and validation
│   ├── nest/         # Google Nest API client (RTSP only)
│   └── cloudflare/   # Cloudflare Calls API client
├── cmd/
│   └── relay/        # Main relay application
├── .env              # Credentials (not committed)
└── go.mod            # Go module definition
```

## Components

### 1. Configuration (`pkg/config/config.go`)

Loads credentials from `.env` file with automatic URL decoding:

```go
type Config struct {
    Google     GoogleConfig     // OAuth2 credentials + project ID
    Cloudflare CloudflareConfig // App ID + API token
}
```

**Features**:
- Parses `key=value` format
- URL-decodes values (handles `%2F` etc.)
- Validates all required fields
- Returns detailed error messages

### 2. Nest Client (`pkg/nest/client.go`)

Handles Google SDM API communication for RTSP streams:

```go
client := nest.NewClient(clientID, clientSecret, refreshToken, logger)

// List all cameras
devices, err := client.ListDevices(ctx, projectID)

// Generate RTSP stream (5-minute TTL)
stream, err := client.GenerateRTSPStream(ctx, projectID, deviceID)

// Extend stream before expiry
err = client.ExtendRTSPStream(ctx, stream)

// Stop stream
err = client.StopRTSPStream(ctx, stream)
```

**Features**:
- OAuth2 token refresh with caching
- RW mutex for thread-safe token access
- Filters devices to cameras only
- Extracts device IDs from full names
- Structured logging with context

**RTSP Stream Details**:
- URL: `rtsps://stream-*.dropcam.com:443/...`
- TTL: 5 minutes (must extend or regenerate)
- Codecs: H264 video, AAC audio (only)
- Protocol: RTSP over TLS

### 3. Cloudflare Client (`pkg/cloudflare/client.go`)

Manages Cloudflare Calls SFU sessions and tracks:

```go
client := cloudflare.NewClient(appID, apiToken, logger)

// Create session
session, err := client.CreateSession(ctx)

// Add tracks (push local or pull remote)
tracksResp, err := client.AddTracks(ctx, sessionID, &TracksRequest{...})

// Renegotiate if required
if tracksResp.RequiresImmediateRenegotiation {
    _, err = client.Renegotiate(ctx, sessionID, &RenegotiateRequest{...})
}

// Close tracks
_, err = client.CloseTracks(ctx, sessionID, &CloseTracksRequest{...})

// Get session state
state, err := client.GetSessionState(ctx, sessionID)
```

**Features**:
- Full OpenAPI schema implementation
- Automatic error handling
- Retry with exponential backoff
- Structured request/response types
- Context-aware timeouts

**API Types** (`pkg/cloudflare/types.go`):
- `SessionDescription`: SDP offer/answer
- `TrackObject`: Media track metadata
- `TracksRequest/Response`: Add tracks operation
- `RenegotiateRequest/Response`: Session renegotiation
- `CloseTracksRequest/Response`: Track cleanup
- `GetSessionStateResponse`: Current session info

### 4. Main Application (`cmd/relay/main.go`)

Demonstrates full initialization flow:

1. Load configuration from `.env`
2. Initialize Nest client
3. List all cameras (20 discovered)
4. Initialize Cloudflare client
5. Create test session
6. Test RTSP stream generation
7. Clean up

**Output**:
```
✓ Success! All components initialized:
  - Configuration loaded
  - Nest client authenticated
  - Found 20 camera(s)
  - Cloudflare client connected
  - Test session created: d43f92c22c9eb00bbe94156abc38c026...
```

## Configuration

Create `.env` in project root:

```bash
## Google API ##
client_id=YOUR_CLIENT_ID
client_secret=YOUR_CLIENT_SECRET
project_id=YOUR_PROJECT_ID
refresh_token=YOUR_REFRESH_TOKEN

## Cloudflare ##
app_id=YOUR_APP_ID
api_token=YOUR_API_TOKEN
```

**Notes**:
- Values are automatically URL-decoded
- All fields are required
- Refresh token must have SDM API scope

## Build & Run

```bash
# Install dependencies
go mod tidy

# Build
go build ./cmd/relay

# Run
./relay
```

**Output**: JSON-structured logs to stdout

## Camera Inventory

20 Nest cameras discovered:
- All support RTSP protocol
- Video: H264 (90kHz clock rate)
- Audio: AAC (48kHz typical)
- Names: Lab-FrontEntrance, Basement, Sterilization, etc.

## Technical Details

### Concurrency Patterns

- **Token Cache**: RWMutex with 30s expiry buffer
- **Context Propagation**: All I/O operations context-aware
- **HTTP Clients**: 30s timeout, connection pooling
- **Structured Logging**: JSON with component tags

### Error Handling

- Explicit error returns (no silent failures)
- Wrapped errors with context (`fmt.Errorf("%w", err)`)
- HTTP response body included in errors
- Validation at config load time

### Dependencies

```go
require (
    github.com/pion/webrtc/v4 v4.1.8  // For Phase 2 RTP handling
)
```

## Next Steps (Phase 2)

1. **RTSP Client**: Connect to Nest stream URLs
   - Parse RTP packets from RTSP
   - Extract H264 NAL units
   - Extract AAC frames

2. **RTP Bridge**: Forward to Cloudflare
   - Create SDP offer with H264/AAC tracks
   - Add tracks to Cloudflare session
   - Map RTP packets to WebRTC

3. **Stream Lifecycle**:
   - Auto-extend every 4 minutes (1-minute buffer)
   - Graceful reconnection on failures
   - Session cleanup on shutdown

4. **Production Features**:
   - Multi-camera support
   - Health monitoring
   - Metrics/observability
   - Deployment configuration

## Architecture Decisions

### Why RTSP (not WebRTC from Nest)?

While Nest cameras support both protocols, the codebase uses RTSP because:
- More stable for long-running streams
- Simpler protocol (no ICE/STUN complexity)
- Direct RTP access for processing
- Better suited for server-to-server relay

### Why Cloudflare Calls?

- Global edge network (low latency)
- Handles WebRTC complexity (ICE, DTLS, SRTP)
- Scales to many viewers per camera
- SFU architecture (efficient bandwidth)

### Codec Choice

H264/AAC only:
- Native Nest camera output
- Universal browser support
- No transcoding needed
- Minimal CPU overhead

## Logging

JSON structured logs with levels:
- **INFO**: Normal operations
- **WARN**: Retries, recoverable errors
- **ERROR**: Fatal errors requiring intervention

Example:
```json
{
  "time": "2025-12-15T16:16:27Z",
  "level": "INFO",
  "msg": "generated RTSP stream",
  "component": "nest",
  "device_id": "AVPHwEtYJ6...",
  "expires_at": "2025-12-16T00:21:34Z"
}
```

## Development

### Code Organization

- **Standard library first**: Minimal external dependencies
- **Context everywhere**: Cancellation, timeouts, deadlines
- **Error wrapping**: Preserve error chains
- **Type safety**: Explicit types, no `interface{}`

### Go Version

- **Minimum**: 1.22.2
- **Toolchain**: 1.24.11 (auto-upgraded by go2rtc dependency)

### Testing

```bash
# Build verification
go build ./pkg/... ./cmd/...

# Run main
go run ./cmd/relay
```

## Reference Files

Located in `rtsp_files/`, `webrtc_files/`, `nest_api_files/` - these are reference implementations not part of the build.

## License

Internal project - all rights reserved.
