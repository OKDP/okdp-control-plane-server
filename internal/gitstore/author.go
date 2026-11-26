package gitstore

import (
	"fmt"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// Author is a git identity.
type Author struct {
	Name  string
	Email string
}

func (a Author) isZero() bool { return a.Name == "" && a.Email == "" }

func validateAuthor(a Author, label string) error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("%w: %s name must not be empty", ErrInvalidRequest, label)
	}
	for _, field := range []string{a.Name, a.Email} {
		if strings.ContainsFunc(field, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '<' || r == '>' }) {
			return fmt.Errorf("%w: %s %q", ErrInvalidRequest, label, field)
		}
	}
	return nil
}

func cleanAuthorField(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '<' || r == '>' {
			return -1
		}
		return r
	}, s))
}

// AuthorFromIdentity is the git author of a write made by the caller id. The
// name is the username, else the subject. The email comes from the token email
// claim when present, else from the username when it is an email address.
// Without an identity, or with an empty one, the author is fallback, the
// control plane's own identity.
func AuthorFromIdentity(id *auth.Identity, fallback Author) Author {
	if id == nil {
		return fallback
	}
	name := cleanAuthorField(id.Username)
	if name == "" {
		name = cleanAuthorField(id.Subject)
	}
	if name == "" {
		return fallback
	}
	email := cleanAuthorField(id.Email)
	if email == "" && strings.Contains(name, "@") && !strings.ContainsAny(name, " \t") {
		email = name
	}
	return Author{Name: name, Email: email}
}
