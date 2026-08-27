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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	store     *crd.ESOSecretStore
	secret    map[string][]byte
	secretErr error
}

func (f *storeRepoFake) Get(context.Context, string, string) (*crd.ESOSecretStore, error) {
	return f.store, nil
}

func (f *storeRepoFake) GetSecretData(context.Context, string, string) (map[string][]byte, error) {
	return f.secret, f.secretErr
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
	vault := kvV2Server(t, map[string]map[string]string{"external-client": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "does-not-exist", "")
	if !resp.Verifiable || resp.Found {
		t.Fatalf("a missing key was not reported: %+v", resp)
	}
}

func TestCheckReportsAnExistingKeyAndItsProperties(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"external-client": {"db_password": "x", "api_token": "y"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "")
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
	vault := kvV2Server(t, map[string]map[string]string{"external-client": {"db_password": "very-secret"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "")
	for _, p := range resp.Properties {
		if strings.Contains(p, "very-secret") {
			t.Fatal("a value leaked through the property names")
		}
	}
	if strings.Contains(resp.Message, "very-secret") {
		t.Fatalf("a value leaked through the message: %s", resp.Message)
	}
}

// A key can exist while the property named in the import does not, which fails
// the sync just the same.
func TestCheckReportsAMissingProperty(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"external-client": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "missing-field")
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
	vault := kvV2Server(t, map[string]map[string]string{"external-client": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "data/external-client", "")
	if resp.Found {
		t.Fatal("the API path was reported as a valid key")
	}
	if !strings.Contains(resp.Message, "external-client") || !strings.Contains(resp.Message, "KV v2") {
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

	resp := checkRef(t, svc, "external-client", "")
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
	svc := checkService(t, vaultStore(address, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "")
	if resp.Verifiable || resp.Found {
		t.Fatalf("an unreachable vault produced a verdict: %+v", resp)
	}
}

// A key goes straight into the URL, so one carrying a query or a parent segment
// would make the check report on another path.
func TestCheckRefusesAKeyThatRetargetsTheURL(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"external-client": {"db_password": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

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
		if r.URL.Path != "/v1/secret/external-client" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"db_password":"x","api_token":"y"}}`))
	}))
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v1", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "db_password")
	if !resp.Found || len(resp.Properties) != 2 {
		t.Fatalf("a KV v1 secret was misread: %+v", resp)
	}
}

// A 403 says the policy refused the read, not that the key is absent: Vault
// answers 403 both for a path a policy denies and for one it will not admit
// exists. Reporting Found:false painted an existing key as missing.
func TestCheckTreatsAForbiddenReadAsUnverifiable(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "")
	if resp.Verifiable {
		t.Fatalf("a refused read was reported as a verdict on the key: %+v", resp)
	}
	if resp.Found {
		t.Fatal("an unverifiable check reported the key as found")
	}
}

// The message must not blame the network for a failure that happened after
// Vault answered, or before any request left.
func TestCheckDoesNotBlameReachabilityForOtherFailures(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("this is not json"))
	}))
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "external-client", "")
	if resp.Verifiable {
		t.Fatalf("an unreadable answer produced a verdict: %+v", resp)
	}
	if strings.Contains(resp.Message, "could not be reached") {
		t.Fatalf("vault answered, yet the message blames reachability: %s", resp.Message)
	}
}

// The server address is caller-supplied, so the answer is read up to a bound.
// The proof has to distinguish the two cases, so this serves a body that is
// valid JSON throughout: under the cap it parses and the key is found, over it
// the read is truncated mid-document and no verdict is reached. Without the
// cap both sizes would parse.
func TestCheckBoundsTheResponseItReads(t *testing.T) {
	serve := func(size int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"data":{"k":"` + strings.Repeat("A", size) + `"}}}`))
		}))
	}

	small := serve(1024)
	defer small.Close()
	svc := checkService(t, vaultStore(small.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")
	if resp := checkRef(t, svc, "external-client", ""); !resp.Verifiable || !resp.Found {
		t.Fatalf("a secret well under the cap was not read: %+v", resp)
	}

	big := serve(maxVaultResponseBytes + 1024)
	defer big.Close()
	svc = checkService(t, vaultStore(big.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")
	resp := checkRef(t, svc, "external-client", "")
	if resp.Verifiable {
		t.Fatalf("an answer past the cap was read whole and produced a verdict: %+v", resp)
	}
}

// The message reads for one property as well as for several.
func TestCheckCountsPropertiesReadably(t *testing.T) {
	vault := kvV2Server(t, map[string]map[string]string{"one": {"only": "x"}})
	defer vault.Close()
	svc := checkService(t, vaultStore(vault.URL, "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil), "s.token")

	resp := checkRef(t, svc, "one", "")
	if strings.Contains(resp.Message, "propertie(s)") || !strings.Contains(resp.Message, "1 property") {
		t.Fatalf("unreadable count: %s", resp.Message)
	}
}

// The credentials Secret is not the store. Wrapped, its NotFound reached the
// handler and came back as "secret store not found" for a store sitting right
// there in the list.
func TestCheckDoesNotReportAMissingCredentialAsAMissingStore(t *testing.T) {
	repo := &storeRepoFake{
		store:     vaultStore("https://vault.example.com", "v2", &crd.ESOTokenSecretRef{Name: "store-credentials", Key: "token"}, nil),
		secretErr: apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "store-credentials"),
	}
	svc := &DefaultExternalSecretService{secretStoreRepo: repo}

	_, err := svc.CheckRemoteRef(context.Background(), "demo", models.ExternalSecretCheckRequest{
		SecretStoreRef: "store",
		RemoteRef:      models.ExternalSecretRemote{Key: "external-client"},
	})
	if err == nil {
		t.Fatal("a missing credentials secret was accepted")
	}
	if apierrors.IsNotFound(err) {
		t.Fatalf("the error still reads as a Kubernetes NotFound, so the handler will call the store missing: %v", err)
	}
	if !IsValidationError(err) || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("the error does not name the credentials secret: %v", err)
	}
}

// The check and the create must agree on what a key may look like. They did
// not: the check refused "a?b=1" as an invalid path while the create accepted
// it and stored an import that can never resolve.
func TestCreateRefusesTheSameKeysTheCheckRefuses(t *testing.T) {
	for _, key := range []string{"a?b=1", "../../sys/seal", "a b", "a#b"} {
		err := validateExternalSecretRequest(models.ExternalSecretRequest{
			Name:            "my-import",
			SecretStoreRef:  "store",
			RefreshInterval: "1m",
			Target:          models.ExternalSecretTarget{Name: "my-secret"},
			Data: []models.ExternalSecretDataEntry{
				{SecretKey: "pwd", RemoteRef: models.ExternalSecretRemote{Key: key}},
			},
		})
		if err == nil {
			t.Fatalf("create accepted key %q that the check refuses", key)
		}
		if !IsValidationError(err) {
			t.Fatalf("key %q refused as a platform failure: %v", key, err)
		}
	}
	// A nested key stays legal on both paths.
	err := validateExternalSecretRequest(models.ExternalSecretRequest{
		Name:            "my-import",
		SecretStoreRef:  "store",
		RefreshInterval: "1m",
		Target:          models.ExternalSecretTarget{Name: "my-secret"},
		Data: []models.ExternalSecretDataEntry{
			{SecretKey: "pwd", RemoteRef: models.ExternalSecretRemote{Key: "team/app/db"}},
		},
	})
	if err != nil {
		t.Fatalf("a nested key was rejected: %v", err)
	}
}
