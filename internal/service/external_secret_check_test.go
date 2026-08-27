package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
)

// vaultStore is a SecretStore as the cluster holds it, pointing at the test
// server and authenticating with a token.
func vaultStore(server, version string, tokenRef *crd.ESOTokenSecretRef, k8s *crd.ESOKubernetesAuth) *crd.ESOSecretStore {
	st := &crd.ESOSecretStore{}
	st.Name = "store"
	st.Namespace = "demo"
	st.Spec.Provider.Vault = &crd.ESOVaultProvider{
		Server:  server,
		Path:    "secret",
		Version: version,
		Auth:    crd.ESOVaultAuth{TokenSecretRef: tokenRef, Kubernetes: k8s},
	}
	return st
}

// storeRepoFake is local to this file on purpose: the shared mock gains its
// Available method on another branch, and adding it here as well would collide
// at merge time for a test that needs two methods.
type storeRepoFake struct {
	repository.SecretStoreRepository
	store  *crd.ESOSecretStore
	secret map[string][]byte
}

func (f *storeRepoFake) Get(context.Context, string, string) (*crd.ESOSecretStore, error) {
	return f.store, nil
}

func (f *storeRepoFake) GetSecretData(context.Context, string, string) (map[string][]byte, error) {
	return f.secret, nil
}

func checkService(t *testing.T, store *crd.ESOSecretStore, token string) *DefaultExternalSecretService {
	t.Helper()
	return &DefaultExternalSecretService{secretStoreRepo: &storeRepoFake{
		store:  store,
		secret: map[string][]byte{"token": []byte(token)},
	}}
}

func checkRef(t *testing.T, svc *DefaultExternalSecretService, key, property string) *models.ExternalSecretCheckResponse {
	t.Helper()
	resp, err := svc.CheckRemoteRef(context.Background(), "demo", models.ExternalSecretCheckRequest{
		SecretStoreRef: "store",
		RemoteRef:      models.ExternalSecretRemote{Key: key, Property: property},
	})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
	return resp
}

func kvV2Server(t *testing.T, keys map[string]map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		fields, ok := keys[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		parts := make([]string, 0, len(fields))
		for k, v := range fields {
			parts = append(parts, `"`+k+`":"`+v+`"`)
		}
		_, _ = w.Write([]byte(`{"data":{"data":{` + strings.Join(parts, ",") + `},"metadata":{}}}`))
	}))
}

// The point of the whole endpoint: a key that is not there is named as such,
// before anything is created.
func TestCheckReportsAMissingKey(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"client-externe": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "nexistepas", "")
	if !resp.Verifiable || resp.Found {
		t.Fatalf("a missing key was not reported: %+v", resp)
	}
}

func TestCheckReportsAnExistingKeyAndItsProperties(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"client-externe": {"db_password": "x", "api_token": "y"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "client-externe", "")
	if !resp.Verifiable || !resp.Found {
		t.Fatalf("an existing key was not found: %+v", resp)
	}
	if len(resp.Properties) != 2 || resp.Properties[0] != "api_token" || resp.Properties[1] != "db_password" {
		t.Fatalf("properties are %v, want them sorted: [api_token db_password]", resp.Properties)
	}
}

// The values must never leave: this endpoint confirms a path, it is not a way
// to read secrets from a form.
func TestCheckNeverReturnsValues(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"client-externe": {"db_password": "tres-secret"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "client-externe", "")
	for _, p := range resp.Properties {
		if strings.Contains(p, "tres-secret") {
			t.Fatal("a value leaked through the property names")
		}
	}
	if strings.Contains(resp.Message, "tres-secret") {
		t.Fatalf("a value leaked through the message: %s", resp.Message)
	}
}

// A key can exist while the property named in the import does not, which fails
// the sync just the same.
func TestCheckReportsAMissingProperty(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"client-externe": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "client-externe", "champ-absent")
	if !resp.Verifiable || resp.Found {
		t.Fatalf("a missing property was not reported: %+v", resp)
	}
	if len(resp.Properties) != 1 {
		t.Fatalf("the available properties were not listed: %+v", resp)
	}
}

// The mistake this check exists to catch: pasting the KV v2 API path, which
// Vault holds but an import cannot find.
func TestCheckNamesTheKvV2PrefixMistake(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"client-externe": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "data/client-externe", "")
	if resp.Found {
		t.Fatal("the API path was reported as a valid key")
	}
	if !strings.Contains(resp.Message, "client-externe") || !strings.Contains(resp.Message, "KV v2") {
		t.Fatalf("the message does not name the mistake: %s", resp.Message)
	}
}

// A store the control plane cannot authenticate as must say so. Reporting
// success would be the false green this check exists to remove.
func TestCheckSaysWhenItCannotVerify(t *testing.T) {
	store := vaultStore("https://vault.example.com", "v2", nil, &crd.ESOKubernetesAuth{
		MountPath: "kubernetes", Role: "demo-role",
	})
	svc := checkService(t, store, "")

	resp := checkRef(t, svc, "client-externe", "")
	if resp.Verifiable {
		t.Fatalf("a kubernetes-auth store was reported as verifiable: %+v", resp)
	}
	if resp.Found {
		t.Fatal("an unverifiable check reported the key as found")
	}
}

// An unreachable Vault is not a verdict on the key either.
func TestCheckSaysWhenVaultIsUnreachable(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := closed.URL
	closed.Close()
	svc := checkService(t, vaultStore(address, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "client-externe", "")
	if resp.Verifiable || resp.Found {
		t.Fatalf("an unreachable vault produced a verdict: %+v", resp)
	}
}

// A key goes straight into the URL, so one carrying a query or a parent segment
// would make the check report on another path.
func TestCheckRefusesAKeyThatRetargetsTheURL(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"client-externe": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	for _, key := range []string{"a?b=1", "../../sys/seal", "a b", "a#b"} {
		_, err := svc.CheckRemoteRef(context.Background(), "demo", models.ExternalSecretCheckRequest{
			SecretStoreRef: "store",
			RemoteRef:      models.ExternalSecretRemote{Key: key},
		})
		if err == nil || !IsValidationError(err) {
			t.Fatalf("key %q was accepted: %v", key, err)
		}
	}
}

// KV v1 holds the fields at the top level, so reading it like a v2 secret would
// report an existing key as empty.
func TestCheckReadsKvV1(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/client-externe" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"db_password":"x","api_token":"y"}}`))
	}))
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v1", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.jeton")

	resp := checkRef(t, svc, "client-externe", "db_password")
	if !resp.Found || len(resp.Properties) != 2 {
		t.Fatalf("a KV v1 secret was misread: %+v", resp)
	}
}
