package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config holds all credentials and configuration for the relay
type Config struct {
	Google     GoogleConfig
	Cloudflare CloudflareConfig
}

// GoogleConfig holds Google OAuth2 and SDM API credentials
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	ProjectID    string
	RefreshToken string
}

// CloudflareConfig holds Cloudflare Calls API credentials
type CloudflareConfig struct {
	AppID    string
	APIToken string
}

// Load reads configuration from a .env file
func Load(envPath string) (*Config, error) {
	file, err := os.Open(envPath)
	if err != nil {
		return nil, fmt.Errorf("open env file: %w", err)
	}
	defer file.Close()

	cfg := &Config{}
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse key=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// URL decode values that might be encoded
		decodedValue, err := url.QueryUnescape(value)
		if err != nil {
			// If decode fails, use original value
			decodedValue = value
		}

		switch key {
		case "client_id":
			cfg.Google.ClientID = decodedValue
		case "client_secret":
			cfg.Google.ClientSecret = decodedValue
		case "project_id":
			cfg.Google.ProjectID = decodedValue
		case "refresh_token":
			cfg.Google.RefreshToken = decodedValue
		case "app_id":
			cfg.Cloudflare.AppID = decodedValue
		case "api_token":
			cfg.Cloudflare.APIToken = decodedValue
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan env file: %w", err)
	}

	// Validate required fields
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that all required configuration fields are present
func (c *Config) Validate() error {
	if c.Google.ClientID == "" {
		return fmt.Errorf("missing client_id")
	}
	if c.Google.ClientSecret == "" {
		return fmt.Errorf("missing client_secret")
	}
	if c.Google.ProjectID == "" {
		return fmt.Errorf("missing project_id")
	}
	if c.Google.RefreshToken == "" {
		return fmt.Errorf("missing refresh_token")
	}
	if c.Cloudflare.AppID == "" {
		return fmt.Errorf("missing app_id")
	}
	if c.Cloudflare.APIToken == "" {
		return fmt.Errorf("missing api_token")
	}
	return nil
}
