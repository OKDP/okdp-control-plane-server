package gitstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

func bootstrapped(t *testing.T, opts ...storeOption) (string, *Store) {
	t.Helper()
	bare := newBare(t)
	s := newStore(t, bare, opts...)
	_, err := s.Bootstrap(testCtx(t))
	require.NoError(t, err)
	return bare, s
}

func TestCreateReadUpdateDeleteWithTheCallerAsAuthor(t *testing.T) {
	bare, s := bootstrapped(t)
	ctx := testCtx(t)
	path, err := ReleaseFile("team-a", "spark")
	require.NoError(t, err)
	alice := AuthorFromIdentity(&auth.Identity{Subject: "sub-1", Username: "alice@example.com"}, s.cfg.Committer)

	created, err := s.Write(ctx, WriteRequest{
		Changes: []Change{{Path: path, Content: []byte("v: 1\n"), MustNotExist: true}},
		Author:  alice,
		Message: "Create spark in team-a",
	})
	require.NoError(t, err)
	assert.Equal(t, headOf(t, bare), created.Revision)
	assert.Equal(t, map[string]string{path: created.Revision}, created.Files)
	assert.Equal(t, "alice@example.com <alice@example.com>|OKDP Control Plane <control-plane@okdp.example>",
		runGit(t, bare, "log", "-1", "--format=%an <%ae>|%cn <%ce>", "main"))
	assert.Equal(t, "Create spark in team-a", runGit(t, bare, "log", "-1", "--format=%B", "main"))

	snap, err := s.Latest(ctx)
	require.NoError(t, err)
	content, err := snap.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "v: 1\n", string(content))
	rev, err := snap.FileRevision(path)
	require.NoError(t, err)
	assert.Equal(t, created.Revision, rev)
	entries, err := snap.ReadDir("projects/team-a/releases")
	require.NoError(t, err)
	assert.Equal(t, []Entry{{Name: "spark.yaml", Kind: KindFile}}, entries)

	_, err = s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Content: []byte("v: 9\n"), MustNotExist: true}}})
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.ErrorIs(t, err, ErrAlreadyExists)
	assert.Equal(t, created.Revision, conflict.Actual)

	updated, err := s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Content: []byte("v: 2\n"), ExpectedRevision: created.Revision}}})
	require.NoError(t, err)
	assert.Equal(t, "v: 2", showFile(t, bare, "clusters/dev/"+path))
	assert.Equal(t, "OKDP Control Plane", runGit(t, bare, "log", "-1", "--format=%an", "main"), "no caller identity: the control plane is the author")

	_, err = s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Content: []byte("v: 3\n"), ExpectedRevision: created.Revision}}})
	require.ErrorAs(t, err, &conflict)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, created.Revision, conflict.Expected)
	assert.Equal(t, updated.Revision, conflict.Actual)
	assert.Equal(t, "v: 2", showFile(t, bare, "clusters/dev/"+path))

	deleted, err := s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Delete: true, ExpectedRevision: updated.Revision}}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{path: ""}, deleted.Files)
	snap, err = s.Latest(ctx)
	require.NoError(t, err)
	_, err = snap.ReadFile(path)
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = snap.ReadDir("projects")
	assert.ErrorIs(t, err, ErrNotFound, "a deletion drops the directories it empties")

	_, err = s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Delete: true}}})
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Content: []byte("x"), ExpectedRevision: updated.Revision}}})
	require.ErrorAs(t, err, &conflict)
	assert.Empty(t, conflict.Actual, "expected a revision of a file that is gone")

	bob := AuthorFromIdentity(&auth.Identity{Subject: "sub-2", Username: "bob"}, s.cfg.Committer)
	_, err = s.Write(ctx, WriteRequest{Changes: []Change{{Path: path, Content: []byte("v: 4\n")}}, Author: bob})
	require.NoError(t, err)
	assert.Equal(t, "bob <>", runGit(t, bare, "log", "-1", "--format=%an <%ae>", "main"), "a username that is not an email leaves the email empty")
	fsck(t, bare)
}

