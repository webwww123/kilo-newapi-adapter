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
	DefaultPublicModel     = "v1chat"
	DefaultTargetModel     = "nvidia/nemotron-3-super-120b-a12b:free"
)

type Config struct {
	UpstreamBaseURL string
	UpstreamAPIKey  string
	DefaultModel    string
	ModelAliases    map[string]string
	MaxBodyBytes    int64
	HTTPClient      *http.Client
}

func ConfigFromEnv() Config {
	cfg := Config{
		UpstreamBaseURL: strings.TrimSpace(os.Getenv("KILO_BASE_URL")),
		UpstreamAPIKey:  strings.TrimSpace(os.Getenv("KILO_API_KEY")),
		DefaultModel:    strings.TrimSpace(os.Getenv("DEFAULT_MODEL")),
		ModelAliases:    ParseAliases(os.Getenv("MODEL_ALIASES")),
		MaxBodyBytes:    parseInt64(os.Getenv("MAX_BODY_BYTES"), 10<<20),
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

	if strings.TrimSpace(cfg.DefaultModel) == "" {
		cfg.DefaultModel = DefaultPublicModel
	}
	if cfg.ModelAliases == nil {
		cfg.ModelAliases = map[string]string{}
	}
	if _, ok := cfg.ModelAliases[DefaultPublicModel]; !ok {
		cfg.ModelAliases[DefaultPublicModel] = DefaultTargetModel
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 10 << 20
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 180 * time.Second}
	}
	return cfg
}

func ParseAliases(raw string) map[string]string {
	aliases := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, target, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		target = strings.TrimSpace(target)
		if name != "" && target != "" {
			aliases[name] = target
		}
	}
	return aliases
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
