package kubocd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Backends a KuboCD controller can turn a Release into.
const BackendFlux = "flux"

// Persistence modes of the desired state.
const (
	PersistenceKubernetes = "kubernetes"
	PersistenceGit        = "git"
)

// Sync tools that apply the git branch to the cluster. none commits only:
// something outside the server applies the branch.
const (
	SyncFlux = "flux"
	SyncNone = "none"
)

// DefaultGitCredentialsDir is where the chart mounts the git Secret.
const DefaultGitCredentialsDir = "/etc/okdp/git"

// Config is the kubocd section of the engine configuration, read from the
// environment.
type Config struct {
	// Backend is what the KuboCD controller produces from a Release
	// (ENGINE_KUBOCD_BACKEND). Only flux.
	Backend     string
	Persistence PersistenceConfig
	// ReleaseInterval and ReleaseTimeout are written on every Release
	// (RELEASE_INTERVAL, RELEASE_TIMEOUT).
	ReleaseInterval string
	ReleaseTimeout  string
	// SyncCleanupName names the Flux GitRepository and Kustomization the
	// chart renders in git mode
	// (ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_CLEANUP_NAME). A server in kubernetes
	// mode deletes them at startup when they carry the label
	// app.kubernetes.io/managed-by-control-plane with that value, so that
	// Flux stops applying a branch the server no longer writes.
	SyncCleanupName string
}

// PersistenceConfig says where the desired state is stored.
type PersistenceConfig struct {
	// Mode is kubernetes (the objects are written to the cluster) or git
	// (they are committed and a sync tool applies them)
	// (ENGINE_KUBOCD_PERSISTENCE_MODE).
	Mode string
	Git  GitConfig
}

// GitConfig is where git persistence commits the desired state and how the
// branch reaches the cluster (ENGINE_KUBOCD_PERSISTENCE_GIT_*).
type GitConfig struct {
	// URL is required in git mode (_URL).
	URL string
	// Branch is main by default (_BRANCH).
	Branch string
	// Path is the directory the server owns, the repository root when empty
	// (_PATH).
	Path string
	// Sync is flux (default) or none (_SYNC).
	Sync string
	// Initialize creates the branch layout and the control plane Context
	// when they are missing, true by default (_INITIALIZE).
	Initialize bool
	// CredentialsDir holds the git Secret mounted as files in the Flux
	// secretRef layout, read on every fetch and push (_CREDENTIALS_DIR,
	// /etc/okdp/git by default). Missing: anonymous access.
	CredentialsDir string
	// SyncNamespace holds the Flux objects (_SYNC_NAMESPACE,
	// PLATFORM_NAMESPACE by default).
	SyncNamespace string
	// GitRepositoryName and KustomizationName name the Flux objects, both
	// required with sync flux (_REPOSITORY_NAME, _KUSTOMIZATION_NAME).
	GitRepositoryName string
	KustomizationName string
	// CommitterName and CommitterEmail override the git committer identity
	// (_COMMITTER_NAME, _COMMITTER_EMAIL).
	CommitterName  string
	CommitterEmail string
}

