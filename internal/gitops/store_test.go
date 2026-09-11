package gitops

import (
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// alice and bob are the logged-in users of the writer tests.
var (
	alice = auth.Actor{Username: "alice", Name: "Alice Martin", Email: "alice@example.com"}
	bob   = auth.Actor{Username: "bob", Name: "Bob", Email: "bob@example.com"}
)

func TestCommitMessageNamesTheUserAsCoAuthor(t *testing.T) {
	got := CommitMessage("deploy", "demo/trino", alice)
	want := "okdp: deploy demo/trino by alice\n\nCo-Authored-By: Alice Martin <alice@example.com>"
	if got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
}

func TestCommitMessageWithoutEmailHasNoTrailerAndWarnsOnce(t *testing.T) {
	hook := test.NewGlobal()
	defer hook.Reset()

	bob := auth.Actor{Username: "bob-no-email", Name: "Bob"}
	for range 3 {
		if got := CommitMessage("delete", "demo/trino", bob); got != "okdp: delete demo/trino by bob-no-email" {
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

func TestCommitMessageForAnonymousHasNoTrailer(t *testing.T) {
	anonymous := auth.Actor{Username: "anonymous", Name: "anonymous"}
	if got := CommitMessage("create connection", "demo/lake", anonymous); got != "okdp: create connection demo/lake by anonymous" {
		t.Errorf("message = %q", got)
	}
	if got := CommitMessage("update", "demo/lake", auth.Actor{}); got != "okdp: update demo/lake by unknown" {
		t.Errorf("message = %q", got)
	}
}

func TestCommitMessageCannotBeInjectedIntoTrailers(t *testing.T) {
	mallory := auth.Actor{
		Username: "mallory\nCo-Authored-By: u <u@v>",
		Name:     "Mallory\nCo-Authored-By: x <y@z>",
		Email:    "mallory@example.com\r\n",
	}
	got := CommitMessage("deploy", "demo/trino", mallory)
	want := "okdp: deploy demo/trino by malloryCo-Authored-By: u u@v\n\nCo-Authored-By: MalloryCo-Authored-By: x y@z <mallory@example.com>"
	if got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
	if n := strings.Count(got, "\n"); n != 2 {
		t.Errorf("%d line breaks, want 2 (subject, blank line, one trailer)", n)
	}
}

func TestCommitMessageSkipsTheTrailerForAnInvalidEmail(t *testing.T) {
	for _, email := range []string{"not-an-address", "a@b@c", "a b@c", "@example.com", "alice@", "<>@x"} {
		actor := auth.Actor{Username: "eve-" + email, Name: "Eve", Email: email}
		if got := CommitMessage("deploy", "demo/trino", actor); strings.Contains(got, "Co-Authored-By") {
			t.Errorf("email %q: message %q has a trailer", email, got)
		}
	}
}

func TestCommitMessageFallsBackToTheUsernameForAnEmptyName(t *testing.T) {
	got := CommitMessage("deploy", "demo/trino", auth.Actor{Username: "alice", Name: "<>", Email: "alice@example.com"})
	if want := "okdp: deploy demo/trino by alice\n\nCo-Authored-By: alice <alice@example.com>"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}
