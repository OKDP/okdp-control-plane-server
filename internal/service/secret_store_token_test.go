package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func tokenRequest(server, token string) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: server, Path: "secret", Version: "v2"},
		Auth: &models.SecretStoreAuth{
			Type:   "token",
			Config: models.SecretAuthConfig{Token: token},
		},
	}
}

// Vault's default policy grants "read" on auth/token/lookup-self, which maps to
// GET. A POST needs "update", a capability no least-privilege token carries, so
// checking with POST accepted only root tokens.
func TestValidateVaultTokenUsesGet(t *testing.T) {
	var method string
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	if err := svc.TestConnection(context.Background(), tokenRequest(vault.URL, "app-token")); err != nil {
		t.Fatalf("a least-privilege token was rejected: %v", err)
	}
	if method != http.MethodGet {
		t.Fatalf("lookup-self was called with %s, want GET", method)
	}
}

// A token Vault actually refuses must still be reported, so the change does not
// turn the check into a formality.
func TestValidateVaultTokenStillRejectsForbidden(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer vault.Close()

	svc := &DefaultSecretStoreService{}
	err := svc.TestConnection(context.Background(), tokenRequest(vault.URL, "bad-token"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("a refused token was accepted: %v", err)
	}
}
