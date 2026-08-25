package gitops

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
)

func init() {
	// Serve file:// in process: the tests need no git binary, and go-git's own
	// server is stricter than git's, which is what a replay must survive.
	client.InstallProtocol("file", server.DefaultServer)
}

// newRemote creates an empty bare repository and returns its URL.
func newRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatalf("init remote: %v", err)
	}
	return "file://" + dir
}

func newStore(t *testing.T, url string, onDisk bool) *GitStore {
	t.Helper()
	opts := GitOptions{URL: url, Branch: "main", PathPrefix: "gitops", AuthorName: "tester", AuthorEmail: "t@example.com"}
	if onDisk {
		opts.CloneDir = filepath.Join(t.TempDir(), "clone")
	}
	s, err := NewGitStore(opts)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func readFile(t *testing.T, s Store, p string) string {
	t.Helper()
	var out string
	err := s.View(context.Background(), func(r Reader) error {
		data, err := r.ReadFile(p)
		out = string(data)
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return out
}

func TestGitStoreWritesToAnEmptyRemoteAndReadsBack(t *testing.T) {
	for _, onDisk := range []bool{false, true} {
		url := newRemote(t)
		writer := newStore(t, url, onDisk)
		ctx := context.Background()

		rev, err := writer.Update(ctx, "okdp: deploy demo/hive by alice", func(tx Tx) error {
			return tx.WriteFile("projects/demo/services/hive/instance.yaml", []byte("name: hive\n"))
		})
		if err != nil || rev == "" {
			t.Fatalf("first write: rev=%q err=%v", rev, err)
		}

		reader := newStore(t, url, !onDisk)
		if got := readFile(t, reader, "projects/demo/services/hive/instance.yaml"); got != "name: hive\n" {
			t.Fatalf("read back %q", got)
		}
		names := []string{}
		_ = reader.View(ctx, func(r Reader) error {
			names, _ = r.ReadDir("projects/demo/services")
			return nil
		})
		if len(names) != 1 || names[0] != "hive" {
			t.Fatalf("ReadDir = %v", names)
		}

		// The prefix is honoured: the file sits under gitops/ in the repository.
		repo, _ := git.PlainOpen(strings.TrimPrefix(url, "file://"))
		head, err := repo.Reference(plumbing.NewBranchReferenceName("main"), true)
		if err != nil {
			t.Fatalf("main branch not pushed: %v", err)
		}
		commit, _ := repo.CommitObject(head.Hash())
		if commit.Message != "okdp: deploy demo/hive by alice" || commit.Author.Name != "tester" {
			t.Fatalf("commit = %q by %q", commit.Message, commit.Author.Name)
		}
		if _, err := commit.File("gitops/projects/demo/services/hive/instance.yaml"); err != nil {
			t.Fatalf("file not under the prefix: %v", err)
		}
	}
}

func TestGitStoreReplaysAWriteThatLostTheRace(t *testing.T) {
	url := newRemote(t)
	ctx := context.Background()
	a := newStore(t, url, false)
	b := newStore(t, url, true)

	if _, err := a.Update(ctx, "seed", func(tx Tx) error {
		return tx.WriteFile("platform/catalog.yaml", []byte("categories: []\n"))
	}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	_, err := b.Update(ctx, "b writes", func(tx Tx) error {
		calls++
		if calls == 1 {
			// Another writer pushes between our fetch and our push.
			if _, err := a.Update(ctx, "a writes", func(tx Tx) error {
				return tx.WriteFile("projects/demo/project.yaml", []byte("name: demo\n"))
			}); err != nil {
				t.Fatalf("concurrent write: %v", err)
			}
		}
		return tx.WriteFile("projects/other/project.yaml", []byte("name: other\n"))
	})
	if err != nil {
		t.Fatalf("replayed write: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the change ran %d times, want 2 (once lost, once replayed)", calls)
	}

	fresh := newStore(t, url, false)
	if got := readFile(t, fresh, "projects/demo/project.yaml"); got != "name: demo\n" {
		t.Fatalf("the concurrent write was lost: %q", got)
	}
	if got := readFile(t, fresh, "projects/other/project.yaml"); got != "name: other\n" {
		t.Fatalf("the replayed write is missing: %q", got)
	}
}

func TestGitStoreFailedChangeLeavesNothingBehind(t *testing.T) {
	url := newRemote(t)
	ctx := context.Background()
	s := newStore(t, url, true)
	if _, err := s.Update(ctx, "seed", func(tx Tx) error {
		return tx.WriteFile("a.yaml", []byte("a\n"))
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	_, err := s.Update(ctx, "fails", func(tx Tx) error {
		_ = tx.WriteFile("b.yaml", []byte("b\n"))
		_ = tx.Remove("a.yaml")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	_ = s.View(ctx, func(r Reader) error {
		if r.Exists("b.yaml") || !r.Exists("a.yaml") {
			t.Fatalf("the failed change leaked into reads")
		}
		return nil
	})
}

func TestGitStoreNoChangeCommitsNothing(t *testing.T) {
	url := newRemote(t)
	ctx := context.Background()
	s := newStore(t, url, false)
	rev1, err := s.Update(ctx, "seed", func(tx Tx) error { return tx.WriteFile("a.yaml", []byte("a\n")) })
	if err != nil {
		t.Fatal(err)
	}
	rev2, err := s.Update(ctx, "same", func(tx Tx) error { return tx.WriteFile("a.yaml", []byte("a\n")) })
	if err != nil {
		t.Fatal(err)
	}
	if rev1 != rev2 {
		t.Fatalf("an empty change produced a commit: %s then %s", rev1, rev2)
	}
}

func TestCleanPathRefusesEscapes(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "../x", "a/../../x"} {
		if _, err := cleanPath(p); err == nil {
			t.Errorf("cleanPath(%q) accepted", p)
		}
	}
}
