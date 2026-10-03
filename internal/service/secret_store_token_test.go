package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
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
	// The handler runs on the server's goroutine, so the method travels back
	// through a channel: reading a shared variable across the two would be a
	// data race the memory model does not forbid from surfacing.
	type call struct{ method, token, path string }
	calls := make(chan call, 1)
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case calls <- call{r.Method, r.Header.Get("X-Vault-Token"), r.URL.Path}:
		default:
		}
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

	select {
	case got := <-calls:
		if got.method != http.MethodGet {
			t.Fatalf("lookup-self was called with %s, want GET", got.method)
		}
		// The whole point of the check is the token: without this header it
		// degrades to a ping that answers "successful" for any token at all.
		if got.token != "app-token" {
			t.Fatalf("X-Vault-Token carried %q, want the caller's token", got.token)
		}
		if got.path != "/v1/auth/token/lookup-self" {
			t.Fatalf("called %s, want /v1/auth/token/lookup-self", got.path)
		}
	default:
		t.Fatal("lookup-self was never called")
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

// A store whose <name>-credentials is someone else's Secret is a bad request,
// and nothing is created.
func TestCreateSecretStoreRefusesAForeignCredentialsSecret(t *testing.T) {
	repo := &mocks.SecretStoreRepository{}
	repo.On("CreateOrUpdateSecret", mock.Anything, "demo", "p-hive-credentials", mock.Anything).
		Return(fmt.Errorf("%w: secret %q", repository.ErrForeignSecret, "p-hive-credentials"))
	req := tokenRequest("https://vault.example", "s.token")
	req.Name = "p-hive"

	_, err := NewDefaultSecretStoreService(repo).CreateSecretStore(context.Background(), "demo", req)

	if !IsValidationError(err) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	repo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
}
