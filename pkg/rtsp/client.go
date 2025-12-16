package rtsp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// Client represents an RTSP client for connecting to rtsps:// URLs
type Client struct {
	url     string
	baseURL string // Content-Base from DESCRIBE response (used for SETUP/PLAY)
	logger  *slog.Logger
	conn    net.Conn
	reader  *bufio.Reader
	session string
	cseq    int
	Channels map[byte]*Channel // channel ID -> Channel info (exported for access)

	// Keepalive management
	keepaliveInterval time.Duration
	keepaliveCancel   context.CancelFunc

	// Write synchronization (protect concurrent writes from keepalive goroutine)
	writeMu sync.Mutex

	// Callbacks
	OnRTPPacket func(channel byte, packet *rtp.Packet)
}

// Channel represents an RTP channel setup
type Channel struct {
	ID          byte
	MediaType   string // "video" or "audio"
	Control     string
	PayloadType uint8
}

// NewClient creates a new RTSP client
func NewClient(rtspURL string, logger *slog.Logger) *Client {
	return &Client{
		url:               rtspURL,
		logger:            logger,
		Channels:          make(map[byte]*Channel),
		keepaliveInterval: 25 * time.Second, // Default keepalive interval (go2rtc uses 25s)
	}
}

// Connect establishes connection to RTSP server
func (c *Client) Connect(ctx context.Context) error {
	u, err := url.Parse(c.url)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}

	// Extract credentials if present
	var username, password string
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}

	// Determine port
	port := u.Port()
	if port == "" {
		if u.Scheme == "rtsps" {
			port = "443" // Standard RTSPS port
		} else {
			port = "554"
		}
	}

	host := u.Hostname()
	addr := net.JoinHostPort(host, port)

	c.logger.Info("connecting to RTSP server",
		"scheme", u.Scheme,
		"host", host,
		"port", port)

	// Establish connection
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second, // Enable TCP keepalive
	}

	var conn net.Conn
	if u.Scheme == "rtsps" {
		tlsConfig := &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: false,
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	// Enable TCP_NODELAY to disable Nagle's algorithm (important for real-time streaming)
	// This ensures RTSP requests are sent immediately without buffering
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			c.logger.Warn("failed to set TCP_NODELAY", "error", err)
		} else {
			c.logger.Debug("TCP_NODELAY enabled")
		}
	} else if tlsConn, ok := conn.(*tls.Conn); ok {
		// For TLS connections, need to get underlying TCP connection
		if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			if err := tcpConn.SetNoDelay(true); err != nil {
				c.logger.Warn("failed to set TCP_NODELAY on TLS connection", "error", err)
			} else {
				c.logger.Debug("TCP_NODELAY enabled on TLS connection")
			}
		}
	}

	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 65536)

	c.logger.Info("connected to RTSP server",
		"remote_addr", conn.RemoteAddr(),
		"local_addr", conn.LocalAddr(),
		"tls", u.Scheme == "rtsps")

	// Perform RTSP handshake
	if err := c.options(ctx); err != nil {
		return fmt.Errorf("OPTIONS: %w", err)
	}

	if err := c.describe(ctx, username, password); err != nil {
		return fmt.Errorf("DESCRIBE: %w", err)
	}

	return nil
}

// SetupTracks sets up all available tracks
func (c *Client) SetupTracks(ctx context.Context) error {
	for channelID, ch := range c.Channels {
		if err := c.setupTrack(ctx, channelID, ch); err != nil {
			return fmt.Errorf("setup track %d: %w", channelID, err)
		}
	}
	return nil
}

