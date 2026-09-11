package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

// stubPlatform serves a catalog with one chart.
type stubPlatform struct {
	repository.PlatformRepository
}

func (stubPlatform) GetPlatformServices(context.Context) ([]models.PlatformService, error) {
	return []models.PlatformService{{Name: "trino", DefaultVersion: "480.0.0-p21", Versions: []string{"480.0.0-p21"}}}, nil
}
func (stubPlatform) GetPackageRepository(context.Context) (string, error) {
	return "quay.io/okdp/platform-charts", nil
}
func (stubPlatform) Invalidate() {}

// stubSchema answers the parameter schema of every chart.
type stubSchema struct {
	PackageSchemaService
	schema map[string]any
}

func (s stubSchema) GetParameterSchema(context.Context, string, string) (map[string]any, error) {
	return s.schema, nil
}

// stubEngine reports fixed statuses.
type stubEngine struct {
	statuses map[string]repository.EngineStatus
	events   chan watch.Event
}

func (stubEngine) Name() string { return "flux" }
func (e stubEngine) List(context.Context, string) (map[string]repository.EngineStatus, error) {
	return e.statuses, nil
}
func (e stubEngine) Get(_ context.Context, _, release string) (repository.EngineStatus, error) {
	return e.statuses[release], nil
}
func (e stubEngine) Watch(context.Context) (watch.Interface, error) {
	if e.events == nil {
		return nil, errors.New("no watch")
	}
	return watch.NewProxyWatcher(e.events), nil
}
func (stubEngine) ReleaseOf(obj *unstructured.Unstructured) (string, string) {
	project, _, _ := unstructured.NestedString(obj.Object, "spec", "targetNamespace")
	return project, obj.GetName()
}

func trinoSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"workers": map[string]any{"type": "integer", "minimum": 1},
			"catalogs": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
					"hive": map[string]any{"type": "string", "x-okdp-connection-ref": map[string]any{"contract": "hive"}},
				},
			}},
		},
	}
}

func newGitServiceUnderTest(t *testing.T, engine repository.EngineAdapter, descriptors repository.DescriptorRepository) (*DefaultServiceService, *gitops.MemoryStore) {
	t.Helper()
	store := gitops.NewMemoryStore(map[string]string{
		"projects/demo/connections/lake-hms.yaml": "connections:\n  lake-hms:\n    contract: hive\n    thriftUri: thrift://hms:9083\n",
	})
	svc := NewDefaultServiceService(ServiceDeps{
		Deployments:   gitops.NewDeployments(store, nil),
		PlatformRepo:  stubPlatform{},
		SchemaService: stubSchema{schema: trinoSchema()},
		Engine:        engine,
		Descriptors:   descriptors,
		K8sClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			{Version: "v1", Resource: "pods"}:                   "PodList",
			{Version: "v1", Resource: "persistentvolumeclaims"}: "PersistentVolumeClaimList",
			{Version: "v1", Resource: "events"}:                 "EventList",
		}),
	})
	return svc, store
}

func aliceContext() context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{Username: "alice", Name: "Alice Martin", Email: "alice@example.com"})
}

func TestDeployCommitsTheInstanceFiles(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)

	instance, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{
		Service:      "trino",
		InstanceName: "sql",
		Parameters: map[string]any{
			"workers":  float64(2),
			"catalogs": []any{map[string]any{"name": "lake", "hive": "lake-hms"}, map[string]any{"name": "local", "hive": "demo-hive"}},
		},
	})
	require.NoError(t, err)

	files := store.Files()
	assert.Equal(t, "name: sql\nproject: demo\nservice: trino\nchart: oci://quay.io/okdp/platform-charts/trino\nversion: 480.0.0-p21\nconnections:\n  - lake-hms\n", files["projects/demo/services/sql/instance.yaml"],
		"the external connection the parameters name is layered in; the internal one (demo-hive) is not a file")
	assert.Contains(t, files["projects/demo/services/sql/values.yaml"], "workers: 2")
	assert.Contains(t, files["projects/demo/services/sql/helmrelease.yaml"], `name: "conn-demo-lake-hms"`)
	assert.Contains(t, files, "projects/demo/services/sql/kustomization.yaml")
	assert.Equal(t, "okdp: deploy demo/sql by alice\n\nCo-Authored-By: Alice Martin <alice@example.com>", store.Messages[len(store.Messages)-1])

	assert.Equal(t, "demo-sql", instance.ReleaseName)
	assert.Equal(t, repository.PhasePending, instance.Status, "committed, the engine has not seen it yet")
	assert.NotEmpty(t, instance.Revision)

	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", InstanceName: "sql"})
	assert.True(t, apierrors.IsAlreadyExists(err), "got %v", err)
}

