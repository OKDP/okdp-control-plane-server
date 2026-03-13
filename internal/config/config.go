package config

import (
	"os"
	"strings"
)

// Config holds the application configuration
type Config struct {
	ServerPort             string
	PlatformNamespace      string
	AllowedOrigins         string
	LogLevel               string
	KuboCDNamespace        string
	ContextName            string
	ContextNamespace       string
	ReleaseInterval        string
	ReleaseTimeout         string
	ExcludedSidecarPrefixes []string
}

const defaultSidecarPrefixes = "istio-proxy,istio-init,dynatrace-,linkerd-proxy,envoy,vault-agent"

// Load returns the configuration loaded from environment variables or defaults
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort:        getEnv("PORT", "8093"),
		PlatformNamespace: getEnv("PLATFORM_NAMESPACE", "okdp-system"),
		AllowedOrigins:    getEnv("ALLOWED_ORIGINS", "http://localhost:4200"),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
		KuboCDNamespace:   getEnv("KUBOCD_NAMESPACE", "kubocd-system"),
		ContextName:       getEnv("CONTEXT_NAME", "default"),
		ContextNamespace:  getEnv("CONTEXT_NAMESPACE", "kubocd-system"),
		ReleaseInterval:   getEnv("RELEASE_INTERVAL", "30m"),
		ReleaseTimeout:    getEnv("RELEASE_TIMEOUT", "10m"),
	}

	raw := getEnv("EXCLUDED_SIDECAR_PREFIXES", defaultSidecarPrefixes)
	for _, p := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cfg.ExcludedSidecarPrefixes = append(cfg.ExcludedSidecarPrefixes, trimmed)
		}
	}

	return cfg, nil
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}
