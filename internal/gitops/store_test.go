package gitops

import (
	"os"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/buildinfo"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// TestMain fixes the version the commits name, as the linker would.
func TestMain(m *testing.M) {
	buildinfo.Version = "0.9.0"
	os.Exit(m.Run())
}

// alice and bob are the logged-in users of the writer tests.
var (
	alice = auth.Actor{Username: "alice", Name: "Alice Martin", Email: "alice@example.com"}
	bob   = auth.Actor{Username: "bob", Name: "Bob", Email: "bob@example.com"}
)

// service is the committer of the message tests.
var service = Signature{Name: "OKDP control plane", Email: "okdp-control-plane@okdp.io"}

const trailer = "\n\nCo-Authored-By: okdp-control-plane-server v0.9.0 <okdp-control-plane@okdp.io>"

func TestCommitIsAuthoredByTheUserAndCoAuthoredByTheControlPlane(t *testing.T) {
	commit := NewCommit("deploy", "demo/trino", alice)
	if commit.Author == nil || *commit.Author != (Signature{Name: "Alice Martin", Email: "alice@example.com"}) {
		t.Errorf("author = %+v", commit.Author)
	}
	want := "okdp: deploy demo/trino by alice" + trailer
	if got := commit.Message(service); got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
}

func TestCommitTrailerNamesTheConfiguredCommitterEmail(t *testing.T) {
	got := NewCommit("deploy", "demo/trino", alice).Message(Signature{Name: "Bot", Email: "bot@corp.example"})
	if want := "okdp: deploy demo/trino by alice\n\nCo-Authored-By: okdp-control-plane-server v0.9.0 <bot@corp.example>"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestCommitWithoutEmailIsAuthoredByTheServiceAndWarnsOnce(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()

	bob := auth.Actor{Username: "bob-no-email", Name: "Bob"}
	for range 3 {
		commit := NewCommit("delete", "demo/trino", bob)
		if commit.Author != nil {
			t.Errorf("author = %+v, want none", commit.Author)
		}
		if got := commit.Message(service); got != "okdp: delete demo/trino by bob-no-email" {
			t.Errorf("message = %q", got)
		}
	}
	warnings := 0
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.WarnLevel && strings.Contains(e.Message, "bob-no-email") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("%d warnings for the same user, want 1", warnings)
	}
}

func TestCommitForAnonymousHasNoAuthorNorTrailer(t *testing.T) {
	anonymous := auth.Actor{Username: "anonymous", Name: "anonymous"}
	commit := NewCommit("create connection", "demo/lake", anonymous)
	if commit.Author != nil || commit.Message(service) != "okdp: create connection demo/lake by anonymous" {
		t.Errorf("commit = %+v, message %q", commit, commit.Message(service))
	}
	commit = NewCommit("update", "demo/lake", auth.Actor{})
	if commit.Author != nil || commit.Message(service) != "okdp: update demo/lake by unknown" {
		t.Errorf("commit = %+v, message %q", commit, commit.Message(service))
	}
}

func TestCommitCannotBeInjectedIntoTheMessageOrTheAuthor(t *testing.T) {
	mallory := auth.Actor{
		Username: "mallory\nCo-Authored-By: u <u@v>",
		Name:     "Mallory\nCo-Authored-By: x <y@z>",
		Email:    "mallory@example.com\r\n",
	}
	commit := NewCommit("deploy", "demo/trino", mallory)
	if commit.Author == nil || *commit.Author != (Signature{Name: "MalloryCo-Authored-By: x y@z", Email: "mallory@example.com"}) {
		t.Errorf("author = %+v", commit.Author)
	}
	got := commit.Message(service)
	if want := "okdp: deploy demo/trino by malloryCo-Authored-By: u u@v" + trailer; got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
	if n := strings.Count(got, "\n"); n != 2 {
		t.Errorf("%d line breaks, want 2 (subject, blank line, one trailer)", n)
	}
}

func TestCommitHasNoUserAuthorForAnInvalidEmail(t *testing.T) {
	for _, email := range []string{"not-an-address", "a@b@c", "a b@c", "@example.com", "alice@", "<>@x"} {
		actor := auth.Actor{Username: "eve-" + email, Name: "Eve", Email: email}
		commit := NewCommit("deploy", "demo/trino", actor)
		if commit.Author != nil || strings.Contains(commit.Message(service), "Co-Authored-By") {
			t.Errorf("email %q: author %+v, message %q", email, commit.Author, commit.Message(service))
		}
	}
}

func TestCommitAuthorFallsBackToTheUsernameForAnEmptyName(t *testing.T) {
	commit := NewCommit("deploy", "demo/trino", auth.Actor{Username: "alice", Name: "<>", Email: "alice@example.com"})
	if commit.Author == nil || *commit.Author != (Signature{Name: "alice", Email: "alice@example.com"}) {
		t.Errorf("author = %+v", commit.Author)
	}
}