func TestDeployRefusesInvalidParametersWithoutCommitting(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)
	before := len(store.Messages)

	_, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", Parameters: map[string]any{"workers": float64(0)}})
	assert.True(t, IsValidationError(err), "got %v", err)
	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", Parameters: map[string]any{"colour": "red"}})
	assert.True(t, IsValidationError(err), "got %v", err)
	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "unknown"})
	assert.True(t, IsValidationError(err), "got %v", err)
	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", InstanceName: "Bad_Name"})
	assert.True(t, IsValidationError(err), "got %v", err)
	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", Parameters: map[string]any{"global": map[string]any{}}})
	assert.True(t, IsValidationError(err), "global is the platform's: got %v", err)

	assert.Equal(t, before, len(store.Messages), "nothing may be committed")
}

func TestUpdateMergesIntoValuesAndMovesTheVersion(t *testing.T) {
	engine := stubEngine{statuses: map[string]repository.EngineStatus{}}
	svc, store := newGitServiceUnderTest(t, engine, nil)
	_, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", Parameters: map[string]any{"workers": float64(2)}})
	require.NoError(t, err)

	engine.statuses["demo-trino"] = repository.EngineStatus{Found: true, Phase: repository.PhaseReady, Revision: "480.0.0-p21"}
	instance, err := svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{
		Tag:        "481.0.0-p01",
		Parameters: map[string]any{"catalogs": []any{map[string]any{"name": "lake", "hive": "lake-hms"}}},
	})
	require.NoError(t, err)

	values := store.Files()["projects/demo/services/trino/values.yaml"]
	assert.Contains(t, values, "workers: 2", "keys not submitted are kept")
	assert.Contains(t, values, "hive: lake-hms")
	assert.Contains(t, store.Files()["projects/demo/services/trino/instance.yaml"], "version: 481.0.0-p01")
	assert.Contains(t, store.Files()["projects/demo/services/trino/instance.yaml"], "- lake-hms")
	assert.Equal(t, "okdp: update demo/trino by alice\n\nCo-Authored-By: Alice Martin <alice@example.com>", store.Messages[len(store.Messages)-1])
	assert.Equal(t, repository.PhaseUpdating, instance.Status, "the engine still runs the previous version")
	assert.NotEmpty(t, instance.Revision)

	_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": "many"}})
	assert.True(t, IsValidationError(err), "got %v", err)
	_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "nope", models.ServiceUpdateRequest{})
	assert.True(t, apierrors.IsNotFound(err), "got %v", err)
}

type stubDescriptors struct {
	list []repository.Descriptor
}

func (d stubDescriptors) List(context.Context, string) ([]repository.Descriptor, error) {
	return d.list, nil
}
func (stubDescriptors) Watch(context.Context, string) (watch.Interface, error) {
	return nil, errors.New("no watch")
}

