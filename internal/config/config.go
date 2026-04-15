package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAppEnv                      = "development"
	defaultServiceName                 = "explorer-api"
	defaultPort                        = "8080"
	defaultReadTimeout                 = 5 * time.Second
	defaultWriteTimeout                = 30 * time.Second
	defaultIdleTimeout                 = 60 * time.Second
	defaultHealthTimeout               = 2 * time.Second
	defaultShutdownTimeout             = 10 * time.Second
	defaultNATSURL                     = "nats://localhost:4222"
	defaultNATSConnectTimeout          = 5 * time.Second
	defaultPostgresDSN                 = "postgres://explorer:explorer@localhost:5432/explorer?sslmode=disable"
	defaultPostgresConnTimeout         = 5 * time.Second
	defaultBlobStorageDir              = "./blob-storage"
	defaultOpenAIBaseURL               = "https://api.openai.com/v1"
	defaultOpenAIResponsesURL          = "wss://api.openai.com/v1/responses"
	defaultOpenAIDialTimeout           = 10 * time.Second
	defaultOpenAIReadTimeout           = 65 * time.Second
	defaultOpenAIWriteTimeout          = 10 * time.Second
	defaultOpenAIPingInterval          = 30 * time.Second
	defaultOpenAIMaxMessageBytes int64 = 16 * 1024 * 1024
)

type Config struct {
	AppEnv          string
	ServiceName     string
	LogLevel        slog.Level
	HTTP            HTTPConfig
	ShutdownTimeout time.Duration
	NATS            NATSConfig
	Postgres        PostgresConfig
	Blob            BlobConfig
	OpenAI          OpenAIConfig
	EventRelayAddr  string
}

type HTTPConfig struct {
	Port          string
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
	IdleTimeout   time.Duration
	HealthTimeout time.Duration
}

type NATSConfig struct {
	URL            string
	ClientName     string
	ConnectTimeout time.Duration
}

type PostgresConfig struct {
	DSN            string
	ConnectTimeout time.Duration
}

type BlobConfig struct {
	StorageDir string
}

type OpenAIConfig struct {
	APIKey             string
	BaseURL            string
	ResponsesSocketURL string
	Organization       string
	Project            string
	DialTimeout        time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	PingInterval       time.Duration
	MaxMessageBytes    int64
}

