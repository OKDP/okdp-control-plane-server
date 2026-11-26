package gitstore

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

// The tests run against throwaway bare repositories on disk. go-git's file
// transport runs git-upload-pack and git-receive-pack, so they need git.

func requireGit(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	require.NoError(t, err, "these tests need the git binary")
	return bin
}

// gitEnv isolates git from the user's configuration.
func gitEnv(t *testing.T) []string {
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home,
		"XDG_CONFIG_HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Direct Pusher",
		"GIT_AUTHOR_EMAIL=direct@example.com",
		"GIT_COMMITTER_NAME=Direct Pusher",
		"GIT_COMMITTER_EMAIL=direct@example.com",
	)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(requireGit(t), args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// newBare creates an empty bare repository, without any branch.
func newBare(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	runGit(t, ".", "init", "--quiet", "--bare", "--initial-branch=main", dir)
	return dir
}

// pushDirect commits files to branch of bare the way a user with a clone
// would, outside the store. A nil content deletes the file.
func pushDirect(t *testing.T, bare, branch string, files map[string][]byte, message string) string {
	t.Helper()
	work := t.TempDir()
	if runGitAllowFail(t, work, "clone", "--quiet", "--branch", branch, bare, ".") != nil {
		runGit(t, work, "init", "--quiet", "--initial-branch="+branch)
		runGit(t, work, "remote", "add", "origin", bare)
	}
	for p, content := range files {
		full := filepath.Join(work, filepath.FromSlash(p))
		if content == nil {
			runGit(t, work, "rm", "--quiet", p)
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, content, 0o644))
		runGit(t, work, "add", p)
	}
	runGit(t, work, "commit", "--quiet", "-m", message)
	runGit(t, work, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
	return runGit(t, work, "rev-parse", "HEAD")
}

func runGitAllowFail(t *testing.T, dir string, args ...string) error {
	cmd := exec.Command(requireGit(t), args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(t)
	return cmd.Run()
}

func headOf(t *testing.T, bare string) string {
	return runGit(t, bare, "rev-parse", "refs/heads/main")
}

func commitCount(t *testing.T, bare string) string {
	if runGitAllowFail(t, bare, "rev-parse", "--verify", "--quiet", "refs/heads/main") != nil {
		return "0"
	}
	return runGit(t, bare, "rev-list", "--count", "main")
}

func showFile(t *testing.T, bare, p string) string {
	return runGit(t, bare, "show", "main:"+p)
}

// fsck checks the objects the store wrote, tree entry order included.
func fsck(t *testing.T, bare string) {
	t.Helper()
	runGit(t, bare, "fsck", "--strict", "--no-dangling")
}

type storeOption func(*Config)

func withPath(p string) storeOption { return func(c *Config) { c.Repository.Path = p } }

func withAttempts(n int) storeOption { return func(c *Config) { c.MaxAttempts = n } }

func newStore(t *testing.T, url string, opts ...storeOption) *Store {
	t.Helper()
	requireGit(t)
	cfg := Config{
		Repository: gitops.Repository{URL: url, Branch: "main", Path: "clusters/dev"},
		Committer:  Author{Name: "OKDP Control Plane", Email: "control-plane@okdp.example"},
	}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	require.NoError(t, err)
	return s
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// barrier makes n stores reach their first push together, after each has
// planned its commit on the same head.
type barrier struct {
	wg sync.WaitGroup
	// replays counts the pushes after the first one, the proof that a store
	// had to replay its write.
	replays atomic.Int32
}

func newBarrier(n int) *barrier {
	b := &barrier{}
	b.wg.Add(n)
	return b
}

func (b *barrier) hook() func(int) {
	var once sync.Once
	return func(round int) {
		if round != 0 {
			b.replays.Add(1)
			return
		}
		once.Do(func() {
			b.wg.Done()
			b.wg.Wait()
		})
	}
}

func write(t *testing.T, s *Store, path, content string) *WriteResult {
	t.Helper()
	res, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: path, Content: []byte(content)}}})
	require.NoError(t, err)
	return res
}
