package config

import (
	"os"
	"strings"
)

// Config holds the application configuration
type Config struct {
	ServerPort        string
	PlatformNamespace string
	AllowedOrigins    string
	LogLevel          string
	KuboCDNamespace   string
	ContextName       string
	ContextNamespace  string // empty: the namespace the server runs in
	// The platform Context, the one package templates also read. Empty means it
	// is the same object as the Control Plane one, which is what a deployment
	// that has not split them yet looks like.
	ReleaseInterval         string
	ReleaseTimeout          string
	ExcludedSidecarPrefixes []string
	// InsecureOCIRegistries lists registry hosts reached over plain HTTP
	// a development-sandbox affordance for local registries without TLS.
	InsecureOCIRegistries []string
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
		ContextName:       getEnv("CONTEXT_NAME", "platform"),
		ContextNamespace:  getEnv("CONTEXT_NAMESPACE", ""),
		ReleaseInterval:   getEnv("RELEASE_INTERVAL", "30m"),
		ReleaseTimeout:    getEnv("RELEASE_TIMEOUT", "10m"),
	}

	if cfg.ContextNamespace == "" {
		cfg.ContextNamespace = ownNamespace()
	}

	for _, h := range strings.Split(getEnv("INSECURE_OCI_REGISTRIES", ""), ",") {
		if trimmed := strings.TrimSpace(h); trimmed != "" {
			cfg.InsecureOCIRegistries = append(cfg.InsecureOCIRegistries, trimmed)
		}
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

// ownNamespace resolves the namespace the server runs in, from the
// serviceaccount mount, falling back to POD_NAMESPACE then okdp-system.
func ownNamespace() string {
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return "okdp-system"
}
