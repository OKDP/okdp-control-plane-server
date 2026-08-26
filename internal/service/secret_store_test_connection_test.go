package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func kubernetesRequest(server, role, mountPath string) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: server, Path: "secret", Version: "v2"},
		Auth: &models.SecretStoreAuth{
			Type:   "kubernetes",
			Config: models.SecretAuthConfig{Role: role, MountPath: mountPath},
		},
	}
}

// The check used to return nil for kubernetes auth before any request, so a
// server that does not answer still reported "Connection successful" while the
// store failed on reconcile. The listener is closed up front so the refusal is
// immediate and owes nothing to DNS.
func TestTestConnectionKubernetesRejectsUnreachableServer(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := closed.URL
	closed.Close()

	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), kubernetesRequest(address, "demo-role", "kubernetes"))
	if err == nil {
		t.Fatal("an unreachable vault server reported success")
	}
	if !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("error does not name the connection failure: %v", err)
	}
}

// A reachable but unwell Vault is actionable, so a 5xx must not pass for a
// working mount.
func TestTestConnectionKubernetesRejectsServerError(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL, "demo-role", "kubernetes"))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("a failing vault was accepted: %v", err)
	}
}

// An auth type the server cannot honour must say so, instead of falling through
// to the token path and asking for a token the caller never meant to supply.
func TestTestConnectionRejectsUnsupportedAuthType(t *testing.T) {
	svc := &DefaultSecretStoreService{}
	req := kubernetesRequest("https://vault.example.com", "demo-role", "kubernetes")
	req.Auth.Type = "appRole"

	err := svc.TestConnection(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "unsupported auth type") {
		t.Fatalf("an unsupported auth type was not named: %v", err)
	}
}

// A mount that was never enabled has no handler, so Vault answers 404. Reporting
// success there sends the user to hunt through the external-secrets logs for
// the real cause.
func TestTestConnectionKubernetesRejectsMissingMount(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/kubernetes/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL, "demo-role", "absent"))
	if err == nil {
		t.Fatal("a mount that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Fatalf("error does not name the mount: %v", err)
	}
}

// An enabled mount rejects the empty body for the missing role and jwt, which is
// as far as the check can go without a ServiceAccount token. That is a success.
func TestTestConnectionKubernetesAcceptsExistingMount(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	if err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL, "demo-role", "kubernetes")); err != nil {
		t.Fatalf("an enabled mount was rejected: %v", err)
	}
}

// The role is what Vault matches the ServiceAccount against; without it the
// store cannot work, so the check must not wait for the reconcile to say so.
func TestTestConnectionKubernetesRequiresRole(t *testing.T) {
	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), kubernetesRequest("https://vault.example.com", "", "kubernetes"))
	if err == nil || !strings.Contains(err.Error(), "role is required") {
		t.Fatalf("a missing role was accepted: %v", err)
	}
}

// A server that redirects is not Vault, and the caller's token must not travel
// to whatever host the Location names.
func TestTestConnectionKubernetesRejectsRedirect(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://elsewhere.example.com/")
		w.WriteHeader(http.StatusFound)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL, "demo-role", "kubernetes"))
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a redirect was accepted: %v", err)
	}
}

// The login endpoint rejects an empty body, so a 200 means an ingress default
// backend or a login page is answering instead of Vault.
func TestTestConnectionKubernetesRejectsPlainOK(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL, "demo-role", "kubernetes"))
	if err == nil || !strings.Contains(err.Error(), "does not look like vault") {
		t.Fatalf("a non-vault server was accepted: %v", err)
	}
}

// The mount path must reach the URL: without the fallback the request would go
// to /v1/auth//login, which no Vault serves.
func TestTestConnectionKubernetesDefaultsTheMountPath(t *testing.T) {
	paths := make(chan string, 1)
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	if err := svc.TestConnection(context.Background(), kubernetesRequest(vault.URL, "demo-role", "")); err != nil {
		t.Fatalf("an empty mount path was rejected: %v", err)
	}
	select {
	case path := <-paths:
		if path != "/v1/auth/kubernetes/login" {
			t.Fatalf("called %s, want the kubernetes fallback", path)
		}
	default:
		t.Fatal("the vault server was never called")
	}
}