func TestAWriteOnAnotherFileDoesNotMoveTheRevisionOfAFile(t *testing.T) {
	bare, s := bootstrapped(t)
	a := write(t, s, "team-a/namespace.yaml", "a\n")
	write(t, s, "team-b/namespace.yaml", "b\n")

	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	rev, err := snap.FileRevision("team-a/namespace.yaml")
	require.NoError(t, err)
	assert.Equal(t, a.Revision, rev)
	assert.NotEqual(t, headOf(t, bare), rev)

	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "team-a/namespace.yaml", Content: []byte("a2\n"), ExpectedRevision: a.Revision}}})
	require.NoError(t, err, "the head moved, team-a/namespace.yaml did not")
}

func TestDeletingTheLastFileKeepsTheStoreDirectory(t *testing.T) {
	bare := newBare(t)
	pushDirect(t, bare, "main", map[string][]byte{
		"README.md":                               []byte("hello\n"),
		"clusters/dev/projects/a/namespace.yaml":  []byte("a\n"),
		"clusters/dev/projects/a/releases/r.yaml": []byte("r\n"),
	}, "existing content without .gitkeep")
	s := newStore(t, bare)

	res, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{
		{Path: "projects/a/namespace.yaml", Delete: true},
		{Path: "projects/a/releases/r.yaml", Delete: true},
	}})
	require.NoError(t, err)
	assert.Equal(t, headOf(t, bare), res.Revision)
	assert.Equal(t, "README.md\nclusters/dev/.gitkeep", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"),
		"the store directory stays for the sync tool")
	fsck(t, bare)

	// At the repository root nothing needs keeping.
	rootBare := newBare(t)
	pushDirect(t, rootBare, "main", map[string][]byte{"only.yaml": []byte("x\n")}, "one file")
	_, err = newStore(t, rootBare, withPath("")).Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "only.yaml", Delete: true}}})
	require.NoError(t, err)
	assert.Empty(t, runGit(t, rootBare, "ls-tree", "-r", "--name-only", "main"))
}

func TestDeleteTreeRemovesWhatTheDirectoryHoldsWhenTheWriteLands(t *testing.T) {
	bare := newBare(t)
	pushDirect(t, bare, "main", map[string][]byte{
		"clusters/dev/projects/a/namespace.yaml":  []byte("a\n"),
		"clusters/dev/projects/a/releases/r.yaml": []byte("r\n"),
		"clusters/dev/projects/b/namespace.yaml":  []byte("b\n"),
	}, "two projects")
	s := newStore(t, bare)
	var once sync.Once
	s.beforePush = func(int) {
		once.Do(func() {
			pushDirect(t, bare, "main", map[string][]byte{
				"clusters/dev/projects/a/connections/c.yaml": []byte("c\n"),
			}, "a file added while the deletion is in flight")
		})
	}

	res, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "projects/a", DeleteTree: true}}})
	require.NoError(t, err)
	assert.Equal(t, headOf(t, bare), res.Revision)
	assert.Equal(t, "", res.Files["projects/a"])
	assert.Equal(t, "clusters/dev/projects/b/namespace.yaml", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"))
	fsck(t, bare)

	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "projects/a", DeleteTree: true}}})
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "projects/b/namespace.yaml", DeleteTree: true}}})
	assert.ErrorIs(t, err, ErrInvalidPath)
}

func TestAnUnchangedWriteMakesNoCommit(t *testing.T) {
	bare, s := bootstrapped(t)
	first := write(t, s, "p/namespace.yaml", "same\n")
	write(t, s, "q/namespace.yaml", "other\n")
	head := headOf(t, bare)

	res, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "p/namespace.yaml", Content: []byte("same\n"), ExpectedRevision: first.Revision}}})
	require.NoError(t, err)
	assert.True(t, res.Unchanged)
	assert.Equal(t, head, res.Revision)
	assert.Equal(t, first.Revision, res.Files["p/namespace.yaml"], "the file kept its own revision")
	assert.Equal(t, head, headOf(t, bare))
}