// Play starts streaming
// Note: Unlike other methods, Play only sends the request without reading the response.
// The response will be handled in ReadPackets() loop, since the server immediately
// starts sending RTP packets after the PLAY response.
func (c *Client) Play(ctx context.Context) error {
	// Use baseURL (from Content-Base header) for PLAY, not the original URL
	// This is critical for Nest cameras - the Content-Base URL does NOT include
	// the ?auth= query parameter, and the server expects PLAY without it
	playURL := c.baseURL

	// Ensure URL path has trailing slash (matches ffmpeg behavior)
	if u, err := url.Parse(playURL); err == nil {
		if !strings.HasSuffix(u.Path, "/") {
			u.Path = u.Path + "/"
		}
		playURL = u.String()
	}

	req := c.newRequest("PLAY", playURL)

	// Range header is REQUIRED for Nest cameras to start streaming
	// Wire protocol analysis shows ffmpeg sends this and receives packets
	// Our client without this header gets zero packets after PLAY
	req.Header["Range"] = "npt=0.000-"

	// Only write the request, don't read response
	// The response will be read in ReadPackets() loop
	if err := c.writeRequest(req); err != nil {
		return fmt.Errorf("PLAY: %w", err)
	}

	// Start keepalive goroutine (critical for Nest cameras!)
	// This mimics go2rtc's behavior: send periodic OPTIONS to keep session alive
	c.startKeepalive(ctx)

	return nil
}

// startKeepalive starts background goroutine that sends periodic OPTIONS requests
// to keep the RTSP session alive. This is critical for Nest cameras which may
// not send packets without keepalive signals.
func (c *Client) startKeepalive(ctx context.Context) {
	keepaliveCtx, cancel := context.WithCancel(ctx)
	c.keepaliveCancel = cancel

	go func() {
		ticker := time.NewTicker(c.keepaliveInterval)
		defer ticker.Stop()

		c.logger.Info("keepalive goroutine started", "interval", c.keepaliveInterval)

		for {
			select {
			case <-keepaliveCtx.Done():
				c.logger.Info("keepalive goroutine stopped")
				return
			case <-ticker.C:
				// Send OPTIONS request to keep session alive
				c.logger.Info("sending keepalive OPTIONS")
				req := c.newRequest("OPTIONS", c.url)
				if err := c.writeRequest(req); err != nil {
					c.logger.Warn("keepalive OPTIONS write failed", "error", err)
					return
				}
				c.logger.Info("keepalive OPTIONS sent successfully")
			}
		}
	}()
}

