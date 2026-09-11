package service

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// fakeSecrets is an in-memory ConnectionSecretRepository with the real
// ownership rule: a Secret without the managed-by label is foreign.
type fakeSecrets struct {
	mu      sync.Mutex
	secrets map[string]map[string][]byte
	managed map[string]bool
	deleted []string
	failPut error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{secrets: map[string]map[string][]byte{}, managed: map[string]bool{}}
}

func (f *fakeSecrets) put(namespace, name string, managed bool, keys ...string) {
	data := map[string][]byte{}
	for _, k := range keys {
		data[k] = []byte("x")
	}
	f.secrets[namespace+"/"+name] = data
	f.managed[namespace+"/"+name] = managed
}

func (f *fakeSecrets) CreateOrUpdateSecret(_ context.Context, namespace, name string, data map[string][]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil {
		return f.failPut
	}
	key := namespace + "/" + name
	if existing, ok := f.secrets[key]; ok {
		if !f.managed[key] {
			return repository.ErrForeignSecret
		}
		for k, v := range data {
			existing[k] = v
		}
		return nil
	}
	f.secrets[key] = data
	f.managed[key] = true
	return nil
}

func (f *fakeSecrets) DeleteSecret(_ context.Context, namespace, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.secrets, namespace+"/"+name)
	f.deleted = append(f.deleted, namespace+"/"+name)
	return nil
}

