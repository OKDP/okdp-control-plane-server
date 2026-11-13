package repository

import (
	"errors"
	"net/http"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/transport"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// CallerPrefix is put before the user and group names taken from a token, as
// the API server's --oidc-username-prefix does. A caller can then never be
// sent as a system: user or group, whatever the identity provider returns.
const CallerPrefix = "oidc:"

// UsersGroup is added to every caller. It carries the reads any console user
// needs outside a project, and the identity provider cannot forge it: its
// groups all arrive under CallerPrefix.
const UsersGroup = "okdp:users"

// ErrNoCaller is what a caller client answers when the request context
// carries no identity. Falling back on the server's own ServiceAccount
// would hand its rights to whoever reached that code path.
var ErrNoCaller = errors.New("no authenticated caller on the request context")

// impersonatingTransport sends every request as the caller found in its
// context, so RBAC, admission policies and the audit log see that caller.
type impersonatingTransport struct {
	next http.RoundTripper
}

func (t *impersonatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	identity := auth.IdentityFrom(req.Context())
	if identity == nil {
		return nil, ErrNoCaller
	}
	username := identity.Username
	if username == "" {
		username = identity.Subject
	}
	if username == "" {
		return nil, ErrNoCaller
	}

	// A RoundTripper must not modify the request it is given.
	req = utilnet.CloneRequest(req)
	req.Header.Set(transport.ImpersonateUserHeader, CallerPrefix+username)
	req.Header.Del(transport.ImpersonateGroupHeader)
	req.Header.Add(transport.ImpersonateGroupHeader, UsersGroup)
	for _, group := range identity.Groups {
		req.Header.Add(transport.ImpersonateGroupHeader, CallerPrefix+group)
	}
	return t.next.RoundTrip(req)
}

func (t *impersonatingTransport) WrappedRoundTripper() http.RoundTripper {
	return t.next
}
