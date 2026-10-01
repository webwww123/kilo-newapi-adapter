package adapter

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultUpstreamBaseURL = "https://api.kilo.ai/api/gateway"
)

type Config struct {
	UpstreamBaseURL string
	UpstreamAPIKey  string
	MaxBodyBytes    int64
	HTTPClient      *http.Client

	RequestLogEnabled   bool
	RequestLogDir       string
	RequestLogRetention time.Duration
	RequestLogBodyLimit int64

	KiloAutoReliabilityEnabled    bool
	KiloAutoRequestCompatEnabled  bool
	KiloAutoTelemetryEnabled      bool
	TrustedNewAPIErrors           bool
	ContextRecallConfigPath       string
	StepToolsBundlePath           string
	ContextRecallMetadataOnly     bool
	ContextRecallResponseIdentity bool
}

func ConfigFromEnv() Config {
	cfg := Config{
		UpstreamBaseURL: strings.TrimSpace(os.Getenv("KILO_BASE_URL")),
		UpstreamAPIKey:  strings.TrimSpace(os.Getenv("KILO_API_KEY")),
		MaxBodyBytes:    parseInt64(os.Getenv("MAX_BODY_BYTES"), 10<<20),
		RequestLogEnabled: parseBool(
			os.Getenv("REQUEST_LOG_ENABLED"),
			true,
		),
		RequestLogDir:       strings.TrimSpace(os.Getenv("REQUEST_LOG_DIR")),
		RequestLogRetention: parseDuration(os.Getenv("REQUEST_LOG_RETENTION"), 48*time.Hour),
		RequestLogBodyLimit: parseInt64(os.Getenv("REQUEST_LOG_BODY_LIMIT"), 1<<20),
		KiloAutoReliabilityEnabled: parseBool(
			os.Getenv("KILO_AUTO_RELIABILITY_ENABLED"),
			false,
		),
		KiloAutoRequestCompatEnabled: parseBool(
			os.Getenv("KILO_AUTO_REQUEST_COMPAT_ENABLED"),
			false,
		),
		KiloAutoTelemetryEnabled:      parseBool(os.Getenv("KILO_AUTO_TELEMETRY_ENABLED"), false),
		ContextRecallConfigPath:       strings.TrimSpace(os.Getenv("CONTEXT_RECALL_CONFIG")),
		StepToolsBundlePath:           strings.TrimSpace(os.Getenv("CONTEXT_STEP_TOOLS_BUNDLE")),
		ContextRecallMetadataOnly:     parseBool(os.Getenv("CONTEXT_RECALL_METADATA_ONLY"), false),
		ContextRecallResponseIdentity: parseBool(os.Getenv("CONTEXT_RECALL_RESPONSE_IDENTITY"), false),
		TrustedNewAPIErrors: parseBool(
			os.Getenv("TRUSTED_NEWAPI_ERROR_DIAGNOSTICS"),
			false,
		),
		HTTPClient: &http.Client{
			Timeout: parseDuration(os.Getenv("UPSTREAM_TIMEOUT"), 180*time.Second),
		},
	}
	return cfg.WithDefaults()
}

func (cfg Config) WithDefaults() Config {
	if strings.TrimSpace(cfg.UpstreamBaseURL) == "" {
		cfg.UpstreamBaseURL = DefaultUpstreamBaseURL
	}
	cfg.UpstreamBaseURL = strings.TrimRight(cfg.UpstreamBaseURL, "/")

	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 10 << 20
	}
	if cfg.RequestLogDir == "" {
		cfg.RequestLogDir = "/var/log/kilo-newapi-adapter"
	}
	if cfg.RequestLogRetention <= 0 {
		cfg.RequestLogRetention = 48 * time.Hour
	}
	if cfg.RequestLogBodyLimit <= 0 {
		cfg.RequestLogBodyLimit = 1 << 20
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 180 * time.Second}
	}
	return cfg
}

func parseInt64(raw string, fallback int64) int64 {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func parseDuration(raw string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func parseBool(raw string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(raw))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}
