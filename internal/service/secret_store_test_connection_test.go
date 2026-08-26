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
// server that does not exist still reported "Connection successful" while the
// store failed on reconcile.
func TestTestConnectionKubernetesRejectsUnreachableServer(t *testing.T) {
	svc := &DefaultSecretStoreService{}
	req := kubernetesRequest("https://this-host-does-not-exist.invalid:8200", "demo-role", "kubernetes")

	err := svc.TestConnection(context.Background(), req)
	if err == nil {
		t.Fatal("an unreachable vault server reported success")
	}
	if !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("error does not name the connection failure: %v", err)
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
