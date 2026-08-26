package service

import (
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

func kubernetesAuth(serviceAccount string) *models.SecretStoreAuth {
	return &models.SecretStoreAuth{
		Type: "kubernetes",
		Config: models.SecretAuthConfig{
			Role:           "demo-role",
			MountPath:      "kubernetes",
			ServiceAccount: serviceAccount,
		},
	}
}

func kubernetesRef(t *testing.T, auth *models.SecretStoreAuth) *crd.ESOServiceAccountRef {
	t.Helper()
	store := buildSecretStoreCRD("demo", models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
		Auth:     auth,
	}, "")
	k8s := store.Spec.Provider.Vault.Auth.Kubernetes
	if k8s == nil {
		t.Fatal("kubernetes auth was not built")
	}
	if k8s.ServiceAccountRef == nil {
		t.Fatal("no service account reference was set")
	}
	return k8s.ServiceAccountRef
}

// Vault matches the role's bound_service_account_names against this account, so
// a project must be able to give its store an identity of its own rather than
// the default account every workload in the namespace already shares.
func TestKubernetesAuthUsesTheChosenServiceAccount(t *testing.T) {
	if got := kubernetesRef(t, kubernetesAuth("vault-reader")).Name; got != "vault-reader" {
		t.Fatalf("service account is %q, want vault-reader", got)
	}
}

// Leaving it empty must keep the previous behaviour, so stores created before
// the field existed keep working untouched.
func TestKubernetesAuthFallsBackToDefault(t *testing.T) {
	if got := kubernetesRef(t, kubernetesAuth("")).Name; got != "default" {
		t.Fatalf("service account is %q, want default", got)
	}
}
