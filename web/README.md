# Nest Camera Viewer

Web-based viewer for consuming Nest camera streams from Cloudflare Calls.

## Architecture

The viewer is a browser-based WebRTC consumer that connects directly to Cloudflare Calls to receive camera streams.

```
┌─────────────┐         ┌──────────────┐         ┌────────────────┐
│   Browser   │  HTTP   │  Go Backend  │  RTSP   │  Nest Camera   │
│   Viewer    │────────▶│  API Server  │◀────────│                │
│             │         │              │         └────────────────┘
│             │         │              │
│             │         │   Producer   │  WebRTC
│             │         │    Session   │────────┐
│             │         └──────────────┘        │
│             │                                 │
│             │  WebRTC (Consumer Session)      │
│             │◀────────────────────────────────┤
│             │         Cloudflare Calls        │
└─────────────┘                                 │
                                                │
                    ┌───────────────────────────┘
                    │
                    ▼
            ┌─────────────────┐
            │  Cloudflare     │
            │  Calls SFU      │
            └─────────────────┘
```

## Components

### Backend API (Go)

**Location:** `/pkg/api/server.go`

Provides HTTP endpoints for the viewer:

- `GET /api/cameras` - Returns list of active camera sessions
- `GET /api/config` - Returns Cloudflare app ID for viewer
- `GET /` - Serves main viewer HTML page
- `GET /static/*` - Serves static assets (JS, CSS)

### Frontend Viewer (JavaScript)

**Files:**
- `index.html` - Main viewer page
- `static/js/viewer.js` - WebRTC connection management
- `static/js/grid.js` - Grid layout and tile management
- `static/css/style.css` - Styling

**Key Features:**
- Responsive grid layout (1-4 columns based on screen size)
- Real-time connection status per camera
- Automatic reconnection on failures
- No backend proxy for media - direct WebRTC to Cloudflare

## WebRTC Flow

### Producer Side (Go Backend)

1. Create Cloudflare session
2. Add local tracks (video, audio)
3. Send RTP packets from Nest camera to tracks

### Consumer Side (Browser Viewer)

1. Fetch active cameras from `/api/cameras`
2. For each camera:
   - Create new Cloudflare session (viewer session)
   - Call `/tracks/new` with `location: remote`, producer's `sessionId`, `trackName`
   - Receive SDP offer from Cloudflare
   - Create RTCPeerConnection, set remote offer
   - Create SDP answer
   - Send answer to Cloudflare via `/renegotiate`
   - Attach received MediaStream to video element

## Security Considerations

The viewer calls Cloudflare Calls API directly from the browser without authentication tokens. This is possible because Cloudflare Calls has a permissive security model for consumer sessions - anyone with the app ID can create a consumer session and pull tracks.

For production deployments, consider:
- Implementing authentication on the backend API
- Using signed URLs for camera access
- Rate limiting the `/api/cameras` endpoint
- Restricting CORS origins

## Usage

1. Start the multi-relay backend:
   ```bash
   ./multi-relay
   ```

2. Open browser to http://localhost:8080

3. Viewer will automatically:
   - Fetch list of active cameras
   - Create WebRTC connections to Cloudflare
   - Display camera streams in a grid

## Troubleshooting

### No video appears

Check browser console for errors. Common issues:
- Cloudflare app ID mismatch
- Producer session not yet established
- Network connectivity to Cloudflare

### "Failed to create session" error

Verify:
- Backend is running and accessible
- `/api/config` returns correct Cloudflare app ID
- Cloudflare Calls API is accessible from browser

### Connection state stuck in "connecting"

- Check WebRTC peer connection logs in browser DevTools
- Verify ICE connectivity (STUN/TURN servers)
- Check for firewall blocking WebRTC ports

## Browser Compatibility

Requires modern browser with WebRTC support:
- Chrome 74+
- Firefox 66+
- Safari 12.1+
- Edge 79+

## Performance

Grid layout adjusts automatically:
- Mobile (320-768px): 1 column
- Tablet (769-1200px): 2 columns
- Desktop (1201-1800px): 3 columns
- Large desktop (1801px+): 4 columns

Each video element uses 16:9 aspect ratio with `object-fit: contain`.