// LoadConfig reads and validates the kubocd section. Without any variable it
// answers backend flux, kubernetes persistence, 30m0s interval and 10m0s
// timeout, and the git defaults (branch main, sync flux, initialize, the
// credentials in /etc/okdp/git, the Flux objects in PLATFORM_NAMESPACE).
// An empty ENGINE_KUBOCD_* variable counts as unset. The error names the field
// and its variable.
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	value := func(key, fallback string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}
	// An explicitly empty RELEASE_* variable writes an empty field on the Release.
	raw := func(key, fallback string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		return fallback
	}

	initialize, err := strconv.ParseBool(value("ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("engine.kubocd.persistence.git.initialize (ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE) is %q: must be true or false",
			value("ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE", ""))
	}
	cfg := Config{
		Backend: value("ENGINE_KUBOCD_BACKEND", BackendFlux),
		Persistence: PersistenceConfig{
			Mode: value("ENGINE_KUBOCD_PERSISTENCE_MODE", PersistenceKubernetes),
			Git: GitConfig{
				URL:               value("ENGINE_KUBOCD_PERSISTENCE_GIT_URL", ""),
				Branch:            value("ENGINE_KUBOCD_PERSISTENCE_GIT_BRANCH", "main"),
				Path:              value("ENGINE_KUBOCD_PERSISTENCE_GIT_PATH", ""),
				Sync:              value("ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC", SyncFlux),
				Initialize:        initialize,
				CredentialsDir:    value("ENGINE_KUBOCD_PERSISTENCE_GIT_CREDENTIALS_DIR", DefaultGitCredentialsDir),
				SyncNamespace:     value("ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_NAMESPACE", value("PLATFORM_NAMESPACE", "okdp-system")),
				GitRepositoryName: value("ENGINE_KUBOCD_PERSISTENCE_GIT_REPOSITORY_NAME", ""),
				KustomizationName: value("ENGINE_KUBOCD_PERSISTENCE_GIT_KUSTOMIZATION_NAME", ""),
				CommitterName:     value("ENGINE_KUBOCD_PERSISTENCE_GIT_COMMITTER_NAME", ""),
				CommitterEmail:    value("ENGINE_KUBOCD_PERSISTENCE_GIT_COMMITTER_EMAIL", ""),
			},
		},
		ReleaseInterval: raw("RELEASE_INTERVAL", "30m"),
		ReleaseTimeout:  raw("RELEASE_TIMEOUT", "10m"),
		SyncCleanupName: value("ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_CLEANUP_NAME", ""),
	}

	if cfg.Backend != BackendFlux {
		return Config{}, fmt.Errorf("engine.kubocd.backend (ENGINE_KUBOCD_BACKEND) is %q: only %q is supported", cfg.Backend, BackendFlux)
	}
	switch cfg.Persistence.Mode {
	case PersistenceKubernetes:
	case PersistenceGit:
		if err := cfg.Persistence.Git.validate(); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("engine.kubocd.persistence.mode (ENGINE_KUBOCD_PERSISTENCE_MODE) is %q: must be %q or %q",
			cfg.Persistence.Mode, PersistenceKubernetes, PersistenceGit)
	}

	if cfg.ReleaseInterval, err = canonicalDuration(cfg.ReleaseInterval, "RELEASE_INTERVAL", intervalPattern); err != nil {
		return Config{}, err
	}
	if cfg.ReleaseTimeout, err = canonicalDuration(cfg.ReleaseTimeout, "RELEASE_TIMEOUT", timeoutPattern); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (g GitConfig) validate() error {
	if g.URL == "" {
		return fmt.Errorf("engine.kubocd.persistence.git.url (ENGINE_KUBOCD_PERSISTENCE_GIT_URL) is required with git persistence")
	}
	switch g.Sync {
	case SyncNone:
	case SyncFlux:
		if g.GitRepositoryName == "" {
			return fmt.Errorf("ENGINE_KUBOCD_PERSISTENCE_GIT_REPOSITORY_NAME names the Flux GitRepository, required with sync %q", SyncFlux)
		}
		if g.KustomizationName == "" {
			return fmt.Errorf("ENGINE_KUBOCD_PERSISTENCE_GIT_KUSTOMIZATION_NAME names the Flux Kustomization, required with sync %q", SyncFlux)
		}
	default:
		return fmt.Errorf("engine.kubocd.persistence.git.sync (ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC) is %q: must be %q or %q", g.Sync, SyncFlux, SyncNone)
	}
	return nil
}

// The Release CRD patterns of spec.package.interval and spec.package.timeout.
var (
	intervalPattern = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`)
	timeoutPattern  = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?(ms|s|m))+$`)
)

// canonicalDuration writes a duration the way the KuboCD controller stores it
// (30m becomes 30m0s). The controller rewrites any other form, which bumps
// metadata.generation and leaves the revision a create answered stale. A
// canonical form the CRD pattern refuses (1h0m0s as a timeout) keeps the
// trimmed input when it matches the CRD pattern; zero, negative, or
// non-matching durations are rejected. An empty value is kept as given.
func canonicalDuration(value, variable string, pattern *regexp.Regexp) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, nil
	}
	parsed, err := time.ParseDuration(trimmed)
	if err != nil {
		return "", fmt.Errorf("%s is %q: not a duration (such as 30m)", variable, value)
	}
	if parsed <= 0 {
		return "", fmt.Errorf("%s is %q: duration must be positive", variable, value)
	}
	if canonical := parsed.String(); pattern.MatchString(canonical) {
		return canonical, nil
	}
	if pattern.MatchString(trimmed) {
		return trimmed, nil
	}
	return "", fmt.Errorf("%s is %q: does not match the Release CRD duration format", variable, value)
}