func TestAMultiFileWriteIsAllOrNothing(t *testing.T) {
	bare, s := bootstrapped(t)
	ns := write(t, s, "p/namespace.yaml", "ns\n")
	write(t, s, "p/namespace.yaml", "ns2\n")
	head := headOf(t, bare)

	_, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{
		{Path: "p/releases/a.yaml", Content: []byte("a\n"), MustNotExist: true},
		{Path: "p/namespace.yaml", Content: []byte("ns3\n"), ExpectedRevision: ns.Revision},
	}})
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, head, headOf(t, bare))

	res, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{
		{Path: "p/releases/a.yaml", Content: []byte("a\n"), MustNotExist: true},
		{Path: "p/connections/c.yaml", Content: []byte("c\n"), MustNotExist: true},
	}})
	require.NoError(t, err)
	assert.Equal(t, res.Revision, res.Files["p/releases/a.yaml"])
	assert.Equal(t, res.Revision, res.Files["p/connections/c.yaml"])
	assert.Equal(t, head, runGit(t, bare, "rev-parse", "main^"), "one commit for both files")
}

func TestInvalidRequestsAreRefusedBeforeAnything(t *testing.T) {
	bare, s := bootstrapped(t)
	head := headOf(t, bare)
	rev := head
	cases := map[string]WriteRequest{
		"no change":           {},
		"twice the same path": {Changes: []Change{{Path: "a"}, {Path: "a"}}},
		"nested paths":        {Changes: []Change{{Path: "a/b.yaml"}, {Path: "a"}}},
		"nested deeper":       {Changes: []Change{{Path: "a/b"}, {Path: "a/b.yaml"}, {Path: "a/b/c"}}},
		"delete with content": {Changes: []Change{{Path: "a", Delete: true, Content: []byte("x")}}},
		"delete a create":     {Changes: []Change{{Path: "a", Delete: true, MustNotExist: true}}},
		"delete a tree twice": {Changes: []Change{{Path: "a", DeleteTree: true, Delete: true}}},
		"delete a tree with if-match": {
			Changes: []Change{{Path: "a", DeleteTree: true, ExpectedRevision: rev}},
		},
		"create with if-match": {
			Changes: []Change{{Path: "a", MustNotExist: true, ExpectedRevision: rev}},
		},
		"short revision":         {Changes: []Change{{Path: "a", ExpectedRevision: rev[:7]}}},
		"key with a newline":     {Changes: []Change{{Path: "a"}}, IdempotencyKey: "k\nOkdp-Request-Digest: x"},
		"key with a space":       {Changes: []Change{{Path: "a"}}, IdempotencyKey: "k k"},
		"trailer in the message": {Changes: []Change{{Path: "a"}}, Message: "x\n\nOkdp-Idempotency-Key: forged"},
		"author with brackets":   {Changes: []Change{{Path: "a"}}, Author: Author{Name: "Eve <x@y>"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.Write(testCtx(t), req)
			assert.ErrorIs(t, err, ErrInvalidRequest)
		})
	}
	assert.Equal(t, head, headOf(t, bare))
}

func TestPathTraversalIsRefusedEverywhere(t *testing.T) {
	bare, s := bootstrapped(t)
	head := headOf(t, bare)
	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)

	for _, p := range []string{
		"../escape", "a/../../b", "/etc/passwd", "a//b", "a/./b", ".", "..", "a/", "a\\b",
		".git/config", "a/.GIT/hooks", "a\x00b", "",
		"x\n\nOkdp-Idempotency-Key: victim\nOkdp-Request-Digest: forged", "a\rb", "a\tb", "a\x7fb",
	} {
		t.Run(p, func(t *testing.T) {
			_, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: p, Content: []byte("x")}}})
			assert.ErrorIs(t, err, ErrInvalidPath)
			_, err = snap.ReadFile(p)
			assert.ErrorIs(t, err, ErrInvalidPath)
			if p != "" {
				_, err = snap.ReadDir(p)
				assert.ErrorIs(t, err, ErrInvalidPath)
				_, err = snap.FileRevision(p)
				assert.ErrorIs(t, err, ErrInvalidPath)
				_, err = New(Config{Repository: gitops.Repository{URL: bare, Path: p}})
				assert.ErrorIs(t, err, ErrInvalidPath)
			}
		})
	}
	assert.Equal(t, head, headOf(t, bare))
}