func (f *fakeSecrets) InspectSecret(_ context.Context, namespace, name string) (repository.SecretContent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.secrets[namespace+"/"+name]
	if !ok {
		return repository.SecretContent{}, false, nil
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return repository.SecretContent{Keys: keys, Managed: f.managed[namespace+"/"+name]}, true, nil
}

// fakeDescriptors serves fixed instance descriptors.
type fakeDescriptors struct {
	list []repository.Descriptor
}

func (f *fakeDescriptors) List(_ context.Context, namespace string) ([]repository.Descriptor, error) {
	var out []repository.Descriptor
	for _, d := range f.list {
		if d.Namespace == namespace {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeDescriptors) Watch(context.Context, string) (watch.Interface, error) { return nil, nil }

type testEnv struct {
	store       *gitops.MemoryStore
	deployments *gitops.Deployments
	secrets     *fakeSecrets
	descriptors *fakeDescriptors
	instances   []models.ServiceInstance
}

func newServiceUnderTest(t *testing.T) (*DefaultConnectionService, *testEnv, *gitops.MemoryStore) {
	t.Helper()
	env := &testEnv{store: gitops.NewMemoryStore(nil), secrets: newFakeSecrets(), descriptors: &fakeDescriptors{}}
	env.deployments = gitops.NewDeployments(env.store, nil)
	catalog, err := NewEmbeddedContractCatalog()
	require.NoError(t, err)
	lister := func(context.Context, string) ([]models.ServiceInstance, error) { return env.instances, nil }
	return NewDefaultConnectionService(env.deployments, env.secrets, env.descriptors, lister, catalog), env, env.store
}

func postgresRequest() models.ConnectionRequest {
	return models.ConnectionRequest{
		Name:        "warehouse",
		Type:        "database-server",
		Description: "Corporate warehouse",
		Values: map[string]any{
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "db.example.com",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "require",
		},
	}
}

// --- Credentials never reach Git ---

func TestCreateStoresCredentialsInASecretAndNotInGit(t *testing.T) {
	svc, env, store := newServiceUnderTest(t)

	response, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	file := store.Files()["projects/demo/connections/warehouse.yaml"]
	assert.Contains(t, file, "contract: database-server")
	assert.Contains(t, file, "host: db.example.com")
	assert.Contains(t, file, "secretRef:\n      name: warehouse-credentials")
	assert.NotContains(t, file, "s3cret", "a password must never be committed")
	assert.NotContains(t, file, "reader", "secret fields stay in the Secret")
	assert.Contains(t, store.Files()["projects/demo/kustomization.yaml"], `name: "conn-demo-warehouse"`, "Flux needs the conn- ConfigMap generated")
	assert.Contains(t, store.Files(), "projects/demo/project.yaml", "render-flux.sh refuses a project directory without it")

	secret := env.secrets.secrets["demo/warehouse-credentials"]
	assert.Equal(t, "s3cret", string(secret["password"]))
	assert.Equal(t, "reader", string(secret["username"]))

	assert.Equal(t, "warehouse-credentials", response.Values["secretRef"])
	require.NotNil(t, response.CredentialsSecret)
	assert.True(t, response.CredentialsSecret.Owned)
	assert.Equal(t, "Corporate warehouse", response.Description)
	assert.Equal(t, "okdp: create connection demo/warehouse by anonymous", store.Messages[0])
}

func TestCreateOnATakenNameIsAConflictAndLeavesTheCredentialsAlone(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	again := postgresRequest()
	again.Values["password"] = "other"
	_, err = svc.Create(context.Background(), "demo", again)

	assert.True(t, apierrors.IsAlreadyExists(err), "got %v", err)
	assert.Equal(t, "s3cret", string(env.secrets.secrets["demo/warehouse-credentials"]["password"]))
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	svc, _, store := newServiceUnderTest(t)
	for name, mutate := range map[string]func(*models.ConnectionRequest){
		"bad name":         func(r *models.ConnectionRequest) { r.Name = "Bad_Name" },
		"dotted name":      func(r *models.ConnectionRequest) { r.Name = "a.b" },
		"unknown contract": func(r *models.ConnectionRequest) { r.Type = "oracle" },
		"missing field":    func(r *models.ConnectionRequest) { delete(r.Values, "host") },
		"unknown field":    func(r *models.ConnectionRequest) { r.Values["colour"] = "red" },
	} {
		t.Run(name, func(t *testing.T) {
			req := postgresRequest()
			mutate(&req)
			_, err := svc.Create(context.Background(), "demo", req)
			assert.True(t, IsValidationError(err), "got %v", err)
		})
	}
	assert.Empty(t, store.Files())
}

func TestUpdateOnlyWritesTheResubmittedCredentials(t *testing.T) {
	svc, env, store := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	edit := postgresRequest()
	delete(edit.Values, "username")
	delete(edit.Values, "password")
	edit.Values["port"] = float64(6432)
	// A client sends back what it read, secretRef included.
	edit.Values["secretRef"] = "warehouse-credentials"
	response, err := svc.Update(context.Background(), "demo", "warehouse", edit)
	require.NoError(t, err)

	assert.Equal(t, "warehouse-credentials", response.Values["secretRef"], "the reference survives an edit without credentials")
	assert.Contains(t, store.Files()["projects/demo/connections/warehouse.yaml"], "port: 6432")
	assert.Equal(t, "s3cret", string(env.secrets.secrets["demo/warehouse-credentials"]["password"]))

	edit.Values["password"] = "rotated"
	_, err = svc.Update(context.Background(), "demo", "warehouse", edit)
	require.NoError(t, err)
	assert.Equal(t, "rotated", string(env.secrets.secrets["demo/warehouse-credentials"]["password"]))
	assert.Equal(t, "reader", string(env.secrets.secrets["demo/warehouse-credentials"]["username"]), "the merge keeps what was not retyped")
}

func TestUpdateOfAnUnknownConnectionIsNotFound(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t)
	_, err := svc.Update(context.Background(), "demo", "warehouse", postgresRequest())
	assert.True(t, IsNotFound(err), "got %v", err)
}

// --- Pointing at a Secret the console does not own ---

func TestCreateCanPointAtAnExistingSecret(t *testing.T) {
	svc, env, store := newServiceUnderTest(t)
	env.secrets.put("demo", "vault-db", false, "username", "password")

	req := postgresRequest()
	req.ExistingSecret = "vault-db"
	delete(req.Values, "password")
	response, err := svc.Create(context.Background(), "demo", req)
	require.NoError(t, err)

	assert.Contains(t, store.Files()["projects/demo/connections/warehouse.yaml"], "name: vault-db")
	assert.False(t, response.CredentialsSecret.Owned)
	_, wrote := env.secrets.secrets["demo/warehouse-credentials"]
	assert.False(t, wrote, "nothing is written when the credentials live elsewhere")
}

func TestCreateRefusesASecretThatCannotWork(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	env.secrets.put("demo", "partial", false, "username")
	env.secrets.put("demo", "someone-credentials", true, "username", "password")

	for _, name := range []string{"absent", "partial", "someone-credentials"} {
		req := postgresRequest()
		req.ExistingSecret = name
		_, err := svc.Create(context.Background(), "demo", req)
		assert.True(t, IsValidationError(err), "%s: got %v", name, err)
	}
}

func TestUpdateToAnExistingSecretRemovesTheOneItOwned(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)
	env.secrets.put("demo", "vault-db", false, "username", "password")

	edit := postgresRequest()
	edit.ExistingSecret = "vault-db"
	_, err = svc.Update(context.Background(), "demo", "warehouse", edit)
	require.NoError(t, err)

	_, stillThere := env.secrets.secrets["demo/warehouse-credentials"]
	assert.False(t, stillThere, "the Secret the console wrote is unreferenced now")
	_, vault := env.secrets.secrets["demo/vault-db"]
	assert.True(t, vault)
}

// --- Delete ---

func TestDeleteRemovesTheSecretItWrote(t *testing.T) {
	svc, env, store := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "demo", "warehouse"))

	assert.NotContains(t, store.Files(), "projects/demo/connections/warehouse.yaml")
	assert.Contains(t, store.Files()["projects/demo/kustomization.yaml"], "configMapGenerator: []")
	_, stillThere := env.secrets.secrets["demo/warehouse-credentials"]
	assert.False(t, stillThere)
}