func Load() (Config, error) {
	cfg := Config{
		AppEnv:      getEnv("APP_ENV", defaultAppEnv),
		ServiceName: getEnv("SERVICE_NAME", defaultServiceName),
		LogLevel:    ParseLogLevel(getEnv("LOG_LEVEL", "info")),
		HTTP: HTTPConfig{
			Port:          getEnv("PORT", defaultPort),
			ReadTimeout:   defaultReadTimeout,
			WriteTimeout:  defaultWriteTimeout,
			IdleTimeout:   defaultIdleTimeout,
			HealthTimeout: defaultHealthTimeout,
		},
		ShutdownTimeout: defaultShutdownTimeout,
		NATS: NATSConfig{
			URL:            getEnv("NATS_URL", defaultNATSURL),
			ConnectTimeout: defaultNATSConnectTimeout,
		},
		Postgres: PostgresConfig{
			DSN:            getEnv("POSTGRES_DSN", defaultPostgresDSN),
			ConnectTimeout: defaultPostgresConnTimeout,
		},
		Blob: BlobConfig{
			StorageDir: getEnv("BLOB_STORAGE_DIR", defaultBlobStorageDir),
		},
		OpenAI: OpenAIConfig{
			APIKey:             getEnv("OPENAI_API_KEY", ""),
			BaseURL:            getEnv("OPENAI_BASE_URL", defaultOpenAIBaseURL),
			ResponsesSocketURL: getEnv("OPENAI_RESPONSES_WS_URL", defaultOpenAIResponsesURL),
			Organization:       getEnv("OPENAI_ORG_ID", ""),
			Project:            getEnv("OPENAI_PROJECT_ID", ""),
			DialTimeout:        defaultOpenAIDialTimeout,
			ReadTimeout:        defaultOpenAIReadTimeout,
			WriteTimeout:       defaultOpenAIWriteTimeout,
			PingInterval:       defaultOpenAIPingInterval,
			MaxMessageBytes:    defaultOpenAIMaxMessageBytes,
		},
		EventRelayAddr: getEnv("EVENT_RELAY_ADDR", "wsserver:9090"),
	}

	var err error

	cfg.HTTP.ReadTimeout, err = durationFromEnv("HTTP_READ_TIMEOUT", cfg.HTTP.ReadTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.HTTP.WriteTimeout, err = durationFromEnv("HTTP_WRITE_TIMEOUT", cfg.HTTP.WriteTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.HTTP.IdleTimeout, err = durationFromEnv("HTTP_IDLE_TIMEOUT", cfg.HTTP.IdleTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.HTTP.HealthTimeout, err = durationFromEnv("HTTP_HEALTH_TIMEOUT", cfg.HTTP.HealthTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.ShutdownTimeout, err = durationFromEnv("SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.NATS.ConnectTimeout, err = durationFromEnv("NATS_CONNECT_TIMEOUT", cfg.NATS.ConnectTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.Postgres.ConnectTimeout, err = durationFromEnv("POSTGRES_CONNECT_TIMEOUT", cfg.Postgres.ConnectTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.OpenAI.DialTimeout, err = durationFromEnv("OPENAI_DIAL_TIMEOUT", cfg.OpenAI.DialTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.OpenAI.ReadTimeout, err = durationFromEnv("OPENAI_READ_TIMEOUT", cfg.OpenAI.ReadTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.OpenAI.WriteTimeout, err = durationFromEnv("OPENAI_WRITE_TIMEOUT", cfg.OpenAI.WriteTimeout)
	if err != nil {
		return Config{}, err
	}

	cfg.OpenAI.PingInterval, err = durationFromEnv("OPENAI_PING_INTERVAL", cfg.OpenAI.PingInterval)
	if err != nil {
		return Config{}, err
	}

	cfg.OpenAI.MaxMessageBytes, err = int64FromEnv("OPENAI_MAX_MESSAGE_BYTES", cfg.OpenAI.MaxMessageBytes)
	if err != nil {
		return Config{}, err
	}

	if _, err := strconv.Atoi(cfg.HTTP.Port); err != nil {
		return Config{}, fmt.Errorf("invalid PORT %q: %w", cfg.HTTP.Port, err)
	}

	if strings.TrimSpace(cfg.NATS.URL) == "" {
		return Config{}, fmt.Errorf("NATS_URL must not be empty")
	}

	if strings.TrimSpace(cfg.Postgres.DSN) == "" {
		return Config{}, fmt.Errorf("POSTGRES_DSN must not be empty")
	}

	if strings.TrimSpace(cfg.Blob.StorageDir) == "" {
		return Config{}, fmt.Errorf("BLOB_STORAGE_DIR must not be empty")
	}

	if strings.TrimSpace(cfg.OpenAI.BaseURL) == "" {
		return Config{}, fmt.Errorf("OPENAI_BASE_URL must not be empty")
	}

	if strings.TrimSpace(cfg.OpenAI.ResponsesSocketURL) == "" {
		return Config{}, fmt.Errorf("OPENAI_RESPONSES_WS_URL must not be empty")
	}

	if cfg.OpenAI.MaxMessageBytes == 0 || cfg.OpenAI.MaxMessageBytes < -1 {
		return Config{}, fmt.Errorf("OPENAI_MAX_MESSAGE_BYTES must be positive or -1")
	}

	cfg.NATS.ClientName = getEnv("NATS_CLIENT_NAME", cfg.ServiceName)

	return cfg, nil
}

func getEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	return value
}

func durationFromEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}

	return value, nil
}

func int64FromEnv(key string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}

	return value, nil
}

func ParseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
