package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
	"github.com/stretchr/testify/mock"
)

// assertEmptyJSONList fails unless v marshals to [], the answer the console
// iterates without a guard; a nil slice marshals to null.
func assertEmptyJSONList(t *testing.T, name string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if string(data) != "[]" {
		t.Errorf("%s marshal to %s, want []", name, data)
	}
}

func TestNoSecretStoreListsAsEmpty(t *testing.T) {
	repo := new(mocks.SecretStoreRepository)
	repo.On("List", mock.Anything, "p").Return([]crd.ESOSecretStore{}, nil)
	repo.On("Get", mock.Anything, "p", "s").Return(&crd.ESOSecretStore{}, nil)
	svc := NewDefaultSecretStoreService(repo)

	stores, err := svc.ListSecretStores(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	assertEmptyJSONList(t, "stores", stores)

	status, err := svc.GetSecretStoreStatus(context.Background(), "p", "s")
	if err != nil {
		t.Fatal(err)
	}
	assertEmptyJSONList(t, "store conditions", status.Conditions)
}

func TestNoExternalSecretListsAsEmpty(t *testing.T) {
	svc := NewDefaultExternalSecretService(noExternalSecret{}, new(mocks.SecretStoreRepository))

	items, err := svc.ListExternalSecrets(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	assertEmptyJSONList(t, "external secrets", items)

	status, err := svc.GetExternalSecretStatus(context.Background(), "p", "e")
	if err != nil {
		t.Fatal(err)
	}
	assertEmptyJSONList(t, "external secret conditions", status.Conditions)
}

// noExternalSecret is a namespace without any ExternalSecret, and one that
// carries no status yet.
type noExternalSecret struct {
	repository.ExternalSecretRepository
}

func (noExternalSecret) List(context.Context, string) ([]crd.ESOExternalSecret, error) {
	return []crd.ESOExternalSecret{}, nil
}
func (noExternalSecret) Get(context.Context, string, string) (*crd.ESOExternalSecret, error) {
	return &crd.ESOExternalSecret{}, nil
}

// versionlessPlatform lists one chart with no default version, in a
// repository whose tags cannot be listed.
type versionlessPlatform struct {
	repository.PlatformRepository
}

func (versionlessPlatform) GetPlatformServices(context.Context) ([]models.PlatformService, error) {
	return []models.PlatformService{{Name: "trino"}}, nil
}
func (versionlessPlatform) GetPackageRepository(context.Context) (string, error) {
	return "no-path", nil
}

func TestNoVersionListsAsEmpty(t *testing.T) {
	resp, err := NewDefaultPackageSchemaService(versionlessPlatform{}).GetServiceVersions(context.Background(), "trino")
	if err != nil {
		t.Fatal(err)
	}
	assertEmptyJSONList(t, "versions", resp.Versions)
}
