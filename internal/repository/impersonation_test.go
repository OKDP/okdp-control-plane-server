package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/transport"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
)

// recordingTransport keeps the last request it was given.
type recordingTransport struct {
	last *http.Request
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.last = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func newRequest(t *testing.T, identity *auth.Identity) *http.Request {
	t.Helper()
	ctx := context.Background()
	if identity != nil {
		ctx = auth.WithIdentity(ctx, identity)
	}
	return httptest.NewRequest(http.MethodGet, "https://kubernetes/api/v1/namespaces", nil).WithContext(ctx)
}

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: ` + server + `
users:
- name: test
  user:
    token: server-token
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestTheCallerIsImpersonatedWithItsGroups(t *testing.T) {
	next := &recordingTransport{}
	rt := &impersonatingTransport{next: next}

	_, err := rt.RoundTrip(newRequest(t, &auth.Identity{Subject: "e5a0-1234", Username: "alice", Groups: []string{"data-scientist", "okdp-platform-admin"}}))

	require.NoError(t, err)
	assert.Equal(t, "oidc:alice", next.last.Header.Get(transport.ImpersonateUserHeader))
	assert.Equal(t, []string{"okdp:users", "oidc:data-scientist", "oidc:okdp-platform-admin"}, next.last.Header.Values(transport.ImpersonateGroupHeader))
}

// Whatever the identity provider returns, the API server never receives a
// system: name from the console: system:masters would be cluster-admin.
func TestNamesFromTheTokenCannotReachSystemNames(t *testing.T) {
	next := &recordingTransport{}
	rt := &impersonatingTransport{next: next}

	_, err := rt.RoundTrip(newRequest(t, &auth.Identity{Username: "system:admin", Groups: []string{"system:masters"}}))

	require.NoError(t, err)
	assert.Equal(t, "oidc:system:admin", next.last.Header.Get(transport.ImpersonateUserHeader))
	assert.Equal(t, []string{"okdp:users", "oidc:system:masters"}, next.last.Header.Values(transport.ImpersonateGroupHeader))
}

// Some providers send no preferred_username nor email: the subject still
// names the caller, and RBAC can bind it.
func TestTheSubjectNamesACallerWithoutUsername(t *testing.T) {
	next := &recordingTransport{}
	rt := &impersonatingTransport{next: next}

	_, err := rt.RoundTrip(newRequest(t, &auth.Identity{Subject: "e5a0-1234"}))

	require.NoError(t, err)
	assert.Equal(t, "oidc:e5a0-1234", next.last.Header.Get(transport.ImpersonateUserHeader))
}

// Fail closed: a code path that lost the request context must not run with
// the server's own rights.
func TestNoCallerMeansNoRequest(t *testing.T) {
	next := &recordingTransport{}
	rt := &impersonatingTransport{next: next}

	_, err := rt.RoundTrip(newRequest(t, nil))

	assert.ErrorIs(t, err, ErrNoCaller)
	assert.Nil(t, next.last)
}

func TestTheGivenRequestIsLeftUntouched(t *testing.T) {
	next := &recordingTransport{}
	rt := &impersonatingTransport{next: next}
	req := newRequest(t, &auth.Identity{Username: "alice", Groups: []string{"data-scientist"}})

	_, err := rt.RoundTrip(req)

	require.NoError(t, err)
	assert.Empty(t, req.Header.Get(transport.ImpersonateUserHeader))
}

// The caller config goes through client-go's own wrapping: this checks the
// header really leaves the process.
func TestTheCallerConfigImpersonatesOnTheWire(t *testing.T) {
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NamespaceList","apiVersion":"v1","items":[]}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("KUBECONFIG", writeKubeconfig(t, server.URL))

	client, err := InitCallerK8sTypedClient()
	require.NoError(t, err)

	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Username: "alice", Groups: []string{"data-scientist"}})
	_, err = client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})

	require.NoError(t, err)
	assert.Equal(t, "oidc:alice", seen.Get(transport.ImpersonateUserHeader))
	assert.Equal(t, []string{"okdp:users", "oidc:data-scientist"}, seen.Values(transport.ImpersonateGroupHeader))
}
