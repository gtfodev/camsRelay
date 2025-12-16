package nest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	googleTokenURL = "https://www.googleapis.com/oauth2/v4/token"
	sdmBaseURL     = "https://smartdevicemanagement.googleapis.com/v1"
)

// Client handles authentication and communication with Google Nest API
type Client struct {
	clientID     string
	clientSecret string
	refreshToken string
	httpClient   *http.Client
	logger       *slog.Logger

	// Token cache
	mu          sync.RWMutex
	accessToken string
	tokenExpiry time.Time
}

// NewClient creates a new Nest API client
func NewClient(clientID, clientSecret, refreshToken string, logger *slog.Logger) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		refreshToken: refreshToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

// Device represents a Nest camera device
type Device struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	DeviceID  string   `json:"-"` // Extracted from Name
	Traits    Traits   `json:"traits"`
	Relations []Parent `json:"parentRelations"`
}

// Traits contains device capabilities
type Traits struct {
	Info struct {
		CustomName string `json:"customName"`
	} `json:"sdm.devices.traits.Info"`
	CameraLiveStream struct {
		VideoCodecs        []string `json:"videoCodecs"`
		AudioCodecs        []string `json:"audioCodecs"`
		SupportedProtocols []string `json:"supportedProtocols"`
	} `json:"sdm.devices.traits.CameraLiveStream"`
}

// Parent represents parent relations (rooms, structures)
type Parent struct {
	Parent      string `json:"parent"`
	DisplayName string `json:"displayName"`
}

// RTSPStream contains RTSP stream information
type RTSPStream struct {
	URL              string
	Token            string
	ExtensionToken   string
	ExpiresAt        time.Time
	ProjectID        string
	DeviceID         string
}

// getAccessToken returns a valid access token, refreshing if necessary
func (c *Client) getAccessToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	// Check if cached token is still valid (with 30s buffer)
	if time.Now().Add(30 * time.Second).Before(c.tokenExpiry) {
		token := c.accessToken
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	// Need to refresh token
	return c.refreshAccessToken(ctx)
}

// refreshAccessToken obtains a new access token using the refresh token
func (c *Client) refreshAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if time.Now().Add(30 * time.Second).Before(c.tokenExpiry) {
		return c.accessToken, nil
	}

	c.logger.Info("refreshing Google OAuth2 access token")

	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"refresh_token": {c.refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL,
		bytes.NewBufferString(data.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token refresh failed: %s (status %d)", body, resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
		TokenType   string `json:"token_type"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}

	c.accessToken = tokenResp.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	c.logger.Info("access token refreshed",
		"expires_at", c.tokenExpiry.Format(time.RFC3339))

	return c.accessToken, nil
}

// ListDevices retrieves all camera devices for the given project
func (c *Client) ListDevices(ctx context.Context, projectID string) ([]Device, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	uri := fmt.Sprintf("%s/enterprises/%s/devices", sdmBaseURL, projectID)
	req, err := http.NewRequestWithContext(ctx, "GET", uri, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list devices request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list devices failed: %s (status %d)", body, resp.StatusCode)
	}

	var devicesResp struct {
		Devices []Device `json:"devices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&devicesResp); err != nil {
		return nil, fmt.Errorf("decode devices response: %w", err)
	}

	// Filter for cameras only and extract device IDs
	cameras := make([]Device, 0, len(devicesResp.Devices))
	for _, device := range devicesResp.Devices {
		// Only include devices with camera streaming capabilities
		if len(device.Traits.CameraLiveStream.SupportedProtocols) == 0 {
			continue
		}

		// Extract device ID from name (format: enterprises/{project}/devices/{deviceId})
		deviceID := extractDeviceID(device.Name)
		if deviceID == "" {
			c.logger.Warn("failed to extract device ID", "name", device.Name)
			continue
		}
		device.DeviceID = deviceID

		cameras = append(cameras, device)
	}

	c.logger.Info("listed cameras", "count", len(cameras))
	return cameras, nil
}