func TestListAssemblesGitAndClusterState(t *testing.T) {
	engine := stubEngine{statuses: map[string]repository.EngineStatus{
		"demo-hive": {Found: true, Phase: repository.PhaseReady, Revision: "1.0.0", CreatedAt: time.Unix(0, 0)},
		"demo-sql":  {Found: true, Phase: repository.PhaseError, Message: "install failed: boom"},
	}}
	descriptors := stubDescriptors{list: []repository.Descriptor{{
		Release: "demo-hive", Namespace: "demo", URL: "https://hive.example", Usage: "# Hive",
		Outputs: []repository.DescriptorOutput{{Name: "demo-hive", Contract: "hive"}},
	}}}
	svc, _ := newGitServiceUnderTest(t, engine, descriptors)
	d := svc.deployments
	for _, st := range []gitops.InstanceState{
		{Instance: gitops.Instance{Name: "hive", Project: "demo", Service: "hive-metastore", Chart: "oci://r/hive-metastore", Version: "1.0.0"}},
		{Instance: gitops.Instance{Name: "sql", Project: "demo", Service: "trino", Chart: "oci://r/trino", Version: "480.0.0-p21"},
			Values: map[string]any{"catalogs": []any{map[string]any{"hive": "demo-hive"}}}},
		{Instance: gitops.Instance{Name: "new", Project: "demo", Service: "trino", Chart: "oci://r/trino", Version: "480.0.0-p21"}},
	} {
		_, err := d.CreateInstance(context.Background(), auth.Actor{Username: "bob"}, st)
		require.NoError(t, err)
	}

	list, err := svc.ListServices(context.Background(), "demo")
	require.NoError(t, err)
	require.Len(t, list, 3)
	byName := map[string]models.ServiceInstance{}
	for _, i := range list {
		byName[i.Name] = i
	}

	assert.Equal(t, "Ready", byName["hive"].Status)
	assert.Equal(t, "https://hive.example", byName["hive"].URL)
	assert.Equal(t, "# Hive", byName["hive"].Usage)
	assert.Equal(t, "1970-01-01T00:00:00Z", byName["hive"].CreatedAt)

	assert.Equal(t, "Error", byName["sql"].Status)
	assert.Equal(t, "install failed: boom", byName["sql"].StatusMessage)
	require.Len(t, byName["sql"].Connections, 1)
	assert.Equal(t, models.ServiceConnection{Name: "demo-hive", Namespace: "demo", Kind: models.ConnectionKindInternal, Resolved: true}, byName["sql"].Connections[0])

	assert.Equal(t, "Pending", byName["new"].Status)
	assert.True(t, strings.Contains(byName["new"].StatusMessage, "flux"))
}

func TestDeleteRemovesTheInstanceDirectory(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)
	_, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino"})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteService(aliceContext(), "demo", "trino"))
	for p := range store.Files() {
		assert.False(t, strings.HasPrefix(p, "projects/demo/services/trino/"), "left behind: %s", p)
	}
	assert.Equal(t, "okdp: delete demo/trino by alice\n\nCo-Authored-By: Alice Martin <alice@example.com>", store.Messages[len(store.Messages)-1])
	assert.True(t, apierrors.IsNotFound(svc.DeleteService(aliceContext(), "demo", "trino")))
}

func TestWatchStreamsTheConsoleCommitsAndEngineChanges(t *testing.T) {
	events := make(chan watch.Event, 4)
	engine := stubEngine{statuses: map[string]repository.EngineStatus{}, events: events}
	svc, _ := newGitServiceUnderTest(t, engine, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := svc.WatchServices(ctx, "demo")
	require.NoError(t, err)

	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino"})
	require.NoError(t, err)
	first := next(t, stream)
	assert.Equal(t, "ADDED", first.Type)
	assert.Equal(t, "Pending", first.Object.Status)

	engine.statuses["demo-trino"] = repository.EngineStatus{Found: true, Phase: repository.PhaseInstalling}
	hr := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"targetNamespace": "demo"}}}
	hr.SetName("demo-trino")
	events <- watch.Event{Type: watch.Modified, Object: hr}
	second := next(t, stream)
	assert.Equal(t, "MODIFIED", second.Type)
	assert.Equal(t, "Installing", second.Object.Status)

	require.NoError(t, svc.DeleteService(aliceContext(), "demo", "trino"))
	third := next(t, stream)
	assert.Equal(t, "DELETED", third.Type)
	assert.Equal(t, "demo-trino", third.Object.ReleaseName)
}

func next(t *testing.T, stream <-chan ServiceEvent) ServiceEvent {
	t.Helper()
	select {
	case e := <-stream:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return ServiceEvent{}
	}
}

func TestReferencedConnectionsOnlyMatchesDeclaredOnes(t *testing.T) {
	values := map[string]any{
		"engine":   "warehouse",
		"catalogs": []any{map[string]any{"hive": "lake"}, "other"},
	}
	got := referencedConnections(values, map[string]bool{"lake": true, "warehouse": true, "unused": true})
	assert.Equal(t, []string{"lake", "warehouse"}, got)
}

func TestInstanceStatusVocabulary(t *testing.T) {
	inst := gitops.Instance{Version: "2.0.0"}
	status, msg := instanceStatus(inst, repository.EngineStatus{}, "argocd")
	assert.Equal(t, "Pending", status)
	assert.Contains(t, msg, "argocd")

	status, _ = instanceStatus(inst, repository.EngineStatus{Found: true, Phase: repository.PhaseReady, Revision: "1.0.0"}, "flux")
	assert.Equal(t, "Updating", status)

	status, msg = instanceStatus(inst, repository.EngineStatus{Found: true, Phase: repository.PhaseReady, Revision: "2.0.0", Message: "stale"}, "flux")
	assert.Equal(t, "Ready", status)
	assert.Empty(t, msg)
}

