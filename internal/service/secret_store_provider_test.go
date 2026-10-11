package service

import (
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

func TestResponseReportsTheStoreProvider(t *testing.T) {
	store := &crd.ESOSecretStore{}
	store.Name = "store"
	store.Spec.Provider.Name = "kubernetes"

	resp := (&DefaultSecretStoreService{}).toResponse(store, "demo")

	if resp.Provider != "kubernetes" {
		t.Fatalf("provider is %q, want kubernetes", resp.Provider)
	}
	if resp.Vault != nil || resp.Auth != nil {
		t.Fatalf("a non-vault store must not carry vault details, got vault=%+v auth=%+v", resp.Vault, resp.Auth)
	}
}

func TestResponseReportsVaultStores(t *testing.T) {
	store := tokenStored()
	store.Spec.Provider.Name = "vault"

	resp := (&DefaultSecretStoreService{}).toResponse(store, "demo")

	if resp.Provider != "vault" {
		t.Fatalf("provider is %q, want vault", resp.Provider)
	}
	if resp.Vault == nil || resp.Vault.Server != "https://vault.example.com" {
		t.Fatalf("vault block is %+v, want the stored server", resp.Vault)
	}
}
