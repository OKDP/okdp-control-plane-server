package service

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
	"github.com/stretchr/testify/mock"
)

func serviceAccountPtr(name string) *string { return &name }

func kubernetesAuth(serviceAccount *string) *models.SecretStoreAuth {
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
	if got := kubernetesRef(t, kubernetesAuth(serviceAccountPtr("vault-reader"))).Name; got != "vault-reader" {
		t.Fatalf("service account is %q, want vault-reader", got)
	}
}

// Omitting it must keep the previous behaviour, so stores created before the
// field existed keep working untouched.
func TestKubernetesAuthFallsBackToDefault(t *testing.T) {
	if got := kubernetesRef(t, kubernetesAuth(nil)).Name; got != "default" {
		t.Fatalf("service account is %q, want default", got)
	}
	if got := kubernetesRef(t, kubernetesAuth(serviceAccountPtr(""))).Name; got != "default" {
		t.Fatalf("an emptied account gives %q, want default", got)
	}
}

// Reading the account back matters as much as writing it: without it the
// console shows an empty field and the next save silently repoints the store.
func TestResponseReportsTheServiceAccount(t *testing.T) {
	store := buildSecretStoreCRD("demo", models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
		Auth:     kubernetesAuth(serviceAccountPtr("vault-reader")),
	}, "")

	svc := &DefaultSecretStoreService{}
	resp := svc.toResponse(store, "demo")

	if resp.Auth == nil || resp.Auth.Config.ServiceAccount == nil {
		t.Fatal("the response carries no service account")
	}
	if got := *resp.Auth.Config.ServiceAccount; got != "vault-reader" {
		t.Fatalf("response reports %q, want vault-reader", got)
	}
}

// storedStore is a SecretStore as the cluster already holds it, used as the
// starting point of the update tests below.
func storedStore(auth crd.ESOVaultAuth) *crd.ESOSecretStore {
	store := &crd.ESOSecretStore{}
	store.Name = "store"
	store.Namespace = "demo"
	store.ResourceVersion = "42"
	store.Spec.Provider.Vault = &crd.ESOVaultProvider{
		Server:  "https://vault.example.com",
		Path:    "secret",
		Version: "v2",
		Auth:    auth,
	}
	return store
}

func kubernetesStored(serviceAccount string) *crd.ESOSecretStore {
	return storedStore(crd.ESOVaultAuth{Kubernetes: &crd.ESOKubernetesAuth{
		MountPath:         "kubernetes",
		Role:              "demo-role",
		ServiceAccountRef: &crd.ESOServiceAccountRef{Name: serviceAccount},
	}})
}

func tokenStored() *crd.ESOSecretStore {
	return storedStore(crd.ESOVaultAuth{TokenSecretRef: &crd.ESOTokenSecretRef{
		Name: "store-credentials",
		Key:  "token",
	}})
}

func updateRequest(auth *models.SecretStoreAuth) models.SecretStoreRequest {
	return models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
		Auth:     auth,
	}
}

// updated runs an update against a store the cluster already holds and returns
// the CR handed to the repository, which is what actually reaches the cluster.
func updated(t *testing.T, stored *crd.ESOSecretStore, req models.SecretStoreRequest, arrange func(*mocks.SecretStoreRepository)) (*crd.ESOSecretStore, error) {
	t.Helper()
	repo := &mocks.SecretStoreRepository{}
	repo.On("Get", mock.Anything, "demo", "store").Return(stored, nil)

	var sent *crd.ESOSecretStore
	repo.On("Update", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
		sent = args.Get(2).(*crd.ESOSecretStore)
	}).Return(nil).Maybe()
	if arrange != nil {
		arrange(repo)
	}

	svc := &DefaultSecretStoreService{repo: repo}
	_, err := svc.UpdateSecretStore(context.Background(), "demo", "store", req)
	return sent, err
}

// The regression this guards: the CR is rebuilt from the request, so a caller
// that says nothing about the account would repoint the store at default and
// break the Vault role binding without touching the field.
func TestUpdateKeepsTheServiceAccountWhenTheFieldIsAbsent(t *testing.T) {
	sent, err := updated(t, kubernetesStored("vault-reader"), updateRequest(kubernetesAuth(nil)), nil)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if got := sent.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef.Name; got != "vault-reader" {
		t.Fatalf("account became %q, want vault-reader kept", got)
	}
}

// The other half of the same rule: keeping an absent field must not make the
// default account unreachable, or a store given an identity could never give
// it back.
func TestUpdateReturnsToDefaultWhenTheFieldIsEmptied(t *testing.T) {
	sent, err := updated(t, kubernetesStored("vault-reader"), updateRequest(kubernetesAuth(serviceAccountPtr(""))), nil)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if got := sent.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef.Name; got != "default" {
		t.Fatalf("account stayed %q, want default", got)
	}
}

func TestUpdateSetsANewServiceAccount(t *testing.T) {
	sent, err := updated(t, kubernetesStored("vault-reader"), updateRequest(kubernetesAuth(serviceAccountPtr("autre"))), nil)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if got := sent.Spec.Provider.Vault.Auth.Kubernetes.ServiceAccountRef.Name; got != "autre" {
		t.Fatalf("account is %q, want autre", got)
	}
}

// Switching to token auth without a token would write a CR referencing a Secret
// that was never created: the store can never sync, and the API used to answer
// success all the same.
func TestUpdateRefusesTokenAuthWithoutAToken(t *testing.T) {
	_, err := updated(t, kubernetesStored("vault-reader"), updateRequest(&models.SecretStoreAuth{
		Type:   "token",
		Config: models.SecretAuthConfig{},
	}), nil)
	if err == nil {
		t.Fatal("switching to token auth with no token was accepted")
	}
}

