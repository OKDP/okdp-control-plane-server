package config

import (
	"errors"
	"os"
	"strings"
)

// Config holds the application configuration
type Config struct {
	ServerPort              string
	PlatformNamespace       string
	AllowedOrigins          string
	LogLevel                string
	ExcludedSidecarPrefixes []string
	// InsecureOCIRegistries lists registry hosts reached over plain HTTP
	// a development-sandbox affordance for local registries without TLS.
	InsecureOCIRegistries []string
	// OIDC configures the verification of the console's bearer token.
	OIDC OIDCConfig
	// GitOps configures the deployments repository, the only desired-state
	// store, and the engine reconciling it.
	GitOps GitOpsConfig
}

// Engines reconciling the deployments repository.
const (
	EngineFlux   = "flux"
	EngineArgoCD = "argocd"
)

type GitOpsConfig struct {
	// RepoURL is the deployments repository (https://, ssh:// or git@host:path).
	RepoURL string
	Branch  string
	// Path is the directory of the repository holding the layout, empty for the root.
	Path string
	// CredentialsDir holds the mounted credentials Secret (Flux GitRepository
	// keys: username/password, bearerToken, or identity/known_hosts).
	CredentialsDir string
	// InsecureIgnoreHostKey accepts any SSH host key. Sandboxes only.
	InsecureIgnoreHostKey bool
	AuthorName            string
	AuthorEmail           string
	// CloneDir is the local clone, a cache (an emptyDir); empty keeps it in memory.
	CloneDir string
	// Engine is flux or argocd: which objects carry the render/sync status.
	Engine string
	// ReleasesNamespace holds the Flux HelmReleases and the values ConfigMaps,
	// including okdp-platform-values.
	ReleasesNamespace string
	// ArgoCDNamespace holds the Argo CD Applications.
	ArgoCDNamespace string
}

// Validate refuses a configuration the server cannot run with.
func (g GitOpsConfig) Validate() error {
	if g.RepoURL == "" {
		return errors.New("GITOPS_REPO_URL is required: the deployments repository is the only desired-state store")
	}
	if g.Engine != EngineFlux && g.Engine != EngineArgoCD {
		return errors.New("GITOPS_ENGINE must be flux or argocd")
	}
	return nil
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

const defaultSidecarPrefixes = "istio-proxy,istio-init,dynatrace-,linkerd-proxy,envoy,vault-agent"

// Load returns the configuration loaded from environment variables or defaults
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort:        getEnv("PORT", "8093"),
		PlatformNamespace: getEnv("PLATFORM_NAMESPACE", "okdp-system"),
		AllowedOrigins:    getEnv("ALLOWED_ORIGINS", "http://localhost:4200"),
		LogLevel:          getEnv("LOG_LEVEL", "info"),
		OIDC: OIDCConfig{
			Issuer:   strings.TrimSpace(getEnv("OIDC_ISSUER", "")),
			ClientID: strings.TrimSpace(getEnv("OIDC_CLIENT_ID", "")),
			Disabled: getEnv("AUTH_DISABLED", "") == "true",
		},
		GitOps: GitOpsConfig{
			RepoURL:               strings.TrimSpace(getEnv("GITOPS_REPO_URL", "")),
			Branch:                getEnv("GITOPS_BRANCH", "main"),
			Path:                  strings.Trim(getEnv("GITOPS_PATH", ""), "/"),
			CredentialsDir:        getEnv("GITOPS_CREDENTIALS_DIR", "/etc/okdp/gitops-credentials"),
			InsecureIgnoreHostKey: getEnv("GITOPS_SSH_INSECURE_IGNORE_HOST_KEY", "") == "true",
			AuthorName:            getEnv("GITOPS_AUTHOR_NAME", "OKDP control plane"),
			AuthorEmail:           getEnv("GITOPS_AUTHOR_EMAIL", "okdp-control-plane@okdp.io"),
			CloneDir:              getEnv("GITOPS_CLONE_DIR", ""),
			Engine:                strings.ToLower(getEnv("GITOPS_ENGINE", EngineFlux)),
			ReleasesNamespace:     getEnv("GITOPS_RELEASES_NAMESPACE", "okdp-releases"),
			ArgoCDNamespace:       getEnv("ARGOCD_NAMESPACE", "argocd"),
		},
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
