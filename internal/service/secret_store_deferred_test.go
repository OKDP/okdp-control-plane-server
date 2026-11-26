package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

// deferredStores is a SecretStoreRepository whose writes reach the cluster
// later, recording the Secrets it is handed over.
type deferredStores struct {
	*mocks.SecretStoreRepository
	released    []string
	checkUpdate error
}

func (d *deferredStores) ReleaseCredentials(_ context.Context, _, name string) error {
	d.released = append(d.released, name)
	return nil
}

func (d *deferredStores) CheckUpdate(context.Context, string, string) error { return d.checkUpdate }

func newDeferredStores() *deferredStores {
	return &deferredStores{SecretStoreRepository: &mocks.SecretStoreRepository{}}
}

func TestADeferredDeletionReleasesTheTokenOnlyOnceCommitted(t *testing.T) {
	unknown := &engine.UnknownError{Operation: engine.Operation{Outcome: engine.OutcomeUnknown}}
	for name, deleteErr := range map[string]error{
		"committed": nil,
		"uncertain": unknown,
		"failed":    errors.New("push refused"),
	} {
		t.Run(name, func(t *testing.T) {
			repo := newDeferredStores()
			repo.On("Get", mock.Anything, "demo", "store").Return(tokenStored(), nil)
			repo.On("Delete", mock.Anything, "demo", "store").Return(deleteErr)
			svc := NewDefaultSecretStoreService(repo)

			err := svc.DeleteSecretStore(context.Background(), "demo", "store")

			repo.AssertNotCalled(t, "DeleteSecret", mock.Anything, mock.Anything, mock.Anything)
			if deleteErr == nil {
				require.NoError(t, err)
				assert.Equal(t, []string{"store-credentials"}, repo.released)
				return
			}
			assert.ErrorIs(t, err, deleteErr)
			assert.Empty(t, repo.released, "nothing is marked when the deletion may not land")
		})
	}
}

func TestADeferredUpdateLeavesTheDefaultLabelToTheWrite(t *testing.T) {
	repo := newDeferredStores()
	repo.On("Get", mock.Anything, "demo", "store").Return(tokenStored(), nil)
	repo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)
	svc := NewDefaultSecretStoreService(repo)

	req := updateRequest(&models.SecretStoreAuth{Type: "token"})
	req.IsDefault = true
	_, err := svc.UpdateSecretStore(context.Background(), "demo", "store", req)

	require.NoError(t, err)
	repo.AssertNotCalled(t, "RemoveDefaultLabel", mock.Anything, mock.Anything)
	assert.Empty(t, repo.released, "the token is kept: nothing to release")
}

func TestADeferredUpdateRefusedUpFrontLeavesTheTokenAlone(t *testing.T) {
	repo := newDeferredStores()
	repo.checkUpdate = apierrors.NewConflict(crd.GetSecretStoreGVR().GroupResource(), "store", errors.New("not in git"))
	repo.On("Get", mock.Anything, "demo", "store").Return(tokenStored(), nil)
	svc := NewDefaultSecretStoreService(repo)

	_, err := svc.UpdateSecretStore(context.Background(), "demo", "store",
		updateRequest(&models.SecretStoreAuth{Type: "token", Config: models.SecretAuthConfig{Token: "new"}}))

	assert.True(t, apierrors.IsConflict(err), "%v", err)
	repo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func newTokenRequest(token string) models.SecretStoreRequest {
	return updateRequest(&models.SecretStoreAuth{Type: "token", Config: models.SecretAuthConfig{Token: token}})
}

func TestADeferredCreateThatSurelyFailedReleasesItsToken(t *testing.T) {
	for name, c := range map[string]struct {
		err     error
		release bool
	}{
		"refused":   {errors.New("push refused"), true},
		"uncertain": {&engine.UnknownError{Operation: engine.Operation{Outcome: engine.OutcomeUnknown}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newDeferredStores()
			repo.On("Get", mock.Anything, "demo", "store").Return(nil, apierrors.NewNotFound(crd.GetSecretStoreGVR().GroupResource(), "store"))
			repo.On("CreateOrUpdateSecret", mock.Anything, "demo", "store-credentials", mock.Anything).Return(nil)
			repo.On("Create", mock.Anything, "demo", mock.Anything).Return(c.err)
			svc := NewDefaultSecretStoreService(repo)

			_, err := svc.CreateSecretStore(context.Background(), "demo", newTokenRequest("hvs.NEW"))

			require.Error(t, err)
			if c.release {
				assert.Equal(t, []string{"store-credentials"}, repo.released, "no store references it")
			} else {
				assert.Empty(t, repo.released, "the store may land and read it")
			}
		})
	}
}

func TestADeferredTokenRotationNeverRewritesTheLiveToken(t *testing.T) {
	for name, updateErr := range map[string]error{
		"committed":  nil,
		"conflict":   apierrors.NewConflict(crd.GetSecretStoreGVR().GroupResource(), "store", errors.New("changed")),
		"uncertain":  &engine.UnknownError{Operation: engine.Operation{Outcome: engine.OutcomeUnknown}},
		"push error": errors.New("push refused"),
	} {
		t.Run(name, func(t *testing.T) {
			repo := newDeferredStores()
			repo.On("Get", mock.Anything, "demo", "store").Return(tokenStored(), nil)
			var tokenSecret string
			repo.On("CreateOrUpdateSecret", mock.Anything, "demo", mock.Anything, map[string][]byte{"token": []byte("hvs.NEW")}).Run(func(args mock.Arguments) {
				tokenSecret = args.String(2)
			}).Return(nil)
			var written *crd.ESOSecretStore
			repo.On("Update", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
				written = args.Get(2).(*crd.ESOSecretStore)
			}).Return(updateErr)
			svc := NewDefaultSecretStoreService(repo)

			_, err := svc.UpdateSecretStore(context.Background(), "demo", "store", newTokenRequest("hvs.NEW"))

			assert.True(t, strings.HasPrefix(tokenSecret, "store-credentials-rot-"), tokenSecret)
			require.NotNil(t, written)
			assert.Equal(t, tokenSecret, written.Spec.Provider.Vault.Auth.TokenSecretRef.Name)
			switch name {
			case "committed":
				require.NoError(t, err)
				assert.Equal(t, []string{"store-credentials"}, repo.released, "the previous token, once the commit passed")
			case "uncertain":
				require.Error(t, err)
				assert.Empty(t, repo.released, "both may still be read")
			default:
				require.Error(t, err)
				assert.Equal(t, []string{tokenSecret}, repo.released, "the new token nothing references")
			}
		})
	}
}

