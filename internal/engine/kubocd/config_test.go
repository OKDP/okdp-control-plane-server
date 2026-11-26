package kubocd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestTheDefaultsAreTodaysBehaviour(t *testing.T) {
	cfg, err := kubocd.LoadConfig(env(nil))

	require.NoError(t, err)
	assert.Equal(t, kubocd.Config{
		Backend: "flux",
		Persistence: kubocd.PersistenceConfig{Mode: "kubernetes", Git: kubocd.GitConfig{
			Branch: "main", Sync: "flux", Initialize: true, CredentialsDir: "/etc/okdp/git", SyncNamespace: "okdp-system",
		}},
		ReleaseInterval: "30m0s",
		ReleaseTimeout:  "10m0s",
	}, cfg)
}

func TestEmptyEngineVariablesCountAsUnset(t *testing.T) {
	cfg, err := kubocd.LoadConfig(env(map[string]string{
		"ENGINE_KUBOCD_BACKEND": "", "ENGINE_KUBOCD_PERSISTENCE_MODE": " ",
	}))

	require.NoError(t, err)
	assert.Equal(t, "flux", cfg.Backend)
	assert.Equal(t, "kubernetes", cfg.Persistence.Mode)
}

func TestReleaseSettingsKeepTheirVariables(t *testing.T) {
	cfg, err := kubocd.LoadConfig(env(map[string]string{"RELEASE_INTERVAL": "5m", "RELEASE_TIMEOUT": ""}))

	require.NoError(t, err)
	assert.Equal(t, "5m0s", cfg.ReleaseInterval)
	assert.Equal(t, "", cfg.ReleaseTimeout, "set and empty writes an empty timeout, as it always did")
}