func TestFilesAndDirectoriesNeverReplaceEachOther(t *testing.T) {
	bare, s := bootstrapped(t)
	write(t, s, "p/releases/a.yaml", "a\n")
	write(t, s, "p/namespace.yaml", "ns\n")
	head := headOf(t, bare)

	for _, p := range []string{"p/releases", "p/namespace.yaml/x.yaml", "p"} {
		_, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: p, Content: []byte("x")}}})
		assert.ErrorIs(t, err, ErrInvalidPath, p)
		_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: p, Delete: true}}})
		assert.Error(t, err, p)
	}
	assert.Equal(t, head, headOf(t, bare))
	assert.Equal(t, "a", showFile(t, bare, "clusters/dev/p/releases/a.yaml"))
}

func TestSymlinksAndModesInTheTree(t *testing.T) {
	bare, s := bootstrapped(t)
	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", bare, ".")
	dir := filepath.Join(work, "clusters", "dev", "p")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "script.sh"), []byte("same\n"), 0o644))
	require.NoError(t, os.Symlink("../../../README.md", filepath.Join(dir, "link.yaml")))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "symlink and file")
	added := runGit(t, work, "rev-parse", "HEAD")
	require.NoError(t, os.Chmod(filepath.Join(dir, "script.sh"), 0o755))
	runGit(t, work, "commit", "--quiet", "-am", "chmod only")
	chmod := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "push", "--quiet", "origin", "HEAD:main")

	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	_, err = snap.ReadFile("p/link.yaml")
	assert.ErrorIs(t, err, ErrInvalidPath, "never read through a symlink")
	entries, err := snap.ReadDir("p")
	require.NoError(t, err)
	assert.Contains(t, entries, Entry{Name: "link.yaml", Kind: KindOther})
	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "p/link.yaml", Content: []byte("x")}}})
	assert.ErrorIs(t, err, ErrInvalidPath, "never write over a symlink")

	rev, err := snap.FileRevision("p/script.sh")
	require.NoError(t, err)
	assert.Equal(t, chmod, rev, "a mode change is a change")
	assert.NotEqual(t, added, rev)

	res := write(t, s, "p/script.sh", "new\n")
	assert.Equal(t, "100755", runGit(t, bare, "ls-tree", "main", "clusters/dev/p/script.sh")[:6], "an update keeps the executable bit")
	assert.Equal(t, res.Revision, headOf(t, bare))
}

func TestRevisionsOfDeletedRecreatedAndUnchangedFiles(t *testing.T) {
	_, s := bootstrapped(t)
	write(t, s, "p/a.yaml", "a\n")
	_, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "p/a.yaml", Delete: true}}})
	require.NoError(t, err)
	recreated := write(t, s, "p/a.yaml", "a\n")
	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	rev, err := snap.FileRevision("p/a.yaml")
	require.NoError(t, err)
	assert.Equal(t, recreated.Revision, rev, "same content as before the deletion, a new revision")

	b := write(t, s, "p/b.yaml", "b\n")
	mixed, err := s.Write(testCtx(t), WriteRequest{Changes: []Change{
		{Path: "p/a.yaml", Content: []byte("a2\n")},
		{Path: "p/b.yaml", Content: []byte("b\n")},
	}})
	require.NoError(t, err)
	assert.Equal(t, mixed.Revision, mixed.Files["p/a.yaml"])
	assert.Equal(t, b.Revision, mixed.Files["p/b.yaml"], "the unchanged file keeps its own revision")
}