// GenerateRTSPStream generates an RTSP stream URL for a camera
func (c *Client) GenerateRTSPStream(ctx context.Context, projectID, deviceID string) (*RTSPStream, error) {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	cmd := map[string]interface{}{
		"command": "sdm.devices.commands.CameraLiveStream.GenerateRtspStream",
		"params":  map[string]interface{}{},
	}

	body, err := json.Marshal(cmd)
	if err != nil {
		return nil, fmt.Errorf("marshal command: %w", err)
	}

	uri := fmt.Sprintf("%s/enterprises/%s/devices/%s:executeCommand",
		sdmBaseURL, projectID, deviceID)

	req, err := http.NewRequestWithContext(ctx, "POST", uri, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("generate stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("generate stream failed: %s (status %d)", body, resp.StatusCode)
	}

	var streamResp struct {
		Results struct {
			StreamURLs           map[string]string `json:"streamUrls"`
			StreamToken          string            `json:"streamToken"`
			StreamExtensionToken string            `json:"streamExtensionToken"`
			ExpiresAt            time.Time         `json:"expiresAt"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&streamResp); err != nil {
		return nil, fmt.Errorf("decode stream response: %w", err)
	}

	rtspURL, ok := streamResp.Results.StreamURLs["rtspUrl"]
	if !ok {
		return nil, fmt.Errorf("rtspUrl not found in response")
	}

	stream := &RTSPStream{
		URL:            rtspURL,
		Token:          streamResp.Results.StreamToken,
		ExtensionToken: streamResp.Results.StreamExtensionToken,
		ExpiresAt:      streamResp.Results.ExpiresAt,
		ProjectID:      projectID,
		DeviceID:       deviceID,
	}

	c.logger.Info("generated RTSP stream",
		"device_id", deviceID,
		"expires_at", stream.ExpiresAt.Format(time.RFC3339))

	return stream, nil
}

// ExtendRTSPStream extends an active RTSP stream
func (c *Client) ExtendRTSPStream(ctx context.Context, stream *RTSPStream) error {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cmd := map[string]interface{}{
		"command": "sdm.devices.commands.CameraLiveStream.ExtendRtspStream",
		"params": map[string]string{
			"streamExtensionToken": stream.ExtensionToken,
		},
	}

	body, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	uri := fmt.Sprintf("%s/enterprises/%s/devices/%s:executeCommand",
		sdmBaseURL, stream.ProjectID, stream.DeviceID)

	req, err := http.NewRequestWithContext(ctx, "POST", uri, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("extend stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("extend stream failed: %s (status %d)", body, resp.StatusCode)
	}

	var extendResp struct {
		Results struct {
			StreamExtensionToken string    `json:"streamExtensionToken"`
			StreamToken          string    `json:"streamToken"`
			ExpiresAt            time.Time `json:"expiresAt"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&extendResp); err != nil {
		return fmt.Errorf("decode extend response: %w", err)
	}

	// Update stream with new tokens and expiry
	stream.Token = extendResp.Results.StreamToken
	stream.ExtensionToken = extendResp.Results.StreamExtensionToken
	stream.ExpiresAt = extendResp.Results.ExpiresAt

	c.logger.Info("extended RTSP stream",
		"device_id", stream.DeviceID,
		"expires_at", stream.ExpiresAt.Format(time.RFC3339))

	return nil
}

// StopRTSPStream stops an active RTSP stream
func (c *Client) StopRTSPStream(ctx context.Context, stream *RTSPStream) error {
	token, err := c.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cmd := map[string]interface{}{
		"command": "sdm.devices.commands.CameraLiveStream.StopRtspStream",
		"params": map[string]string{
			"streamExtensionToken": stream.ExtensionToken,
		},
	}

	body, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	uri := fmt.Sprintf("%s/enterprises/%s/devices/%s:executeCommand",
		sdmBaseURL, stream.ProjectID, stream.DeviceID)

	req, err := http.NewRequestWithContext(ctx, "POST", uri, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("stop stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stop stream failed: %s (status %d)", body, resp.StatusCode)
	}

	c.logger.Info("stopped RTSP stream", "device_id", stream.DeviceID)
	return nil
}

// extractDeviceID extracts the device ID from the full device name
// Format: enterprises/{project}/devices/{deviceId}
func extractDeviceID(name string) string {
	const prefix = "enterprises/"
	const devicesPrefix = "/devices/"

	// Find the last occurrence of "/devices/"
	idx := len(name)
	for i := len(name) - 1; i >= 0; i-- {
		if i+len(devicesPrefix) <= len(name) && name[i:i+len(devicesPrefix)] == devicesPrefix {
			idx = i + len(devicesPrefix)
			break
		}
	}

	if idx >= len(name) {
		return ""
	}

	return name[idx:]
}