// A vault-projected Secret following the naming convention is still not ours.
func TestDeleteLeavesASecretItDoesNotOwn(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	env.secrets.put("demo", "warehouse-credentials", false, "username", "password")
	req := postgresRequest()
	req.ExistingSecret = "warehouse-credentials"
	_, err := svc.Create(context.Background(), "demo", req)
	require.NoError(t, err)

	require.NoError(t, svc.Delete(context.Background(), "demo", "warehouse"))

	_, stillThere := env.secrets.secrets["demo/warehouse-credentials"]
	assert.True(t, stillThere, "a Secret without the managed-by label is left alone")
}

// Both engines fail to render an instance whose connection file is gone.
func TestDeleteRefusesAConnectionAnInstanceLayersIn(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)
	_, err = env.deployments.CreateInstance(context.Background(), auth.Actor{Username: "alice"}, gitops.InstanceState{
		Instance: gitops.Instance{Name: "hive", Project: "demo", Service: "hive-metastore", Chart: "oci://r/hive-metastore", Version: "1.0.0", Connections: []string{"warehouse"}},
	})
	require.NoError(t, err)

	err = svc.Delete(context.Background(), "demo", "warehouse")
	assert.True(t, IsValidationError(err), "got %v", err)
	assert.Contains(t, err.Error(), "hive")
	_, stillThere := env.secrets.secrets["demo/warehouse-credentials"]
	assert.True(t, stillThere)
}

func TestDeleteOfAnUnknownConnectionIsNotFound(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t)
	assert.True(t, IsNotFound(svc.Delete(context.Background(), "demo", "nope")))
}

// --- Lists ---

func TestListReadsTheConnectionFiles(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	list, err := svc.List(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "warehouse", list[0].Name)
	assert.Equal(t, "database-server", list[0].Type)
	assert.Equal(t, "Corporate warehouse", list[0].Description)
	assert.ElementsMatch(t, []string{"username", "password"}, list[0].SecretFields)

	other, err := svc.List(context.Background(), "other")
	require.NoError(t, err)
	assert.Empty(t, other)
	assert.True(t, svc.Catalog(context.Background()).CRDAvailable)
}

func descriptorWith(release string, outputs ...repository.DescriptorOutput) repository.Descriptor {
	return repository.Descriptor{Release: release, Namespace: "demo", Service: "trino", Outputs: outputs}
}

func TestListInternalReadsTheDescriptorOutputs(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	out := repository.DescriptorOutput{Name: "demo-trino", Contract: "trino", Values: map[string]any{"uri": "http://demo-trino.demo.svc:8080"}}
	out.SecretRef.Name = "demo-trino-trino-credentials"
	env.descriptors.list = []repository.Descriptor{descriptorWith("demo-trino", out), descriptorWith("demo-ui")}

	internal, err := svc.ListInternal(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, internal, 1, "an instance that publishes nothing is absent")
	assert.Equal(t, "demo-trino", internal[0].Name)
	assert.Equal(t, "demo-trino", internal[0].ReleaseName)
	assert.True(t, internal[0].Managed)
}

func TestListSelectableOffersBothKindsOfTheContract(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	_, err := svc.Create(context.Background(), "demo", models.ConnectionRequest{
		Name: "shared-hms", Type: "hive", Values: map[string]any{"thriftUri": "thrift://hms:9083"},
	})
	require.NoError(t, err)
	_, err = svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)
	env.descriptors.list = []repository.Descriptor{
		descriptorWith("demo-hive", repository.DescriptorOutput{Name: "demo-hive", Contract: "hive"}),
		// database-server has no naming convention: an output of it cannot be
		// referenced by name, so it is never offered.
		descriptorWith("demo-pg", repository.DescriptorOutput{Name: "demo-pg", Contract: "database-server"}),
	}

	selectable, err := svc.ListSelectable(context.Background(), "demo", "hive")
	require.NoError(t, err)
	require.Len(t, selectable, 2)
	assert.Equal(t, "demo-hive", selectable[0].Name)
	assert.True(t, selectable[0].Managed)
	assert.Equal(t, "demo-hive", selectable[0].ProvidedBy)
	assert.Equal(t, "shared-hms", selectable[1].Name)
	assert.False(t, selectable[1].Managed)

	databases, err := svc.ListSelectable(context.Background(), "demo", "database-server")
	require.NoError(t, err)
	require.Len(t, databases, 1)
	assert.Equal(t, "warehouse", databases[0].Name)

	internal, err := svc.ListInternal(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, internal, 1)
	assert.Equal(t, "demo-hive", internal[0].Name)
}

