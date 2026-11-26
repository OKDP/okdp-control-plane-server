package gitstore

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func keyedCreate(path, content, key string) WriteRequest {
	return WriteRequest{
		Changes:        []Change{{Path: path, Content: []byte(content), MustNotExist: true}},
		IdempotencyKey: key,
	}
}

func TestReplayingACreateAnswersTheFirstResult(t *testing.T) {
	bare, s := bootstrapped(t)
	first, err := s.Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "req-1"))
	require.NoError(t, err)
	assert.False(t, first.Replayed)
	assert.Contains(t, runGit(t, bare, "log", "-1", "--format=%B", "main"), "Okdp-Idempotency-Key: req-1")

	again, err := s.Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "req-1"))
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Revision, again.Revision)
	assert.Equal(t, first.Files, again.Files)

	// A restarted server, with an empty index, finds the key in git.
	restarted, err := newStore(t, bare).Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "req-1"))
	require.NoError(t, err)
	assert.True(t, restarted.Replayed)
	assert.Equal(t, first.Revision, restarted.Revision)
	assert.Equal(t, "2", commitCount(t, bare))
}

func TestReusingAKeyForAnotherRequestIsAConflict(t *testing.T) {
	bare, s := bootstrapped(t)
	_, err := s.Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "req-1"))
	require.NoError(t, err)
	head := headOf(t, bare)

	for name, req := range map[string]WriteRequest{
		"other content": keyedCreate("p/releases/a.yaml", "different\n", "req-1"),
		"other file":    keyedCreate("p/releases/b.yaml", "a\n", "req-1"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Write(testCtx(t), req)
			assert.ErrorIs(t, err, ErrIdempotencyConflict)
			assert.ErrorIs(t, err, ErrConflict)
		})
	}
	assert.Equal(t, head, headOf(t, bare))
}

func TestAKeyIsNotSharedByStoresRootedElsewhere(t *testing.T) {
	bare, a := bootstrapped(t, withPath("apps/a"))
	b := newStore(t, bare, withPath("apps/b"))
	_, err := a.Write(testCtx(t), keyedCreate("x.yaml", "x\n", "shared"))
	require.NoError(t, err)

	_, err = b.Write(testCtx(t), keyedCreate("x.yaml", "x\n", "shared"))
	assert.ErrorIs(t, err, ErrIdempotencyConflict, "same relative change, another file: never a replay")
	assert.Error(t, runGitAllowFail(t, bare, "cat-file", "-e", "main:apps/b/x.yaml"))
}

func TestTheDigestIgnoresTheOrderOfTheChanges(t *testing.T) {
	_, s := bootstrapped(t)
	req := WriteRequest{IdempotencyKey: "multi", Changes: []Change{
		{Path: "p/a.yaml", Content: []byte("a")},
		{Path: "p/b.yaml", Content: []byte("b")},
	}}
	first, err := s.Write(testCtx(t), req)
	require.NoError(t, err)
	req.Changes[0], req.Changes[1] = req.Changes[1], req.Changes[0]
	again, err := s.Write(testCtx(t), req)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Revision, again.Revision)
}

func TestAReplayAfterTheObjectChangedStillAnswersTheFirstResult(t *testing.T) {
	_, s := bootstrapped(t)
	first, err := s.Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "req-1"))
	require.NoError(t, err)
	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "p/releases/a.yaml", Delete: true}}})
	require.NoError(t, err)

	again, err := s.Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "req-1"))
	require.NoError(t, err, "not recreated, not reported as existing")
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Revision, again.Revision)

	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	_, err = snap.ReadFile("p/releases/a.yaml")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestAKeyedWriteThatChangesNothingIsRecorded(t *testing.T) {
	bare, s := bootstrapped(t)
	base := write(t, s, "p/namespace.yaml", "ns\n")
	req := WriteRequest{
		Changes:        []Change{{Path: "p/namespace.yaml", Content: []byte("ns\n"), ExpectedRevision: base.Revision}},
		IdempotencyKey: "noop",
	}
	first, err := s.Write(testCtx(t), req)
	require.NoError(t, err)
	assert.False(t, first.Unchanged)
	assert.Equal(t, headOf(t, bare), first.Revision, "an empty commit carries the key")
	assert.Equal(t, base.Revision, first.Files["p/namespace.yaml"])

	again, err := s.Write(testCtx(t), req)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Revision, again.Revision)
}

func TestAKeyIsFoundBehindALongHistory(t *testing.T) {
	bare, s := bootstrapped(t)
	first, err := s.Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "old-key"))
	require.NoError(t, err)
	fastImport(t, bare, 1500)
	require.Equal(t, "1502", commitCount(t, bare))

	again, err := newStore(t, bare).Write(testCtx(t), keyedCreate("p/releases/a.yaml", "a\n", "old-key"))
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Revision, again.Revision)
}

func TestConcurrentReplaysOfOneKeyLandOnce(t *testing.T) {
	bare, _ := bootstrapped(t)
	req := keyedCreate("p/releases/a.yaml", "a\n", "same-key")
	results, errs, _ := raceTwo(t, bare, req, req)
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, results[0].Revision, results[1].Revision)
	assert.NotEqual(t, results[0].Replayed, results[1].Replayed, "one wrote, the other replayed")
	assert.Equal(t, "2", commitCount(t, bare))
}

func TestTrailersAreReadFromTheLastParagraphOnly(t *testing.T) {
	key, digest, ok := parseTrailers("Create a\n\nOkdp-Idempotency-Key: k\nOkdp-Request-Digest: sha256:00\n")
	assert.True(t, ok)
	assert.Equal(t, "k", key)
	assert.Equal(t, "sha256:00", digest)

	_, _, ok = parseTrailers("Okdp-Idempotency-Key: k\nOkdp-Request-Digest: sha256:00\n")
	assert.False(t, ok, "a subject line is not a trailer block")
	_, _, ok = parseTrailers("x\n\nOkdp-Idempotency-Key: k\nOkdp-Request-Digest: d\n\nbody after\n")
	assert.False(t, ok)
}

// fastImport appends n commits touching other files to main.
func fastImport(t *testing.T, bare string, n int) {
	t.Helper()
	parent := headOf(t, bare)
	var b strings.Builder
	when := time.Now().Unix()
	for i := range n {
		content := fmt.Sprintf("n: %d\n", i)
		fmt.Fprintf(&b, "commit refs/heads/main\ncommitter Bulk <bulk@example.com> %d +0000\ndata 5\nbulk\n", when+int64(i))
		if i == 0 {
			fmt.Fprintf(&b, "from %s\n", parent)
		}
		fmt.Fprintf(&b, "M 100644 inline other/f%d.yaml\ndata %d\n%s\n", i%50, len(content), content)
	}
	cmd := exec.Command(requireGit(t), "fast-import", "--quiet")
	cmd.Dir = bare
	cmd.Env = gitEnv(t)
	cmd.Stdin = strings.NewReader(b.String())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}