// ReadPackets reads RTP packets from the interleaved stream
// This also handles RTSP responses that may be interleaved with RTP packets
// Based on go2rtc's handleTCPData implementation
func (c *Client) ReadPackets(ctx context.Context) error {
	c.logger.Info("starting packet read loop")
	packetCount := 0
	timeoutCount := 0
	playResponseReceived := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Set read deadline for this iteration
		// Use 10 seconds timeout (RTP packets should arrive frequently)
		if err := c.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}

		// Log buffered data BEFORE peek attempt (only first few times)
		if !playResponseReceived || packetCount < 3 {
			buffered := c.reader.Buffered()
			c.logger.Info("read loop iteration",
				"buffered_bytes", buffered,
				"play_response_received", playResponseReceived,
				"packet_count", packetCount)
		}

		// Peek at next 4 bytes to determine packet type
		// RTP/RTCP interleaved packet format:
		//   byte 0: '$' (0x24) magic byte
		//   byte 1: channel ID (0=video RTP, 1=video RTCP, 2=audio RTP, 3=audio RTCP)
		//   byte 2-3: payload size (big-endian uint16)
		//   byte 4+: payload data
		// RTSP response format:
		//   bytes 0-3: "RTSP"
		buf4, err := c.reader.Peek(4)
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.logger.Info("connection closed by server (EOF)", "packets_received", packetCount)
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				timeoutCount++
				if timeoutCount%6 == 1 { // Log every minute (6 * 10s timeout)
					c.logger.Warn("read timeout - no data from RTSP server",
						"timeout_count", timeoutCount,
						"packets_received", packetCount)
				}
				continue
			}
			return fmt.Errorf("peek: %w", err)
		}

		var channel byte
		var size uint16

		// Check if it's an interleaved RTP/RTCP packet (starts with '$')
		if buf4[0] != '$' {
			// Not an RTP packet - check if it's an RTSP response
			if string(buf4) == "RTSP" {
				// Read RTSP response (without setting deadline again)
				resp, err := c.readResponseNoDeadline()
				if err != nil {
					return fmt.Errorf("read RTSP response: %w", err)
				}

				// Handle PLAY response
				if !playResponseReceived {
					c.logger.Info("RTSP PLAY response received",
						"status", resp.StatusCode,
						"rtp_info", resp.Header["RTP-Info"],
						"range", resp.Header["Range"])
					playResponseReceived = true

					// Log what's buffered after PLAY response
					buffered := c.reader.Buffered()
					if buffered > 0 {
						c.logger.Info("data buffered after PLAY response", "bytes", buffered)
					} else {
						c.logger.Info("no buffered data after PLAY response - waiting for server to send packets")
					}
				} else {
					// This is likely a keepalive OPTIONS response
					c.logger.Debug("RTSP response in packet stream (likely keepalive)", "status", resp.StatusCode)
				}
				continue
			}

			// Unexpected data - log first 32 bytes for debugging
			peek, _ := c.reader.Peek(32)
			c.logger.Warn("unexpected data in stream (not '$' or 'RTSP')",
				"first_4_bytes", fmt.Sprintf("%q (hex: % x)", string(buf4), buf4),
				"peek_32", fmt.Sprintf("%q", string(peek)),
				"packets_so_far", packetCount)

			// Try to recover by searching for next '$' or "RTSP"
			if _, err := c.reader.ReadByte(); err != nil {
				return fmt.Errorf("discard unexpected byte: %w", err)
			}
			continue
		}

		// This is an interleaved RTP/RTCP packet
		// buf4 = ['$', channel, size_hi, size_lo]
		channel = buf4[1]
		size = binary.BigEndian.Uint16(buf4[2:4])

		// Discard the 4 peeked bytes (like go2rtc does)
		// This is the critical fix - we must consume the peeked bytes
		if _, err := c.reader.Discard(4); err != nil {
			return fmt.Errorf("discard header: %w", err)
		}

		// Read the RTP/RTCP payload
		payload := make([]byte, size)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			if errors.Is(err, io.EOF) {
				c.logger.Info("connection closed during packet read", "packets_received", packetCount)
				return nil
			}
			return fmt.Errorf("read payload: %w", err)
		}

		// Process RTP packets (even channels), ignore RTCP (odd channels)
		if channel%2 == 0 {
			packet := &rtp.Packet{}
			if err := packet.Unmarshal(payload); err != nil {
				c.logger.Warn("failed to unmarshal RTP packet",
					"channel", channel,
					"size", size,
					"error", err)
				continue
			}

			// Call handler if set
			if c.OnRTPPacket != nil {
				c.OnRTPPacket(channel, packet)
			}

			packetCount++
			if packetCount == 1 {
				c.logger.Info("received first RTP packet successfully")
			}
			if packetCount%1000 == 0 {
				c.logger.Info("packets received", "count", packetCount)
			}
		} else {
			// RTCP packet on odd channel
			c.logger.Debug("RTCP packet received",
				"channel", channel,
				"size", size)
		}
	}
}

// Close closes the RTSP connection
func (c *Client) Close() error {
	// Stop keepalive goroutine first
	if c.keepaliveCancel != nil {
		c.keepaliveCancel()
		c.keepaliveCancel = nil
	}

	if c.conn != nil {
		// Send TEARDOWN
		req := c.newRequest("TEARDOWN", c.url)
		_ = c.writeRequest(req)

		return c.conn.Close()
	}
	return nil
}

