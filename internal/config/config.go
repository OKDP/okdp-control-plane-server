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
	// ControlPlaneContextName and ControlPlaneContextNamespace name the Control
	// Plane Context, which holds the service catalog.
	ControlPlaneContextName      string
	ControlPlaneContextNamespace string // defaults to ownNamespace()
	// PlatformContextName and PlatformContextNamespace name the platform
	// Context, holding shared infrastructure settings supplied to KuboCD
	// packages (ingress, OIDC, identity). The server only reads it. When both
	// name the same object as the Control Plane Context in kubernetes mode,
	// that single object holds both infrastructure settings and the service
	// catalog.
	PlatformContextName      string
	PlatformContextNamespace string // defaults to ControlPlaneContextNamespace
	// EngineType selects the deployment engine (ENGINE_TYPE). The engine reads
	// and validates its own section.
	EngineType              string
	ExcludedSidecarPrefixes []string
	// InsecureOCIRegistries lists registry hosts reached over plain HTTP
	// a development-sandbox affordance for local registries without TLS.
	InsecureOCIRegistries []string
	// RegistryAuthFile is a dockerconfigjson file with credentials for private package registries. Empty means anonymous access only.
	RegistryAuthFile string
	// OIDC configures the verification of the console's bearer token.
	OIDC OIDCConfig
}

type OIDCConfig struct {
	// It overrides the issuer the platform Context declares. Empty is the
	// normal case: the Context is where that setting lives.
	Issuer string
	// ClientID overrides the client id the platform Context declares.
	ClientID string
	// Disabled leaves the API open to anyone who can reach the port.
	Disabled bool
}

// DefaultEngineType is the engine of a server configured with no ENGINE_TYPE.
const DefaultEngineType = "kubocd"

const defaultSidecarPrefixes = "istio-proxy,istio-init,dynatrace-,linkerd-proxy,envoy,vault-agent"

// Load returns the configuration loaded from environment variables or defaults
func Load() (*Config, error) {
	legacyContextName := strings.TrimSpace(getEnv("CONTEXT_NAME", ""))
	legacyContextNamespace := strings.TrimSpace(getEnv("CONTEXT_NAMESPACE", ""))

	cfg := &Config{
		ServerPort:                   getEnv("PORT", "8093"),
		PlatformNamespace:            getEnv("PLATFORM_NAMESPACE", "okdp-system"),
		AllowedOrigins:               getEnv("ALLOWED_ORIGINS", "http://localhost:4200"),
		LogLevel:                     getEnv("LOG_LEVEL", "info"),
		KuboCDNamespace:              getEnv("KUBOCD_NAMESPACE", "kubocd-system"),
		ControlPlaneContextName:      strings.TrimSpace(getEnv("CONTROL_PLANE_CONTEXT_NAME", "")),
		ControlPlaneContextNamespace: strings.TrimSpace(getEnv("CONTROL_PLANE_CONTEXT_NAMESPACE", "")),
		PlatformContextName:          strings.TrimSpace(getEnv("PLATFORM_CONTEXT_NAME", "")),
		PlatformContextNamespace:     strings.TrimSpace(getEnv("PLATFORM_CONTEXT_NAMESPACE", "")),
		EngineType:                   strings.TrimSpace(getEnv("ENGINE_TYPE", "")),
		RegistryAuthFile:             getEnv("REGISTRY_AUTH_FILE", ""),
		OIDC: OIDCConfig{
			Issuer:   strings.TrimSpace(getEnv("OIDC_ISSUER", "")),
			ClientID: strings.TrimSpace(getEnv("OIDC_CLIENT_ID", "")),
			Disabled: getEnv("AUTH_DISABLED", "") == "true",
		},
	}

	if cfg.EngineType == "" {
		cfg.EngineType = DefaultEngineType
	}

	if cfg.ControlPlaneContextName == "" {
		cfg.ControlPlaneContextName = "control-plane"
	}
	if cfg.ControlPlaneContextNamespace == "" {
		cfg.ControlPlaneContextNamespace = legacyContextNamespace
	}
	if cfg.ControlPlaneContextNamespace == "" {
		cfg.ControlPlaneContextNamespace = ownNamespace()
	}

	if cfg.PlatformContextName == "" {
		cfg.PlatformContextName = legacyContextName
	}
	if cfg.PlatformContextName == "" {
		cfg.PlatformContextName = "platform"
	}
	if cfg.PlatformContextNamespace == "" {
		cfg.PlatformContextNamespace = legacyContextNamespace
	}
	if cfg.PlatformContextNamespace == "" {
		cfg.PlatformContextNamespace = cfg.ControlPlaneContextNamespace
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
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	return "okdp-system"
}