func TestInvalidSectionsNameTheFieldAndTheVariable(t *testing.T) {
	tests := map[string]struct {
		vars map[string]string
		want string
	}{
		"another backend": {map[string]string{"ENGINE_KUBOCD_BACKEND": "argocd"},
			`engine.kubocd.backend (ENGINE_KUBOCD_BACKEND) is "argocd": only "flux" is supported`},
		"git without a URL": {map[string]string{"ENGINE_KUBOCD_PERSISTENCE_MODE": "git"},
			`engine.kubocd.persistence.git.url (ENGINE_KUBOCD_PERSISTENCE_GIT_URL) is required with git persistence`},
		"flux without its objects": {map[string]string{
			"ENGINE_KUBOCD_PERSISTENCE_MODE":    "git",
			"ENGINE_KUBOCD_PERSISTENCE_GIT_URL": "https://forge.example/okdp/deployments.git",
		}, `ENGINE_KUBOCD_PERSISTENCE_GIT_REPOSITORY_NAME names the Flux GitRepository, required with sync "flux"`},
		"flux without its Kustomization": {map[string]string{
			"ENGINE_KUBOCD_PERSISTENCE_MODE":                "git",
			"ENGINE_KUBOCD_PERSISTENCE_GIT_URL":             "https://forge.example/okdp/deployments.git",
			"ENGINE_KUBOCD_PERSISTENCE_GIT_REPOSITORY_NAME": "okdp-control-plane-server",
		}, `ENGINE_KUBOCD_PERSISTENCE_GIT_KUSTOMIZATION_NAME names the Flux Kustomization, required with sync "flux"`},
		"another sync tool": {map[string]string{
			"ENGINE_KUBOCD_PERSISTENCE_MODE":     "git",
			"ENGINE_KUBOCD_PERSISTENCE_GIT_URL":  "https://forge.example/okdp/deployments.git",
			"ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC": "argocd",
		}, `engine.kubocd.persistence.git.sync (ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC) is "argocd": must be "flux" or "none"`},
		"initialize not a boolean": {map[string]string{"ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE": "maybe"},
			`engine.kubocd.persistence.git.initialize (ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE) is "maybe": must be true or false`},
		"an unknown persistence": {map[string]string{"ENGINE_KUBOCD_PERSISTENCE_MODE": "s3"},
			`engine.kubocd.persistence.mode (ENGINE_KUBOCD_PERSISTENCE_MODE) is "s3": must be "kubernetes" or "git"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := kubocd.LoadConfig(env(tt.vars))
			require.Error(t, err)
			assert.Equal(t, tt.want, err.Error())
		})
	}
}

// The controller stores durations in their canonical form and rewrites any
// other, which would move the Release generation behind the server's back.
func TestDurationsAreWrittenTheWayTheControllerStoresThem(t *testing.T) {
	tests := []struct{ interval, timeout, wantInterval, wantTimeout string }{
		{"30m", "10m", "30m0s", "10m0s"},
		{"1h", "90s", "1h0m0s", "1m30s"},
		{"1.5s", "250ms", "1.5s", "250ms"},
		// 1h0m0s does not match the timeout pattern of the CRD: kept as given.
		{"2h", "60m", "2h0m0s", "60m"},
		{"", " ", "", " "},
	}
	for _, tt := range tests {
		cfg, err := kubocd.LoadConfig(env(map[string]string{"RELEASE_INTERVAL": tt.interval, "RELEASE_TIMEOUT": tt.timeout}))
		require.NoError(t, err)
		assert.Equal(t, tt.wantInterval, cfg.ReleaseInterval, tt.interval)
		assert.Equal(t, tt.wantTimeout, cfg.ReleaseTimeout, tt.timeout)
	}
}

func TestAReleaseSettingThatIsNotADurationStopsTheServer(t *testing.T) {
	for variable, want := range map[string]string{
		"RELEASE_INTERVAL": `RELEASE_INTERVAL is "soon": not a duration (such as 30m)`,
		"RELEASE_TIMEOUT":  `RELEASE_TIMEOUT is "soon": not a duration (such as 30m)`,
	} {
		_, err := kubocd.LoadConfig(env(map[string]string{variable: "soon"}))
		require.Error(t, err)
		assert.Equal(t, want, err.Error())
	}
	for _, tc := range []struct {
		variable string
		value    string
		want     string
	}{
		{"RELEASE_INTERVAL", "-5m", "must be positive"},
		{"RELEASE_TIMEOUT", "-1m", "must be positive"},
		{"RELEASE_INTERVAL", "0s", "must be positive"},
		{"RELEASE_TIMEOUT", "0m", "must be positive"},
		{"RELEASE_TIMEOUT", "1h", "does not match the Release CRD duration format"},
		{"RELEASE_INTERVAL", "500us", "does not match the Release CRD duration format"},
	} {
		_, err := kubocd.LoadConfig(env(map[string]string{tc.variable: tc.value}))
		require.Error(t, err, "%s=%s", tc.variable, tc.value)
		assert.Contains(t, err.Error(), tc.want)
	}
}

func TestGitPersistenceReadsItsSection(t *testing.T) {
	cfg, err := kubocd.LoadConfig(env(map[string]string{
		"PLATFORM_NAMESPACE":                               "platform",
		"ENGINE_KUBOCD_PERSISTENCE_MODE":                   "git",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_URL":                "https://forge.example/okdp/deployments.git",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_PATH":               "clusters/prod",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_REPOSITORY_NAME":    "okdp-control-plane-server",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_KUSTOMIZATION_NAME": "okdp-control-plane-server",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_COMMITTER_NAME":     "Bot",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_COMMITTER_EMAIL":    "bot@example.com",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_CLEANUP_NAME":  "okdp-control-plane-server",
	}))

	require.NoError(t, err)
	assert.Equal(t, kubocd.GitConfig{
		URL:               "https://forge.example/okdp/deployments.git",
		Branch:            "main",
		Path:              "clusters/prod",
		Sync:              "flux",
		Initialize:        true,
		CredentialsDir:    "/etc/okdp/git",
		SyncNamespace:     "platform",
		GitRepositoryName: "okdp-control-plane-server",
		KustomizationName: "okdp-control-plane-server",
		CommitterName:     "Bot",
		CommitterEmail:    "bot@example.com",
	}, cfg.Persistence.Git)
	assert.Equal(t, "okdp-control-plane-server", cfg.SyncCleanupName)
}

func TestGitPersistenceWithoutASyncToolNeedsNoFluxObject(t *testing.T) {
	cfg, err := kubocd.LoadConfig(env(map[string]string{
		"ENGINE_KUBOCD_PERSISTENCE_MODE":                "git",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_URL":             "ssh://git@forge.example/okdp/deployments.git",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC":            "none",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_INITIALIZE":      "false",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_SYNC_NAMESPACE":  "flux-system",
		"ENGINE_KUBOCD_PERSISTENCE_GIT_CREDENTIALS_DIR": "/var/run/git",
	}))

	require.NoError(t, err)
	assert.Equal(t, "none", cfg.Persistence.Git.Sync)
	assert.False(t, cfg.Persistence.Git.Initialize)
	assert.Equal(t, "flux-system", cfg.Persistence.Git.SyncNamespace)
	assert.Equal(t, "/var/run/git", cfg.Persistence.Git.CredentialsDir)
}
