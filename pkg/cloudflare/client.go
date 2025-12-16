package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	baseURL = "https://rtc.live.cloudflare.com/v1"
)

// Client handles communication with Cloudflare Calls API
type Client struct {
	appID      string
	apiToken   string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewClient creates a new Cloudflare Calls API client
func NewClient(appID, apiToken string, logger *slog.Logger) *Client {
	return &Client{
		appID:    appID,
		apiToken: apiToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

// CreateSession creates a new WebRTC session
func (c *Client) CreateSession(ctx context.Context) (*NewSessionResponse, error) {
	url := fmt.Sprintf("%s/apps/%s/sessions/new", baseURL, c.appID)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create session request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("create session failed: %s (status %d)", body, resp.StatusCode)
	}

	var sessionResp NewSessionResponse
	if err := json.Unmarshal(body, &sessionResp); err != nil {
		return nil, fmt.Errorf("decode session response: %w", err)
	}

	if sessionResp.ErrorCode != "" {
		return nil, fmt.Errorf("session creation error: %s - %s",
			sessionResp.ErrorCode, sessionResp.ErrorDesc)
	}

	c.logger.Info("created Cloudflare session", "session_id", sessionResp.SessionID)
	return &sessionResp, nil
}

// AddTracks adds media tracks to a session
func (c *Client) AddTracks(ctx context.Context, sessionID string, req *TracksRequest) (*TracksResponse, error) {
	url := fmt.Sprintf("%s/apps/%s/sessions/%s/tracks/new", baseURL, c.appID, sessionID)

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal tracks request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("add tracks request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("add tracks failed: %s (status %d)", body, resp.StatusCode)
	}

	var tracksResp TracksResponse
	if err := json.Unmarshal(body, &tracksResp); err != nil {
		return nil, fmt.Errorf("decode tracks response: %w", err)
	}

	if tracksResp.ErrorCode != "" {
		return nil, fmt.Errorf("tracks error: %s - %s",
			tracksResp.ErrorCode, tracksResp.ErrorDesc)
	}

	c.logger.Info("added tracks to session",
		"session_id", sessionID,
		"track_count", len(tracksResp.Tracks),
		"requires_renegotiation", tracksResp.RequiresImmediateRenegotiation)

	return &tracksResp, nil
}

// Renegotiate performs session renegotiation
func (c *Client) Renegotiate(ctx context.Context, sessionID string, req *RenegotiateRequest) (*RenegotiateResponse, error) {
	url := fmt.Sprintf("%s/apps/%s/sessions/%s/renegotiate", baseURL, c.appID, sessionID)

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal renegotiate request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("renegotiate request: %w", err)
	}
	defer resp.Body.Close()

	// Cloudflare returns 204 No Content on successful renegotiation
	if resp.StatusCode == http.StatusNoContent {
		c.logger.Info("renegotiated session", "session_id", sessionID)
		// Return empty response for 204 - renegotiation doesn't return SDP
		return &RenegotiateResponse{}, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("renegotiate failed: %s (status %d)", body, resp.StatusCode)
	}

	var renegResp RenegotiateResponse
	if err := json.Unmarshal(body, &renegResp); err != nil {
		return nil, fmt.Errorf("decode renegotiate response: %w", err)
	}

	if renegResp.ErrorCode != "" {
		return nil, fmt.Errorf("renegotiation error: %s - %s",
			renegResp.ErrorCode, renegResp.ErrorDesc)
	}

	c.logger.Info("renegotiated session", "session_id", sessionID)
	return &renegResp, nil
}

// CloseTracks closes media tracks in a session
func (c *Client) CloseTracks(ctx context.Context, sessionID string, req *CloseTracksRequest) (*CloseTracksResponse, error) {
	url := fmt.Sprintf("%s/apps/%s/sessions/%s/tracks/close", baseURL, c.appID, sessionID)

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal close tracks request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("close tracks request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("close tracks failed: %s (status %d)", body, resp.StatusCode)
	}

	var closeResp CloseTracksResponse
	if err := json.Unmarshal(body, &closeResp); err != nil {
		return nil, fmt.Errorf("decode close tracks response: %w", err)
	}

	if closeResp.ErrorCode != "" {
		return nil, fmt.Errorf("close tracks error: %s - %s",
			closeResp.ErrorCode, closeResp.ErrorDesc)
	}

	c.logger.Info("closed tracks",
		"session_id", sessionID,
		"track_count", len(closeResp.Tracks))

	return &closeResp, nil
}

// GetSessionState retrieves the current state of a session
func (c *Client) GetSessionState(ctx context.Context, sessionID string) (*GetSessionStateResponse, error) {
	url := fmt.Sprintf("%s/apps/%s/sessions/%s", baseURL, c.appID, sessionID)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get session state request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get session state failed: %s (status %d)", body, resp.StatusCode)
	}

	var stateResp GetSessionStateResponse
	if err := json.Unmarshal(body, &stateResp); err != nil {
		return nil, fmt.Errorf("decode session state response: %w", err)
	}

	if stateResp.ErrorCode != "" {
		return nil, fmt.Errorf("session state error: %s - %s",
			stateResp.ErrorCode, stateResp.ErrorDesc)
	}

	c.logger.Info("retrieved session state",
		"session_id", sessionID,
		"track_count", len(stateResp.Tracks))

	return &stateResp, nil
}

// AddTracksWithRetry adds tracks with automatic retry on transient failures
func (c *Client) AddTracksWithRetry(ctx context.Context, sessionID string, req *TracksRequest, maxRetries int) (*TracksResponse, error) {
	var lastErr error
	backoff := 100 * time.Millisecond
	maxBackoff := 10 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		resp, err := c.AddTracks(ctx, sessionID, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Exponential backoff with jitter
		if attempt < maxRetries-1 {
			delay := backoff
			if delay > maxBackoff {
				delay = maxBackoff
			}
			backoff *= 2

			c.logger.Warn("retrying add tracks",
				"attempt", attempt+1,
				"max_retries", maxRetries,
				"delay_ms", delay.Milliseconds(),
				"error", err)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	return nil, fmt.Errorf("max retries exceeded: %w", lastErr)
}