func TestMergePatchFollowsRFC7386(t *testing.T) {
	target := map[string]any{
		"workers": float64(2),
		"opa":     map[string]any{"enabled": true, "debug": true, "policy": map[string]any{"repo": "x"}},
		"list":    []any{"a", "b"},
	}
	got := MergePatch(target, map[string]any{
		"workers": nil,
		"opa":     map[string]any{"debug": nil, "policy": map[string]any{"branch": "main"}},
		"list":    []any{"c"},
		"new":     map[string]any{"k": nil, "v": "1"},
	})
	want := map[string]any{
		"opa":  map[string]any{"enabled": true, "policy": map[string]any{"repo": "x", "branch": "main"}},
		"list": []any{"c"},
		"new":  map[string]any{"v": "1"},
	}
	assert.Equal(t, want, got)
	assert.Equal(t, float64(2), target["workers"], "the target is not modified")
	assert.Equal(t, map[string]any{}, MergePatch(map[string]any{"a": float64(1)}, map[string]any{"a": nil}))
}

func TestPatchWithNullDeletesFromValuesYAML(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)
	_, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", Parameters: map[string]any{
		"workers":  float64(2),
		"catalogs": []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
	}})
	require.NoError(t, err)

	_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{
		Parameters: map[string]any{"catalogs": []any{map[string]any{"name": "c"}}},
	})
	require.NoError(t, err)
	values := store.Files()["projects/demo/services/trino/values.yaml"]
	assert.Contains(t, values, "name: c")
	assert.NotContains(t, values, "name: a", "arrays replace, they do not merge")

	_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{
		Parameters: map[string]any{"workers": nil, "catalogs": nil},
	})
	require.NoError(t, err)
	assert.Equal(t, "{}\n", store.Files()["projects/demo/services/trino/values.yaml"])

	// Validation runs on the merged result: removing a required key is refused.
	svc.schemaService = stubSchema{schema: map[string]any{
		"type": "object", "required": []any{"workers"},
		"properties": map[string]any{"workers": map[string]any{"type": "integer"}},
	}}
	_, err = svc.UpdateServiceParameters(aliceContext(), "demo", "trino", models.ServiceUpdateRequest{Parameters: map[string]any{"workers": nil}})
	assert.True(t, IsValidationError(err), "got %v", err)
}

// The chart's defaults apply at render time; values.yaml holds only what the
// user submitted, never the schema's defaults.
func TestDeployWritesOnlyTheSubmittedParameters(t *testing.T) {
	svc, store := newGitServiceUnderTest(t, stubEngine{}, nil)
	svc.schemaService = stubSchema{schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"workers": map[string]any{"type": "integer", "default": 3},
			"opa":     map[string]any{"type": "object", "default": map[string]any{"enabled": false}},
			"memory":  map[string]any{"type": "string", "default": "4Gi"},
		},
	}}
	_, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", InstanceName: "a", Parameters: map[string]any{"memory": "8Gi"}})
	require.NoError(t, err)
	assert.Equal(t, "memory: 8Gi\n", store.Files()["projects/demo/services/a/values.yaml"])

	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", InstanceName: "b"})
	require.NoError(t, err)
	assert.Equal(t, "{}\n", store.Files()["projects/demo/services/b/values.yaml"])
}

func TestReleaseNameCollisionIsNotADuplicateInstance(t *testing.T) {
	svc, _ := newGitServiceUnderTest(t, stubEngine{}, nil)
	_, err := svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", InstanceName: "sql-x"})
	require.NoError(t, err)

	_, err = svc.DeployService(aliceContext(), "demo-sql", models.ServiceRequest{Service: "trino", InstanceName: "x"})
	require.True(t, IsReleaseNameTaken(err), "got %v", err)
	assert.False(t, apierrors.IsAlreadyExists(err))
	assert.Equal(t, "release name 'demo-sql-x' is already used by instance 'sql-x' of project 'demo'", err.Error())

	_, err = svc.DeployService(aliceContext(), "demo", models.ServiceRequest{Service: "trino", InstanceName: "sql-x"})
	assert.True(t, apierrors.IsAlreadyExists(err), "got %v", err)
	assert.False(t, IsReleaseNameTaken(err))
}
