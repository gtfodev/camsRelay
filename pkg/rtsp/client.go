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
	"time"

	"github.com/pion/rtp"
)

// Client represents an RTSP client for connecting to rtsps:// URLs
type Client struct {
	url      string
	logger   *slog.Logger
	conn     net.Conn
	reader   *bufio.Reader
	session  string
	cseq     int
	Channels map[byte]*Channel // channel ID -> Channel info (exported for access)

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
		url:      rtspURL,
		logger:   logger,
		Channels: make(map[byte]*Channel),
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
			port = "322"
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
		Timeout: 10 * time.Second,
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

	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 65536)

	c.logger.Info("connected to RTSP server", "remote_addr", conn.RemoteAddr())

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
func (c *Client) Play(ctx context.Context) error {
	req := c.newRequest("PLAY", c.url)
	req.Header["Range"] = "npt=0.000-"

	if _, err := c.do(req); err != nil {
		return fmt.Errorf("PLAY: %w", err)
	}

	c.logger.Info("RTSP PLAY started")
	return nil
}

// ReadPackets reads RTP packets from the interleaved stream
func (c *Client) ReadPackets(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Set read deadline
		if err := c.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return fmt.Errorf("set read deadline: %w", err)
		}

		// Read packet
		if err := c.readInterleavedPacket(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return fmt.Errorf("read packet: %w", err)
		}
	}
}

// Close closes the RTSP connection
func (c *Client) Close() error {
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
	// Build control URL
	u, _ := url.Parse(c.url)
	if !strings.HasPrefix(ch.Control, "rtsp://") {
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

	c.logger.Info("track setup complete",
		"channel", channelID,
		"type", ch.MediaType,
		"session", c.session)

	return nil
}

// readInterleavedPacket reads one interleaved RTP/RTCP packet
func (c *Client) readInterleavedPacket() error {
	// Read magic byte '$'
	magic, err := c.reader.ReadByte()
	if err != nil {
		return err
	}

	// If not '$', it might be RTSP response
	if magic != '$' {
		return fmt.Errorf("expected '$' but got %c", magic)
	}

	// Read channel ID
	channelID, err := c.reader.ReadByte()
	if err != nil {
		return err
	}

	// Read length (2 bytes, big endian)
	var lengthBuf [2]byte
	if _, err := io.ReadFull(c.reader, lengthBuf[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint16(lengthBuf[:])

	// Read payload
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return err
	}

	// Only process RTP packets (even channels)
	if channelID%2 == 0 {
		packet := &rtp.Packet{}
		if err := packet.Unmarshal(payload); err != nil {
			c.logger.Warn("failed to unmarshal RTP packet",
				"channel", channelID,
				"error", err)
			return nil
		}

		// Call handler if set
		if c.OnRTPPacket != nil {
			c.OnRTPPacket(channelID, packet)
		}
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
	if c.session != "" {
		req.Header["Session"] = c.session
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("%s %s RTSP/1.0\r\n", req.Method, req.URL))
	buf.WriteString(fmt.Sprintf("CSeq: %d\r\n", req.CSeq))

	for k, v := range req.Header {
		buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}

	buf.WriteString("\r\n")

	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}

	if _, err := c.conn.Write([]byte(buf.String())); err != nil {
		return err
	}

	c.logger.Debug("sent RTSP request", "method", req.Method, "url", req.URL)
	return nil
}

// readResponse reads an RTSP response
func (c *Client) readResponse() (*Response, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}

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