func TestAFileTurnedIntoASymlinkOrASubmoduleChangesItsRevision(t *testing.T) {
	bare, s := bootstrapped(t)
	file := write(t, s, "p/x.yaml", "target")
	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", bare, ".")
	x := filepath.Join(work, "clusters", "dev", "p", "x.yaml")
	require.NoError(t, os.Remove(x))
	// The symlink's blob is its target: the same bytes as the file.
	require.NoError(t, os.Symlink("target", x))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "symlink")
	link := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "update-index", "--add", "--cacheinfo", "160000,"+file.Revision+",clusters/dev/p/sub")
	runGit(t, work, "commit", "--quiet", "-m", "submodule")
	runGit(t, work, "push", "--quiet", "origin", "HEAD:main")
	assert.Equal(t, runGit(t, bare, "rev-parse", "main:clusters/dev/p/x.yaml"), runGit(t, bare, "rev-parse", file.Revision+":clusters/dev/p/x.yaml"))

	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	rev, err := snap.FileRevision("p/x.yaml")
	require.NoError(t, err)
	assert.Equal(t, link, rev)
	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "p/x.yaml", Content: []byte("y"), ExpectedRevision: file.Revision}}})
	assert.Error(t, err)

	_, err = snap.ReadFile("p/sub")
	assert.ErrorIs(t, err, ErrInvalidPath)
	_, err = snap.ReadFile("p/sub/inside.yaml")
	assert.ErrorIs(t, err, ErrNotFound, "never read through a submodule")
	for _, p := range []string{"p/sub", "p/sub/inside.yaml"} {
		_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: p, Content: []byte("x")}}})
		assert.ErrorIs(t, err, ErrInvalidPath, p)
	}
}

func TestFileRevisionThroughMerges(t *testing.T) {
	bare, s := bootstrapped(t)
	write(t, s, "x.yaml", "x0\n")
	write(t, s, "y.yaml", "y0\n")
	write(t, s, "z.yaml", "z0\n")

	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", bare, ".")
	edit := func(p, content, msg string) string {
		require.NoError(t, os.WriteFile(filepath.Join(work, "clusters", "dev", p), []byte(content), 0o644))
		runGit(t, work, "commit", "--quiet", "-am", msg)
		return runGit(t, work, "rev-parse", "HEAD")
	}
	runGit(t, work, "checkout", "--quiet", "-b", "side")
	sideX := edit("x.yaml", "x1\n", "side changes x")
	edit("z.yaml", "z-side\n", "side changes z")
	runGit(t, work, "checkout", "--quiet", "main")
	mainY := edit("y.yaml", "y1\n", "main changes y")
	edit("z.yaml", "z-main\n", "main changes z")
	// The merge conflicts on z and resolves it to a third content.
	_ = runGitAllowFail(t, work, "merge", "--quiet", "--no-edit", "side")
	require.NoError(t, os.WriteFile(filepath.Join(work, "clusters", "dev", "z.yaml"), []byte("z-merged\n"), 0o644))
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "--no-edit")
	merge := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "push", "--quiet", "origin", "main")

	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	for p, want := range map[string]string{"x.yaml": sideX, "y.yaml": mainY, "z.yaml": merge} {
		rev, err := snap.FileRevision(p)
		require.NoError(t, err)
		assert.Equal(t, want, rev, p)
	}

	_, err = s.Write(testCtx(t), WriteRequest{Changes: []Change{{Path: "x.yaml", Content: []byte("x2\n"), ExpectedRevision: sideX}}})
	require.NoError(t, err, "If-Match with the revision read after a merge")
}

func TestReadsOnAnEmptyRemote(t *testing.T) {
	s := newStore(t, newBare(t))
	snap, err := s.Latest(testCtx(t))
	require.NoError(t, err)
	assert.Empty(t, snap.Revision())
	_, err = snap.ReadFile("a.yaml")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = snap.ReadDir("")
	assert.ErrorIs(t, err, ErrNotFound)
	ok, err := snap.Exists("")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestAtAndIsAncestor(t *testing.T) {
	bare, s := bootstrapped(t)
	first := write(t, s, "a.yaml", "1\n")
	second := write(t, s, "a.yaml", "2\n")

	// Another store has never seen these commits: At fetches them.
	other := newStore(t, bare)
	snap, err := other.At(testCtx(t), first.Revision)
	require.NoError(t, err)
	content, err := snap.ReadFile("a.yaml")
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(content))

	ok, err := other.IsAncestor(testCtx(t), first.Revision, second.Revision)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = other.IsAncestor(testCtx(t), second.Revision, first.Revision)
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = other.At(testCtx(t), "0123456789012345678901234567890123456789")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = other.At(testCtx(t), "main")
	assert.ErrorIs(t, err, ErrInvalidRequest)
}

