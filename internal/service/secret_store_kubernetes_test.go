package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func kubernetesRequest(server string) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: server, Path: "secret", Version: "v2"},
		Auth:     &models.SecretStoreAuth{Type: "kubernetes"},
	}
}

// With Kubernetes auth there is no token to look up, so the test used to
// answer "successful" without contacting Vault at all.
func TestKubernetesAuthAsksVaultHealth(t *testing.T) {
	type call struct{ path, token string }
	calls := make(chan call, 1)
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case calls <- call{r.URL.Path, r.Header.Get("X-Vault-Token")}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	if err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL)); err != nil {
		t.Fatalf("a healthy Vault was rejected: %v", err)
	}

	select {
	case got := <-calls:
		if got.path != "/v1/sys/health" {
			t.Fatalf("called %s, want /v1/sys/health", got.path)
		}
		if got.token != "" {
			t.Fatalf("sys/health was called with a token %q, it needs none", got.token)
		}
	default:
		t.Fatal("Vault was never called")
	}
}

func TestKubernetesAuthReportsASealedVault(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer vault.Close()

	err := (&DefaultSecretStoreService{}).TestConnection(context.Background(), kubernetesRequest(vault.URL))
	if err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("got %v, want an error naming the sealed Vault", err)
	}
}

func TestKubernetesAuthFailsWhenVaultDoesNotAnswer(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := vault.URL
	vault.Close()

	err := (&DefaultSecretStoreService{}).TestConnection(context.Background(), kubernetesRequest(url))
	if err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("got %v, want a connection failure", err)
	}
}

// A server written with a trailing slash must not turn the path into
// "//v1/sys/health", which Vault answers with a 404.
func TestKubernetesAuthAcceptsATrailingSlash(t *testing.T) {
	paths := make(chan string, 1)
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer vault.Close()

	if err := (&DefaultSecretStoreService{}).TestConnection(context.Background(), kubernetesRequest(vault.URL+"/")); err != nil {
		t.Fatalf("a trailing slash broke the check: %v", err)
	}
	if got := <-paths; got != "/v1/sys/health" {
		t.Fatalf("called %s, want /v1/sys/health", got)
	}
}

// sys/health encodes the node state in the status code. Every answering node
// passes, the two states that cannot serve are named.
func TestKubernetesAuthReadsTheHealthStatusCode(t *testing.T) {
	cases := []struct {
		code int
		want string // empty when the check must pass
	}{
		{http.StatusOK, ""},
		{http.StatusTooManyRequests, ""},
		{472, ""},
		{473, ""},
		{http.StatusNotImplemented, "not initialized"},
		{http.StatusServiceUnavailable, "sealed"},
		{http.StatusInternalServerError, "status 500"},
	}
	for _, tc := range cases {
		code := tc.code
		vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		err := (&DefaultSecretStoreService{}).TestConnection(context.Background(), kubernetesRequest(vault.URL))
		vault.Close()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("status %d: got %v, want the check to pass", code, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("status %d: got %v, want an error naming %q", code, err, tc.want)
		}
	}
}