func TestEachRotationWritesAFreshSecret(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		name, err := rotatedTokenSecret("vault")
		require.NoError(t, err)
		assert.False(t, seen[name], "never a Secret a store may read")
		seen[name] = true
		assert.True(t, isRotatedTokenSecret("vault", &crd.ESOTokenSecretRef{Name: name}))
		assert.False(t, isRotatedTokenSecret("vaul", &crd.ESOTokenSecretRef{Name: name}))
	}
	for _, other := range []string{"vault-credentials", "vault-credentials-rot-", "vault-credentials-rot-zzzzzzzzzz", "shared-token"} {
		assert.False(t, isRotatedTokenSecret("vault", &crd.ESOTokenSecretRef{Name: other}), other)
	}

	long := strings.Repeat("a", 250)
	name, err := rotatedTokenSecret(long)
	require.NoError(t, err)
	assert.Empty(t, validation.IsDNS1123Subdomain(name), name)
	assert.True(t, isRotatedTokenSecret(long, &crd.ESOTokenSecretRef{Name: name}))
}

func TestKubernetesModeDeletesATokenRotatedInGitMode(t *testing.T) {
	rotated := storedStore(crd.ESOVaultAuth{TokenSecretRef: &crd.ESOTokenSecretRef{Name: "store-credentials-rot-0a1b2c3d4e", Key: "token"}})

	repo := &mocks.SecretStoreRepository{}
	repo.On("Get", mock.Anything, "demo", "store").Return(rotated, nil)
	repo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)
	repo.On("DeleteSecret", mock.Anything, "demo", mock.Anything).Return(nil)
	_, err := NewDefaultSecretStoreService(repo).UpdateSecretStore(context.Background(), "demo", "store",
		updateRequest(&models.SecretStoreAuth{Type: "kubernetes", Config: models.SecretAuthConfig{Role: "reader"}}))
	require.NoError(t, err)
	repo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "store-credentials-rot-0a1b2c3d4e")

	repo = &mocks.SecretStoreRepository{}
	repo.On("Get", mock.Anything, "demo", "store").Return(rotated, nil)
	repo.On("Delete", mock.Anything, "demo", "store").Return(nil)
	repo.On("DeleteSecret", mock.Anything, "demo", mock.Anything).Return(nil)
	require.NoError(t, NewDefaultSecretStoreService(repo).DeleteSecretStore(context.Background(), "demo", "store"))
	repo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "store-credentials-rot-0a1b2c3d4e")
	repo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "store-credentials")

	repo = &mocks.SecretStoreRepository{}
	repo.On("Get", mock.Anything, "demo", "store").Return(rotated, nil)
	repo.On("Delete", mock.Anything, "demo", "store").Return(errors.New("api unavailable"))
	repo.On("DeleteSecret", mock.Anything, "demo", mock.Anything).Return(nil)
	require.Error(t, NewDefaultSecretStoreService(repo).DeleteSecretStore(context.Background(), "demo", "store"))
	repo.AssertNotCalled(t, "DeleteSecret", mock.Anything, "demo", "store-credentials-rot-0a1b2c3d4e")
}