// A store that already authenticates by token keeps it, so an edit that only
// renames the store does not force the operator to paste the token again.
func TestUpdateKeepsTheStoredTokenWhenTheFieldIsAbsent(t *testing.T) {
	repoCalls := &mocks.SecretStoreRepository{}
	_, err := updated(t, tokenStored(), updateRequest(&models.SecretStoreAuth{
		Type:   "token",
		Config: models.SecretAuthConfig{},
	}), func(r *mocks.SecretStoreRepository) { repoCalls = r })
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	repoCalls.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Leaving token auth strands the credentials Secret: the CR no longer points at
// it, nothing in the console shows it, and the token stays valid in Vault.
func TestUpdateDropsTheTokenSecretWhenLeavingTokenAuth(t *testing.T) {
	var deleted string
	sent, err := updated(t, tokenStored(), updateRequest(kubernetesAuth(serviceAccountPtr("vault-reader"))),
		func(r *mocks.SecretStoreRepository) {
			r.On("DeleteSecret", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
				deleted = args.String(2)
			}).Return(nil)
		})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if sent.Spec.Provider.Vault.Auth.Kubernetes == nil {
		t.Fatal("the store did not switch to kubernetes auth")
	}
	if deleted != "store-credentials" {
		t.Fatalf("deleted secret is %q, want store-credentials", deleted)
	}
}

// A store that stays on token auth must keep its Secret, or every edit would
// break the store it just saved.
func TestUpdateKeepsTheTokenSecretWhenStayingOnTokenAuth(t *testing.T) {
	repoCalls := &mocks.SecretStoreRepository{}
	_, err := updated(t, tokenStored(), updateRequest(&models.SecretStoreAuth{
		Type:   "token",
		Config: models.SecretAuthConfig{Token: "s.nouveau"},
	}), func(r *mocks.SecretStoreRepository) {
		repoCalls = r
		r.On("CreateOrUpdateSecret", mock.Anything, "demo", "store-credentials", mock.Anything).Return(nil)
	})
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	repoCalls.AssertNotCalled(t, "DeleteSecret", mock.Anything, mock.Anything, mock.Anything)
}

// A refusal caused by the request must reach the handler as a ValidationError,
// which is what turns it into a 400. Reported as a plain error it became a 500,
// so a console could only tell the user the server had broken when the fix was
// to fill in a field.
func TestUpdateRefusalsAreReportedAsBadRequests(t *testing.T) {
	cases := map[string]models.SecretStoreRequest{
		"no token on the switch to token auth": updateRequest(&models.SecretStoreAuth{
			Type:   "token",
			Config: models.SecretAuthConfig{},
		}),
		"no role on kubernetes auth": updateRequest(&models.SecretStoreAuth{
			Type:   "kubernetes",
			Config: models.SecretAuthConfig{MountPath: "kubernetes"},
		}),
		"unsupported auth type": updateRequest(&models.SecretStoreAuth{
			Type:   "appRole",
			Config: models.SecretAuthConfig{},
		}),
	}

	for name, req := range cases {
		_, err := updated(t, kubernetesStored("vault-reader"), req, nil)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !IsValidationError(err) {
			t.Fatalf("%s: refused as a server fault, not a bad request: %v", name, err)
		}
	}
}

// validateRequest lets the token be empty because an update reads that as
// "keep the stored one". A create has nothing to keep: the store was written
// with an empty Secret, could never authenticate, and the API answered 201.
func TestCreateRefusesTokenAuthWithoutAToken(t *testing.T) {
	repo := &mocks.SecretStoreRepository{}
	repo.On("CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	repo.On("Create", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	repo.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(tokenStored(), nil).Maybe()
	svc := &DefaultSecretStoreService{repo: repo}

	_, err := svc.CreateSecretStore(context.Background(), "demo", models.SecretStoreRequest{
		Name:     "store",
		Provider: "vault",
		Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
		Auth:     &models.SecretStoreAuth{Type: "token", Config: models.SecretAuthConfig{}},
	})
	if err == nil {
		t.Fatal("a token store was created with no token")
	}
	if !IsValidationError(err) {
		t.Fatalf("refused as a platform failure rather than a rejected input: %v", err)
	}
	repo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
}

// A mistyped ServiceAccount reached the API server, came back as a schema
// rejection the handler could not tell from a platform failure, and answered
// 500 where the fix was to correct a name.
func TestKubernetesAuthRefusesAnInvalidServiceAccount(t *testing.T) {
	for _, sa := range []string{"Mon_Compte", "compte invalide", "-debut", "MAJUSCULE"} {
		err := validateRequest(models.SecretStoreRequest{
			Name:     "store",
			Provider: "vault",
			Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
			Auth:     kubernetesAuth(serviceAccountPtr(sa)),
		})
		if !IsValidationError(err) {
			t.Fatalf("service account %q was accepted or misreported: %v", sa, err)
		}
	}
	// A valid one, and an emptied one asking for the default, both pass.
	for _, sa := range []*string{serviceAccountPtr("vault-reader"), serviceAccountPtr(""), nil} {
		if err := validateRequest(models.SecretStoreRequest{
			Name:     "store",
			Provider: "vault",
			Vault:    &models.VaultConfig{Server: "https://vault.example.com", Path: "secret", Version: "v2"},
			Auth:     kubernetesAuth(sa),
		}); err != nil {
			t.Fatalf("a legitimate service account was rejected: %v", err)
		}
	}
}
