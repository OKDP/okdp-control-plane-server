package gitstore

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBootstrapEmptyRemoteCreatesARootCommitOnce(t *testing.T) {
	bare := newBare(t)
	s := newStore(t, bare)

	first, err := s.Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.True(t, first.Created)
	assert.Equal(t, headOf(t, bare), first.Revision)
	assert.Equal(t, "1", commitCount(t, bare))
	assert.Equal(t, "clusters/dev/.gitkeep", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"))
	assert.Equal(t, "OKDP Control Plane <control-plane@okdp.example>", runGit(t, bare, "log", "-1", "--format=%an <%ae>", "main"))

	second, err := s.Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.False(t, second.Created)
	assert.Equal(t, first.Revision, second.Revision)
	assert.Equal(t, "1", commitCount(t, bare))

	// A fresh store, a restarted server, finds the directory too.
	third, err := newStore(t, bare).Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.False(t, third.Created)
	fsck(t, bare)
}

func TestBootstrapAtTheRepositoryRoot(t *testing.T) {
	bare := newBare(t)
	res, err := newStore(t, bare, withPath("")).Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.Equal(t, ".gitkeep", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"))
	fsck(t, bare)
}

func TestBootstrapCreatesTheBranchWhenOnlyAnotherOneExists(t *testing.T) {
	bare := newBare(t)
	other := pushDirect(t, bare, "other", map[string][]byte{"README.md": []byte("other\n")}, "other branch")

	res, err := newStore(t, bare).Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.Equal(t, "1", commitCount(t, bare))
	assert.Equal(t, other, runGit(t, bare, "rev-parse", "refs/heads/other"))
}

func TestBootstrapAddsTheMissingPathWithoutTouchingTheRest(t *testing.T) {
	bare := newBare(t)
	before := pushDirect(t, bare, "main", map[string][]byte{
		"README.md":           []byte("hello\n"),
		"clusters/prod/a.yml": []byte("a: 1\n"),
	}, "existing content")

	res, err := newStore(t, bare).Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.Equal(t, before, runGit(t, bare, "rev-parse", "main^"))
	assert.Equal(t, "README.md\nclusters/dev/.gitkeep\nclusters/prod/a.yml", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"))
	assert.Equal(t, "hello", showFile(t, bare, "README.md"))
	fsck(t, bare)
}

func TestBootstrapLeavesAnExistingPathAlone(t *testing.T) {
	bare := newBare(t)
	before := pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/team-a/namespace.yaml": []byte("kind: Namespace\n")}, "existing")

	res, err := newStore(t, bare).Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.False(t, res.Created)
	assert.Equal(t, before, res.Revision)
	assert.Equal(t, before, headOf(t, bare))
}

func TestBootstrapRefusesAPathThatIsAFile(t *testing.T) {
	bare := newBare(t)
	before := pushDirect(t, bare, "main", map[string][]byte{"clusters/dev": []byte("not a directory\n")}, "file")

	_, err := newStore(t, bare).Bootstrap(testCtx(t))
	assert.ErrorIs(t, err, ErrInvalidPath)
	assert.Equal(t, before, headOf(t, bare))
}

func TestConcurrentBootstrapsOnAnEmptyRemoteMakeOneCommit(t *testing.T) {
	bare := newBare(t)
	const replicas = 3
	b := newBarrier(replicas)
	results := make([]BootstrapResult, replicas)
	errs := make([]error, replicas)

	var wg sync.WaitGroup
	for i := range replicas {
		s := newStore(t, bare)
		s.beforePush = b.hook()
		// Replicas committing in the same second with the same identity
		// would produce the very same root commit, which is not a race.
		offset := time.Duration(i) * time.Hour
		s.now = func() time.Time { return time.Now().Add(offset) }
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.Bootstrap(testCtx(t))
		}()
	}
	wg.Wait()

	created := 0
	for i := range replicas {
		require.NoError(t, errs[i])
		assert.Equal(t, headOf(t, bare), results[i].Revision)
		if results[i].Created {
			created++
		}
	}
	assert.Equal(t, 1, created)
	assert.Equal(t, "1", commitCount(t, bare))
	assert.Zero(t, b.replays.Load(), "the losers find the directory there instead of pushing again")
	fsck(t, bare)
}

func TestBootstrapRacingAWriteKeepsBoth(t *testing.T) {
	bare := newBare(t)
	pushDirect(t, bare, "main", map[string][]byte{"README.md": []byte("hello\n")}, "existing")

	b := newBarrier(2)
	boot := newStore(t, bare)
	boot.beforePush = b.hook()
	writer := newStore(t, bare, withPath("apps"))
	writer.beforePush = b.hook()

	var wg sync.WaitGroup
	var bootErr, writeErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, bootErr = boot.Bootstrap(testCtx(t)) }()
	go func() {
		defer wg.Done()
		_, writeErr = writer.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "x.yaml", Content: []byte("x\n")}}})
	}()
	wg.Wait()

	require.NoError(t, bootErr)
	require.NoError(t, writeErr)
	assert.Equal(t, int32(1), b.replays.Load(), "the loser replays on the winner's commit")
	assert.Equal(t, "README.md\napps/x.yaml\nclusters/dev/.gitkeep", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"))
	fsck(t, bare)
}
