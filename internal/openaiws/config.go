package openaiws

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	appconfig "explorer/internal/config"
)

type Config struct {
	APIKey               string
	BaseURL              string
	ResponsesSocketURL   string
	Organization         string
	Project              string
	DialTimeout          time.Duration
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	PingInterval         time.Duration
	MaxMessageBytes      int64
	MaxConcurrentSockets int
}

func FromAppConfig(cfg appconfig.OpenAIConfig) Config {
	return Config{
		APIKey:               cfg.APIKey,
		BaseURL:              cfg.BaseURL,
		ResponsesSocketURL:   cfg.ResponsesSocketURL,
		Organization:         cfg.Organization,
		Project:              cfg.Project,
		DialTimeout:          cfg.DialTimeout,
		ReadTimeout:          cfg.ReadTimeout,
		WriteTimeout:         cfg.WriteTimeout,
		PingInterval:         cfg.PingInterval,
		MaxMessageBytes:      cfg.MaxMessageBytes,
		MaxConcurrentSockets: cfg.MaxConcurrentSockets,
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("openai api key is required")
	}

	if strings.TrimSpace(c.ResponsesSocketURL) == "" {
		return fmt.Errorf("openai responses websocket url is required")
	}

	if c.DialTimeout <= 0 {
		return fmt.Errorf("openai dial timeout must be positive")
	}

	if c.ReadTimeout <= 0 {
		return fmt.Errorf("openai read timeout must be positive")
	}

	if c.WriteTimeout <= 0 {
		return fmt.Errorf("openai write timeout must be positive")
	}

	if c.PingInterval <= 0 {
		return fmt.Errorf("openai ping interval must be positive")
	}

	if c.MaxMessageBytes == 0 || c.MaxMessageBytes < -1 {
		return fmt.Errorf("openai max message bytes must be positive or -1")
	}

	if c.MaxConcurrentSockets <= 0 {
		return fmt.Errorf("openai max concurrent sockets must be positive")
	}

	return nil
}

func (c Config) HandshakeHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.APIKey)

	if org := strings.TrimSpace(c.Organization); org != "" {
		headers.Set("OpenAI-Organization", org)
	}

	if project := strings.TrimSpace(c.Project); project != "" {
		headers.Set("OpenAI-Project", project)
	}

	return headers
}