// options sends OPTIONS request
func (c *Client) options(ctx context.Context) error {
	req := c.newRequest("OPTIONS", c.url)
	resp, err := c.do(req)
	if err != nil {
		return err
	}

	c.logger.Debug("OPTIONS response",
		"public", resp.Header["Public"])

	return nil
}

// describe sends DESCRIBE request and parses SDP
func (c *Client) describe(ctx context.Context, username, password string) error {
	req := c.newRequest("DESCRIBE", c.url)
	req.Header["Accept"] = "application/sdp"

	// Add basic auth if credentials provided
	if username != "" {
		auth := username + ":" + password
		encoded := base64.StdEncoding.EncodeToString([]byte(auth))
		req.Header["Authorization"] = "Basic " + encoded
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}

	// Extract Content-Base header - this is the URL to use for SETUP/PLAY
	// The server may return a different base URL than the original request URL
	// (e.g., without query parameters like ?auth=)
	if contentBase := resp.Header["Content-Base"]; contentBase != "" {
		c.baseURL = strings.TrimSpace(contentBase)
		c.logger.Info("using Content-Base for subsequent requests",
			"original_url", c.url,
			"content_base", c.baseURL)
	} else {
		// Fallback to original URL if no Content-Base
		c.baseURL = c.url
	}

	// Parse SDP
	sdp := string(resp.Body)
	c.logger.Debug("received SDP", "sdp", sdp)

	if err := c.parseSDP(sdp); err != nil {
		return fmt.Errorf("parse SDP: %w", err)
	}

	return nil
}

// parseSDP parses SDP and extracts media information
func (c *Client) parseSDP(sdp string) error {
	lines := strings.Split(sdp, "\n")
	var currentMedia string
	var currentControl string
	var channelID byte = 0

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Media line: m=video 0 RTP/AVP 96
		if strings.HasPrefix(line, "m=") {
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				currentMedia = parts[0][2:] // "video" or "audio"
				currentControl = ""

				// Extract payload type
				var pt uint8
				if ptVal, err := strconv.Atoi(parts[3]); err == nil {
					pt = uint8(ptVal)
				}

				c.Channels[channelID] = &Channel{
					ID:          channelID,
					MediaType:   currentMedia,
					PayloadType: pt,
				}
				channelID += 2 // RTP on even, RTCP on odd
			}
		}

		// Control attribute: a=control:track1
		if strings.HasPrefix(line, "a=control:") {
			currentControl = strings.TrimPrefix(line, "a=control:")
			// Update the last added channel
			if len(c.Channels) > 0 {
				lastCh := c.Channels[channelID-2]
				lastCh.Control = currentControl
			}
		}
	}

	c.logger.Info("parsed SDP", "channels", len(c.Channels)/2)
	for id, ch := range c.Channels {
		if id%2 == 0 { // Only log RTP channels
			c.logger.Debug("media track",
				"channel", id,
				"type", ch.MediaType,
				"payload_type", ch.PayloadType,
				"control", ch.Control)
		}
	}

	return nil
}

// setupTrack sends SETUP request for a specific track
func (c *Client) setupTrack(ctx context.Context, channelID byte, ch *Channel) error {
	// Build control URL using baseURL (from Content-Base header)
	// This is critical for Nest cameras which return a different base URL
	u, _ := url.Parse(c.baseURL)
	if !strings.HasPrefix(ch.Control, "rtsp://") && !strings.HasPrefix(ch.Control, "rtsps://") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(ch.Control, "/")
	} else {
		u, _ = url.Parse(ch.Control)
	}

	controlURL := u.String()

	req := c.newRequest("SETUP", controlURL)
	req.Header["Transport"] = fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d",
		channelID, channelID+1)

	resp, err := c.do(req)
	if err != nil {
		return err
	}

	// Extract session ID from first SETUP
	if c.session == "" {
		session := resp.Header["Session"]
		if session != "" {
			// Session might be "123456;timeout=60"
			if idx := strings.IndexByte(session, ';'); idx > 0 {
				c.session = session[:idx]
			} else {
				c.session = session
			}
		}
	}

	// Log and validate Transport response
	transportResp := resp.Header["Transport"]
	c.logger.Info("track setup complete",
		"channel", channelID,
		"type", ch.MediaType,
		"session", c.session,
		"transport_request", fmt.Sprintf("RTP/AVP/TCP;unicast;interleaved=%d-%d", channelID, channelID+1),
		"transport_response", transportResp)

	// Warn if transport doesn't include expected interleaved parameters
	if transportResp == "" {
		c.logger.Warn("server returned empty Transport header - may not support interleaved TCP")
	} else if !strings.Contains(transportResp, "interleaved") {
		c.logger.Warn("server Transport response missing 'interleaved' - may have rejected TCP transport",
			"transport", transportResp)
	}

	return nil
}