func TestAuthorFromIdentity(t *testing.T) {
	cp := Author{Name: "OKDP Control Plane", Email: "control-plane@okdp.io"}
	assert.Equal(t, cp, AuthorFromIdentity(nil, cp))
	assert.Equal(t, cp, AuthorFromIdentity(&auth.Identity{}, cp))
	assert.Equal(t, Author{Name: "alice"}, AuthorFromIdentity(&auth.Identity{Subject: "s", Username: "alice"}, cp))
	assert.Equal(t, Author{Name: "bob", Email: "bob@corp.example"}, AuthorFromIdentity(&auth.Identity{Subject: "s", Username: "bob", Email: "bob@corp.example"}, cp))
	assert.Equal(t, Author{Name: "bob", Email: "bob@corp.example"}, AuthorFromIdentity(&auth.Identity{Subject: "s", Username: "bob\r\t", Email: "bob\x1b@corp.example"}, cp))
	assert.Equal(t, Author{Name: "a@b.c", Email: "a@b.c"}, AuthorFromIdentity(&auth.Identity{Username: "a@b.c"}, cp))
	assert.Equal(t, Author{Name: "alice @ team"}, AuthorFromIdentity(&auth.Identity{Username: "alice @ team"}, cp), "a username containing whitespace is not copied into email")
	assert.Equal(t, Author{Name: "sub-1"}, AuthorFromIdentity(&auth.Identity{Subject: "sub-1"}, cp))
}

func TestAuthorRejectsControlCharactersAndBlankName(t *testing.T) {
	bare, s := bootstrapped(t)
	assert.NotEmpty(t, DefaultCommitter.Email)

	for _, bad := range []Author{
		{Name: "   "},
		{Email: "only-email@example.com"},
		{Name: "eve\rforged"},
		{Name: "eve\tforged"},
		{Email: "eve\r@example.com"},
		{Email: "eve\x1b[31m@example.com"},
		{Email: "eve\x7f@example.com"},
	} {
		_, err := s.Write(testCtx(t), WriteRequest{
			Changes: []Change{{Path: "p/a.yaml", Content: []byte("a\n")}},
			Author:  bad,
		})
		assert.ErrorIs(t, err, ErrInvalidRequest, "%+v", bad)

		_, err = New(Config{Repository: gitops.Repository{URL: bare}, Committer: bad})
		assert.ErrorIs(t, err, ErrInvalidRequest, "committer %+v", bad)
	}
}

func TestClassifyPushCaseInsensitivePolicyStatuses(t *testing.T) {
	for _, msg := range []string{
		"command error on refs/heads/main: Pre-receive HOOK DECLINED",
		"command error on refs/heads/main: You are NOT ALLOWED to push to main",
		"command error on refs/heads/main: Permission denied for user",
		"command error on refs/heads/main: Refusing to update checked out branch",
		"command error on refs/heads/main: Protected branch hook",
	} {
		assert.Equal(t, pushRefused, classifyPush(errors.New(msg)), msg)
	}
	assert.Equal(t, pushLost, classifyPush(errors.New("command error on refs/heads/main: NON-FAST-FORWARD")))
}

func TestErrorsMatchTheirSentinels(t *testing.T) {
	assert.ErrorIs(t, ErrIdempotencyConflict, ErrConflict)
	err := error(&ConflictError{Path: "p", Expected: "a", Actual: "b", err: ErrConflict})
	assert.True(t, errors.Is(err, ErrConflict))
	assert.Contains(t, err.Error(), "expected revision a, current revision b")
}
