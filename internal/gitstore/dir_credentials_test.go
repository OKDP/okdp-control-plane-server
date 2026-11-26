package gitstore

import (
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

// mountSecret lays data out the way the kubelet mounts a Secret: the keys
// are symlinks into ..data, itself a symlink swapped atomically on update.
func mountSecret(t *testing.T, dir string, data map[string]string) {
	t.Helper()
	version, err := os.MkdirTemp(dir, "..version-")
	require.NoError(t, err)
	for key, value := range data {
		require.NoError(t, os.WriteFile(filepath.Join(version, key), []byte(value), 0o600))
	}
	tmp := filepath.Join(dir, "..data_tmp")
	require.NoError(t, os.Symlink(filepath.Base(version), tmp))
	require.NoError(t, os.Rename(tmp, filepath.Join(dir, "..data")))
	for key := range data {
		link := filepath.Join(dir, key)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			require.NoError(t, os.Symlink(filepath.Join("..data", key), link))
		}
	}
}

func TestCredentialsFromDir(t *testing.T) {
	url := "https://git.example.com/org/repo.git"
	ca := testCertPEM(t, "dir-ca")

	creds, err := CredentialsFromDir(url, filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err)
	assert.Nil(t, creds.Auth, "no mounted secret: anonymous")

	dir := t.TempDir()
	mountSecret(t, dir, map[string]string{"username": "bot", "password": "s3cret", "ca.crt": string(ca)})
	creds, err = CredentialsFromDir(url, dir)
	require.NoError(t, err)
	assert.Equal(t, &githttp.BasicAuth{Username: "bot", Password: "s3cret"}, creds.Auth)
	assert.Equal(t, ca, creds.CABundle)

	empty := t.TempDir()
	creds, err = CredentialsFromDir(url, empty)
	require.NoError(t, err)
	assert.Nil(t, creds.Auth, "an empty secret: anonymous")
}

func TestCredentialsFromDirReadsAtomicallyThroughDotDotDataAndSkipsTmpForSSH(t *testing.T) {
	// Ensure SSH known_hosts parsing does not write to TMPDIR when read from disk.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "nonexistent", "ro-tmp"))

	_, hostKey := newHostKey(t)
	identity, _ := newIdentity(t, "")
	kh := knownhosts.Line([]string{"git.example.com"}, hostKey) + "\n"

	dir := t.TempDir()
	mountSecret(t, dir, map[string]string{
		"identity":    string(identity),
		"known_hosts": kh,
	})
	// Place a stale file at top-level that is NOT in ..data to prove ..data is
	// resolved once and read atomically.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "password"), []byte("stale-passphrase"), 0o600))

	creds, err := CredentialsFromDir("ssh://git@git.example.com/org/repo.git", dir)
	require.NoError(t, err)
	require.NotNil(t, creds.Auth)
}

func TestCredentialsFromDirRejectsInvalidDotDotDataSymlinkAndInnerSymlinks(t *testing.T) {
	repoURL := "https://git.example.com/org/repo.git"
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "bearerToken"), []byte("leaked"), 0o600))

	for name, target := range map[string]string{
		"absolute target":   outside,
		"parent traversal":  "../" + filepath.Base(outside),
		"nested subpath":    "sub/version",
		"dot":               ".",
		"dotdot":            "..",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Symlink(target, filepath.Join(dir, "..data")))
			_, err := CredentialsFromDir(repoURL, dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid ..data")
		})
	}

	// A symlink planted inside the valid ..data target directory must be ignored (os.Lstat).
	dir := t.TempDir()
	mountSecret(t, dir, map[string]string{"username": "bot", "password": "s3cret"})
	target, err := os.Readlink(filepath.Join(dir, "..data"))
	require.NoError(t, err)
	require.NoError(t, os.Symlink(filepath.Join(outside, "bearerToken"), filepath.Join(dir, target, "bearerToken")))

	creds, err := CredentialsFromDir(repoURL, dir)
	require.NoError(t, err)
	assert.Equal(t, &githttp.BasicAuth{Username: "bot", Password: "s3cret"}, creds.Auth, "symlinked bearerToken inside ..data must not be followed")
}

func TestARotatedSecretAppliesWithoutANewStore(t *testing.T) {
	gitBin := requireGit(t)
	bare := newBare(t)
	runGit(t, bare, "config", "http.receivepack", "true")
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "bot" || pass != "rotated" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	url := server.URL + "/remote.git"

	dir := t.TempDir()
	mountSecret(t, dir, map[string]string{"username": "bot", "password": "expired"})
	s, err := New(Config{
		Repository:          gitops.Repository{URL: url, Path: "clusters/dev"},
		CredentialsProvider: DirCredentialsProvider(url, dir),
	})
	require.NoError(t, err)

	_, err = s.Latest(testCtx(t))
	require.ErrorIs(t, err, ErrUnavailable)

	mountSecret(t, dir, map[string]string{"username": "bot", "password": "rotated"})
	_, err = s.Bootstrap(testCtx(t))
	require.NoError(t, err)
	_, err = s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	require.NoError(t, err)
	assert.Equal(t, "a", showFile(t, bare, "clusters/dev/a.yaml"))
}

func TestAFailingCredentialsProviderNeverReachesTheRemote(t *testing.T) {
	bare := newBare(t)
	pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/.gitkeep": {}}, "init")
	calls := 0
	s, err := New(Config{
		Repository: gitops.Repository{URL: bare, Path: "clusters/dev"},
		CredentialsProvider: func() (Credentials, error) {
			calls++
			return Credentials{}, errors.New("secret unreadable")
		},
	})
	require.NoError(t, err)

	_, err = s.Latest(testCtx(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret unreadable")
	assert.Equal(t, 1, calls)

	_, err = s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUncertain)
}
