package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	// Google
	ClientID     string
	ClientSecret string
	ProjectID    string
	RefreshToken string
	// Cloudflare
	AppID    string
	APIToken string
}

func loadEnv(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		switch key {
		case "client_id":
			cfg.ClientID = value
		case "client_secret":
			cfg.ClientSecret = value
		case "project_id":
			cfg.ProjectID = value
		case "refresh_token":
			cfg.RefreshToken = value
		case "app_id":
			cfg.AppID = value
		case "api_token":
			cfg.APIToken = value
		}
	}
	return cfg, nil
}

// Google OAuth2 token response
type GoogleTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// Cloudflare session response
type CloudflareSessionResponse struct {
	SessionID        string `json:"sessionId"`
	ErrorCode        string `json:"errorCode,omitempty"`
	ErrorDescription string `json:"errorDescription,omitempty"`
}

func verifyGoogle(cfg *Config) error {
	fmt.Println("\n=== Verifying Google OAuth2 ===")

	// Decode refresh token (it's URL encoded)
	refreshToken, err := url.QueryUnescape(cfg.RefreshToken)
	if err != nil {
		refreshToken = cfg.RefreshToken
	}

	data := url.Values{}
	data.Set("client_id", cfg.ClientID)
	data.Set("client_secret", cfg.ClientSecret)
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")

	req, err := http.NewRequest("POST", "https://www.googleapis.com/oauth2/v4/token", strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp GoogleTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	fmt.Printf("✓ Access token obtained (expires in %ds)\n", tokenResp.ExpiresIn)
	fmt.Printf("  Token type: %s\n", tokenResp.TokenType)
	fmt.Printf("  Token preview: %s...\n", tokenResp.AccessToken[:50])

	// Now try to list devices
	fmt.Println("\n=== Listing Nest Devices ===")
	devicesURL := fmt.Sprintf("https://smartdevicemanagement.googleapis.com/v1/enterprises/%s/devices", cfg.ProjectID)

	req, err = http.NewRequest("GET", devicesURL, nil)
	if err != nil {
		return fmt.Errorf("create devices request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)

	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("devices request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("devices status %d: %s", resp.StatusCode, string(body))
	}

	var devicesResp map[string]interface{}
	if err := json.Unmarshal(body, &devicesResp); err != nil {
		return fmt.Errorf("parse devices: %w", err)
	}

	devices, ok := devicesResp["devices"].([]interface{})
	if !ok {
		fmt.Println("✓ No devices found or empty response")
		fmt.Printf("  Raw: %s\n", string(body))
		return nil
	}

	fmt.Printf("✓ Found %d device(s)\n", len(devices))
	for i, d := range devices {
		dev := d.(map[string]interface{})
		name := dev["name"].(string)
		devType := dev["type"].(string)

		// Extract device ID from name
		parts := strings.Split(name, "/")
		deviceID := parts[len(parts)-1]

		fmt.Printf("  [%d] Type: %s\n", i+1, devType)
		fmt.Printf("      ID: %s\n", deviceID)

		// Check for camera traits
		if traits, ok := dev["traits"].(map[string]interface{}); ok {
			if liveStream, ok := traits["sdm.devices.traits.CameraLiveStream"].(map[string]interface{}); ok {
				if protocols, ok := liveStream["supportedProtocols"].([]interface{}); ok {
					fmt.Printf("      Protocols: %v\n", protocols)
				}
			}
		}
	}

	return nil
}

func verifyCloudflare(cfg *Config) error {
	fmt.Println("\n=== Verifying Cloudflare Calls ===")

	url := fmt.Sprintf("https://rtc.live.cloudflare.com/v1/apps/%s/sessions/new", cfg.AppID)

	// Empty request body - no sessionDescription needed for basic session creation
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var sessionResp CloudflareSessionResponse
	if err := json.Unmarshal(body, &sessionResp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	if sessionResp.ErrorCode != "" {
		return fmt.Errorf("error %s: %s", sessionResp.ErrorCode, sessionResp.ErrorDescription)
	}

	fmt.Printf("✓ Session created: %s\n", sessionResp.SessionID)

	return nil
}

func main() {
	fmt.Println("Nest → Cloudflare Relay - Connection Verification")
	fmt.Println("=" + strings.Repeat("=", 50))

	cfg, err := loadEnv(".env")
	if err != nil {
		fmt.Printf("✗ Failed to load .env: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nLoaded configuration:")
	fmt.Printf("  Google Project ID: %s\n", cfg.ProjectID)
	fmt.Printf("  Google Client ID: %s...%s\n", cfg.ClientID[:20], cfg.ClientID[len(cfg.ClientID)-10:])
	fmt.Printf("  Cloudflare App ID: %s\n", cfg.AppID)

	// Verify Google
	if err := verifyGoogle(cfg); err != nil {
		fmt.Printf("\n✗ Google verification failed: %v\n", err)
		os.Exit(1)
	}

	// Verify Cloudflare
	if err := verifyCloudflare(cfg); err != nil {
		fmt.Printf("\n✗ Cloudflare verification failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n" + strings.Repeat("=", 51))
	fmt.Println("✓ All connections verified successfully!")
	fmt.Println("  Ready to proceed with Phase 1 implementation")
}