// newRequest creates a new RTSP request
func (c *Client) newRequest(method, url string) *Request {
	c.cseq++
	return &Request{
		Method: method,
		URL:    url,
		Header: make(map[string]string),
		CSeq:   c.cseq,
	}
}

// do sends a request and reads response
func (c *Client) do(req *Request) (*Response, error) {
	if err := c.writeRequest(req); err != nil {
		return nil, err
	}

	return c.readResponse()
}

// writeRequest writes an RTSP request
func (c *Client) writeRequest(req *Request) error {
	// Lock to prevent concurrent writes from keepalive goroutine
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.session != "" {
		req.Header["Session"] = c.session
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("%s %s RTSP/1.0\r\n", req.Method, req.URL))
	buf.WriteString(fmt.Sprintf("CSeq: %d\r\n", req.CSeq))
	buf.WriteString("User-Agent: nest-cloudflare-relay/1.0\r\n")

	for k, v := range req.Header {
		buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	buf.WriteString("\r\n")

	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}

	requestStr := buf.String()
	if _, err := c.conn.Write([]byte(requestStr)); err != nil {
		return err
	}

	// Log full request for PLAY to debug
	if req.Method == "PLAY" {
		c.logger.Info("sent RTSP PLAY request",
			"method", req.Method,
			"url", req.URL,
			"session", c.session,
			"range", req.Header["Range"],
			"full_request", strings.ReplaceAll(requestStr, "\r\n", " | "))
	} else {
		c.logger.Debug("sent RTSP request", "method", req.Method, "url", req.URL)
	}
	return nil
}

// readResponse reads an RTSP response (sets its own deadline)
// Used by do() method for request/response pairs
func (c *Client) readResponse() (*Response, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return nil, err
	}
	return c.readResponseNoDeadline()
}

// readResponseNoDeadline reads an RTSP response without setting deadline
// Used by ReadPackets() which manages deadlines at the loop level
func (c *Client) readResponseNoDeadline() (*Response, error) {
	// Read status line
	statusLine, err := c.reader.ReadString('\n')
	if err != nil {
		return nil, err
	}

	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid status line: %s", statusLine)
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid status code: %s", parts[1])
	}

	resp := &Response{
		StatusCode: statusCode,
		Header:     make(map[string]string),
	}

	// Read headers
	var contentLength int
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break
		}

		if idx := strings.IndexByte(line, ':'); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			resp.Header[key] = value

			if key == "Content-Length" {
				contentLength, _ = strconv.Atoi(value)
			}
		}
	}

	// Read body if present
	if contentLength > 0 {
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(c.reader, body); err != nil {
			return nil, err
		}
		resp.Body = body
	}

	if statusCode != 200 {
		return nil, fmt.Errorf("RTSP error: %d", statusCode)
	}

	return resp, nil
}

// Request represents an RTSP request
type Request struct {
	Method string
	URL    string
	Header map[string]string
	CSeq   int
}

// Response represents an RTSP response
type Response struct {
	StatusCode int
	Header     map[string]string
	Body       []byte
}