// A contract without secret fields carries no secretRef, even when a Secret
// is named.
func TestAContractWithoutSecretsHasNoSecretRef(t *testing.T) {
	svc, _, store := newServiceUnderTest(t)
	response, err := svc.Create(context.Background(), "demo", models.ConnectionRequest{
		Name: "shared-hms", Type: "hive", ExistingSecret: "whatever", Values: map[string]any{"thriftUri": "thrift://hms:9083"},
	})
	require.NoError(t, err)
	assert.NotContains(t, store.Files()["projects/demo/connections/shared-hms.yaml"], "secretRef")
	assert.Nil(t, response.CredentialsSecret)
}

func TestListConsumersReadsTheBoundConnections(t *testing.T) {
	svc, env, _ := newServiceUnderTest(t)
	env.instances = []models.ServiceInstance{
		{Name: "hive", ReleaseName: "demo-hive", Status: "Ready", Connections: []models.ServiceConnection{{Name: "warehouse", Resolved: true}}},
		{Name: "trino", ReleaseName: "demo-trino", Status: "Pending", Connections: []models.ServiceConnection{{Name: "demo-hive", Resolved: true}}},
		{Name: "ui", ReleaseName: "demo-ui"},
	}

	consumers, err := svc.ListConsumers(context.Background(), "demo", "warehouse")
	require.NoError(t, err)
	require.Len(t, consumers, 1)
	assert.Equal(t, models.ConnectionConsumer{Service: "hive", ReleaseName: "demo-hive", Status: "Ready", Effective: true}, consumers[0])
}

// --- Connectivity test ---

func TestTestRejectsBadInputBeforeDialing(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t)

	tests := []struct {
		name    string
		request models.ConnectionTestRequest
		reason  string
	}{
		{
			name:    "unknown type",
			request: models.ConnectionTestRequest{Type: "oracle", Values: map[string]any{}},
			reason:  models.TestReasonInvalidConfig,
		},
		{
			name:    "missing required value",
			request: models.ConnectionTestRequest{Type: "database-server", Values: map[string]any{"host": "db"}},
			reason:  models.TestReasonInvalidConfig,
		},
		{
			name: "type that has no tester",
			request: models.ConnectionTestRequest{Type: "trino", Values: map[string]any{
				"host": "trino.demo.svc.cluster.local", "port": float64(8080),
			}},
			reason: models.TestReasonInvalidConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.Test(context.Background(), tt.request)

			assert.False(t, result.Success)
			assert.Equal(t, tt.reason, result.Reason)
			assert.NotEmpty(t, result.Message)
		})
	}
}

func TestTestReportsAnUnreachableHost(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t)

	result := svc.Test(context.Background(), models.ConnectionTestRequest{
		Type: "database-server",
		Values: map[string]any{
			// Reserved by RFC 6761 to never resolve.
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "nothing.invalid",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "disable",
		},
	})

	assert.False(t, result.Success)
	assert.Contains(t, []string{models.TestReasonUnreachable, models.TestReasonTimeout}, result.Reason)
}

// One type, one form, two drivers: the probe reads the engine field. An engine
// the server has no driver for must say so rather than fall back to a default,
// which would report a MySQL server unreachable on the PostgreSQL port. The
// catalog's enum stops this at the door, so the guard is tested directly.
func TestDatabaseProbeRejectsAnUnknownEngine(t *testing.T) {
	checks := testDatabaseServer(context.Background(), connectionValues{
		"engine": "oracle",
		"host":   "db.example.com",
		"port":   float64(1521),
	})

	require.Len(t, checks, 1)
	assert.False(t, checks[0].Success)
	assert.Equal(t, models.TestReasonInvalidConfig, checks[0].Reason)
	assert.Contains(t, checks[0].Message, "oracle")
}

// The form does not send a derived field, so the button must test the values as
// they will be stored, not the raw payload. Validating before normalizing asked
// for a JDBC driver nobody was offered, and every test came back "JDBC driver is
// required" without a packet ever leaving the server.
func TestTestNormalizesBeforeValidating(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t)

	result := svc.Test(context.Background(), models.ConnectionTestRequest{
		Type: "database-server",
		Values: map[string]any{
			"engine": "postgresql",
			// Reserved by RFC 6761 to never resolve, so the probe fails on the
			// network rather than on the payload.
			"host":     "nothing.invalid",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "disable",
		},
	})

	assert.False(t, result.Success)
	assert.NotEqual(t, models.TestReasonInvalidConfig, result.Reason,
		"the payload is complete once normalized, the failure must come from the network")
}
