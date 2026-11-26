package gitstore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalPaths(t *testing.T) {
	assert.Equal(t, "contexts", ContextsDir())
	for _, c := range []struct {
		got  func() (string, error)
		want string
	}{
		{func() (string, error) { return ControlPlaneContextFile("control-plane") }, "contexts/control-plane.yaml"},
		{func() (string, error) { return ProjectDir("team-a") }, "projects/team-a"},
		{func() (string, error) { return NamespaceFile("team-a") }, "projects/team-a/namespace.yaml"},
		{func() (string, error) { return ReleasesDir("team-a") }, "projects/team-a/releases"},
		{func() (string, error) { return ReleaseFile("team-a", "spark.v2") }, "projects/team-a/releases/spark.v2.yaml"},
		{func() (string, error) { return ConnectionsDir("team-a") }, "projects/team-a/connections"},
		{func() (string, error) { return ConnectionFile("team-a", "pg") }, "projects/team-a/connections/pg.yaml"},
		{func() (string, error) { return SecretStoresDir("team-a") }, "projects/team-a/secret-stores"},
		{func() (string, error) { return SecretStoreFile("team-a", "vault") }, "projects/team-a/secret-stores/vault.yaml"},
		{func() (string, error) { return ExternalSecretsDir("team-a") }, "projects/team-a/external-secrets"},
		{func() (string, error) { return ExternalSecretFile("team-a", "db-creds") }, "projects/team-a/external-secrets/db-creds.yaml"},
	} {
		got, err := c.got()
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}
}

func TestAProjectNamedContextsDoesNotCollide(t *testing.T) {
	cp, err := ControlPlaneContextFile("control-plane")
	require.NoError(t, err)
	paths := []func() (string, error){
		func() (string, error) { return ProjectDir("contexts") },
		func() (string, error) { return NamespaceFile("contexts") },
		func() (string, error) { return ReleaseFile("contexts", "control-plane") },
	}
	for _, f := range paths {
		p, err := f()
		require.NoError(t, err)
		assert.False(t, strings.HasPrefix(p, ContextsDir()+"/"), p)
		assert.NotEqual(t, cp, p)
	}
}

func TestCanonicalPathsRefuseBadNames(t *testing.T) {
	for _, bad := range []string{"", "..", "../x", "a/b", "A", "team.a", "-a", "a_b", ".git", "a\nb"} {
		_, err := ProjectDir(bad)
		assert.ErrorIs(t, err, ErrInvalidPath, "project %q", bad)
		_, err = NamespaceFile(bad)
		assert.ErrorIs(t, err, ErrInvalidPath, "project %q", bad)
		_, err = SecretStoresDir(bad)
		assert.ErrorIs(t, err, ErrInvalidPath, "project %q", bad)
		_, err = ExternalSecretsDir(bad)
		assert.ErrorIs(t, err, ErrInvalidPath, "project %q", bad)
	}
	for _, bad := range []string{"", "..", "../x", "a/b", "A", ".hidden", "a..b", "x\\y", "a\nb"} {
		for name, f := range map[string]func(string) (string, error){
			"release":    func(n string) (string, error) { return ReleaseFile("team-a", n) },
			"connection": func(n string) (string, error) { return ConnectionFile("team-a", n) },
			"store":      func(n string) (string, error) { return SecretStoreFile("team-a", n) },
			"external":   func(n string) (string, error) { return ExternalSecretFile("team-a", n) },
			"cp context": ControlPlaneContextFile,
		} {
			_, err := f(bad)
			assert.ErrorIs(t, err, ErrInvalidPath, "%s %q", name, bad)
		}
	}
}

func TestObjectName(t *testing.T) {
	name, ok := ObjectName("spark.yaml")
	assert.True(t, ok)
	assert.Equal(t, "spark", name)
	for _, other := range []string{"README.md", ".gitkeep", "UPPER.yaml", ".yaml"} {
		_, ok := ObjectName(other)
		assert.False(t, ok, other)
	}
}
