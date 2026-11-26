package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/okdp/okdp-control-plane-server/internal/engine/kubocd"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// These tests drive the services and catalog routes end to end, from the HTTP
// request to the objects in a fake cluster, and compare whole response bodies.
// They pin what the console receives, whatever sits between the handler and
// the cluster.

var (
	podGVR     = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	pvcGVR     = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	eventGVR   = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	ingressGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
)

type catalogContextStub struct {
	repository.ContextRepository
	mu          sync.Mutex
	services    []models.PlatformService
	invalidated int
}

func (s *catalogContextStub) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

func (s *catalogContextStub) GetPlatformServices(context.Context) ([]models.PlatformService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.PlatformService, len(s.services))
	copy(out, s.services)
	return out, nil
}

func (s *catalogContextStub) GetMenuCategories(context.Context) ([]models.MenuCategory, error) {
	return []models.MenuCategory{{Key: "Notebooks", Label: "Notebooks", Order: 1}}, nil
}

func (s *catalogContextStub) GetPackageRepository(context.Context) (string, error) {
	return "quay.io/okdp/packages", nil
}

func (s *catalogContextStub) GetIngressSuffix(context.Context) (string, error) {
	return "okdp.local", nil
}

func (s *catalogContextStub) GetProfileImages(context.Context) (map[string][]models.ProfileImage, error) {
	return map[string][]models.ProfileImage{"python": {{Label: "Python 3", Image: "jupyter/python:3"}}}, nil
}

type catalogWriterStub struct {
	ctx    *catalogContextStub
	writes []string
}

func (w *catalogWriterStub) AddPlatformService(_ context.Context, svc models.PlatformService) error {
	w.writes = append(w.writes, "add:"+svc.Name)
	w.ctx.mu.Lock()
	defer w.ctx.mu.Unlock()
	w.ctx.services = append(w.ctx.services, svc)
	return nil
}

func (w *catalogWriterStub) UpdatePlatformService(_ context.Context, name string, svc models.PlatformService) error {
	w.writes = append(w.writes, "update:"+name)
	return nil
}

func (w *catalogWriterStub) RemovePlatformService(_ context.Context, name string) error {
	w.writes = append(w.writes, "remove:"+name)
	return nil
}

// schemaStub answers like a package whose only parameter is an integer "replicas".
type schemaStub struct{}

func (schemaStub) GetParameterSchema(context.Context, string, string) (map[string]any, error) {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"replicas": map[string]any{"type": "integer"},
			"profile":  map[string]any{"type": "string"},
		},
	}, nil
}

func (schemaStub) GetPackageInputs(context.Context, string, string) ([]models.PackageInput, error) {
	return []models.PackageInput{{Alias: "db", Contract: "database-server"}}, nil
}

func (schemaStub) GetServiceVersions(_ context.Context, name string) (*models.ServiceVersions, error) {
	return &models.ServiceVersions{Versions: []string{"1.0.0", "1.1.0"}, Default: "1.0.0"}, nil
}

func (schemaStub) ListVersionsForServices(_ context.Context, services []models.PlatformService) map[string][]string {
	out := map[string][]string{}
	for _, svc := range services {
		out[svc.Name] = []string{"1.0.0", "1.1.0", "2.0.0"}
	}
	return out
}

func (schemaStub) ListPackageTags(context.Context, string, string) ([]string, error) {
	return []string{"1.0.0", "1.1.0"}, nil
}

type provisionerSpy struct{ deleted []string }

func (p *provisionerSpy) EnsureClient(context.Context, provisioning.OidcClientSpec) error { return nil }

func (p *provisionerSpy) DeleteClient(_ context.Context, name string) error {
	p.deleted = append(p.deleted, name)
	return nil
}

type servicesAPI struct {
	router      *gin.Engine
	cluster     *dynamicfake.FakeDynamicClient
	contextRepo *catalogContextStub
	writer      *catalogWriterStub
	provisioner *provisionerSpy
}

// createdAt is the creation time of readyRelease as the API renders it: in the
// server's local zone.
var createdAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC).Local().Format(time.RFC3339)

func newServicesAPI(t *testing.T, objects ...runtime.Object) *servicesAPI {
	t.Helper()
	return newServicesAPIWith(t, schemaStub{}, objects...)
}

func newServicesAPIWith(t *testing.T, packages kubocd.PackageSource, objects ...runtime.Object) *servicesAPI {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cluster := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crd.GetReleaseGVR():           "ReleaseList",
		crd.GetHelmReleaseGVR():       "HelmReleaseList",
		podGVR:                        "PodList",
		pvcGVR:                        "PersistentVolumeClaimList",
		eventGVR:                      "EventList",
		ingressGVR:                    "IngressList",
		crd.GetConnectionGVR():        "ConnectionList",
		crd.GetClusterConnectionGVR(): "ClusterConnectionList",
	}, objects...)

	contextRepo := &catalogContextStub{services: []models.PlatformService{
		{Name: "jupyterhub", Versions: []string{"1.0.0"}, DefaultVersion: "1.0.0", Category: "Notebooks"},
	}}
	writer := &catalogWriterStub{ctx: contextRepo}
	provisioner := &provisionerSpy{}

	eng := kubocd.New(kubocd.Deps{
		Releases:      repository.NewServiceRepository(cluster),
		Context:       contextRepo,
		ContextWriter: writer,
		Packages:      packages,
		Cluster:       cluster,
		Config:        kubocd.Config{ReleaseInterval: "30m", ReleaseTimeout: "10m"},
	})
	svc := service.NewDefaultServiceService(eng, provisioner, cluster, k8sfake.NewClientset(), []string{"istio-proxy"})
	h := NewServiceHandler(svc, eng.Catalog)

	r := gin.New()
	api := r.Group("/api")
	api.GET("/platform-services", h.GetPlatformServices)
	api.GET("/platform-categories", h.GetMenuCategories)
	api.POST("/platform-services", h.CreatePlatformService)
	api.PUT("/platform-services/:serviceName", h.UpdatePlatformService)
	api.DELETE("/platform-services/:serviceName", h.DeletePlatformService)
	api.GET("/platform-services/:serviceName/versions", h.GetServiceVersions)
	api.GET("/platform-services/:serviceName/schema", h.GetServiceSchema)
	api.GET("/platform-services/:serviceName/inputs", h.GetServiceInputs)
	api.GET("/profile-images", h.GetProfileImages)
	services := api.Group("/projects/:name/services")
	services.GET("", h.ListServices)
	services.GET("/stream", h.StreamServices)
	services.POST("", h.DeployService)
	services.GET("/:serviceName", h.GetService)
	services.PATCH("/:serviceName/parameters", h.UpdateServiceParameters)
	services.DELETE("/:serviceName", h.DeleteService)

	return &servicesAPI{router: r, cluster: cluster, contextRepo: contextRepo, writer: writer, provisioner: provisioner}
}

func (a *servicesAPI) do(method, path, body string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

func (a *servicesAPI) release(t *testing.T, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := a.cluster.Resource(crd.GetReleaseGVR()).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return u
}

func object(gvr schema.GroupVersionResource, kind, namespace, name string, fields map[string]any) *unstructured.Unstructured {
	apiVersion := gvr.Version
	if gvr.Group != "" {
		apiVersion = gvr.Group + "/" + gvr.Version
	}
	obj := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace},
	}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}

// readyRelease is a Release the controller has reconciled: one HelmRelease
// owned by it, a healthy pod, an Ingress routing to it, one resolved
// connection and one still waited for.
func readyRelease() []runtime.Object {
	release := object(crd.GetReleaseGVR(), crd.ReleaseKind, "demo", "demo-jh", map[string]any{
		"spec": map[string]any{
			"package":         map[string]any{"repository": "quay.io/okdp/packages/jupyterhub", "tag": "1.0.0"},
			"parameters":      map[string]any{"replicas": int64(1), "profile": "small"},
			"targetNamespace": "demo",
		},
		"status": map[string]any{
			"phase": "READY",
			"roles": []any{"notebook"},
			"effectiveInputConnections": []any{
				map[string]any{"kind": "Connection", "name": "pg", "namespace": "demo"},
			},
			"watchedInputConnections": []any{
				map[string]any{"kind": "Connection", "name": "pg", "namespace": "demo"},
				map[string]any{"kind": "ClusterConnection", "name": "pg", "namespace": ""},
				map[string]any{"kind": "Connection", "name": "s3", "namespace": "demo"},
			},
		},
	})
	release.SetUID("uid-jh")
	release.SetGeneration(3)
	release.SetLabels(map[string]string{crd.LabelProject: "demo", crd.LabelService: "jupyterhub", crd.LabelInstanceName: "jh"})
	release.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)))

	helmRelease := object(crd.GetHelmReleaseGVR(), "HelmRelease", "demo", "demo-jh-main", nil)
	helmRelease.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: crd.ReleaseAPIVersion, Kind: crd.ReleaseKind, Name: "demo-jh", UID: "uid-jh"}})

	pod := object(podGVR, "Pod", "demo", "demo-jh-main-0", map[string]any{
		"status": map[string]any{"containerStatuses": []any{map[string]any{"name": "hub", "ready": true}}},
	})
	pod.SetLabels(map[string]string{"app.kubernetes.io/instance": "demo-jh-main"})

	ingress := object(ingressGVR, "Ingress", "demo", "demo-jh-main", map[string]any{
		"spec": map[string]any{"rules": []any{map[string]any{"host": "jupyterhub-demo.okdp.local"}}},
	})
	ingress.SetLabels(map[string]string{"helm.toolkit.fluxcd.io/name": "demo-jh-main"})

	return []runtime.Object{release, helmRelease, pod, ingress}
}

func TestDeployServiceAnswersWithTheReleaseItCreated(t *testing.T) {
	api := newServicesAPI(t)

	rec := api.do(http.MethodPost, "/api/projects/demo/services", `{"service":"jupyterhub","instanceName":"jh","parameters":{"replicas":2}}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{
		"phase":"Pending","desiredRevision":"","appliedRevision":"",
		"name":"jh","releaseName":"demo-jh","service":"jupyterhub","serviceTag":"1.0.0",
		"status":"Installing","targetNamespace":"demo","url":"https://demo-jh.okdp.local",
		"parameters":{"replicas":2},
		"operation":{"outcome":"accepted","desiredRevision":""}
	}`, rec.Body.String())

	created := api.release(t, "demo", "demo-jh")
	assert.Equal(t, map[string]string{crd.LabelProject: "demo", crd.LabelService: "jupyterhub", crd.LabelInstanceName: "jh"}, created.GetLabels())
	spec, _, _ := unstructured.NestedMap(created.Object, "spec")
	assert.Equal(t, map[string]any{
		"description":     "jupyterhub for project demo",
		"package":         map[string]any{"repository": "quay.io/okdp/packages/jupyterhub", "tag": "1.0.0", "interval": "30m", "timeout": "10m"},
		"parameters":      map[string]any{"replicas": float64(2)},
		"targetNamespace": "demo",
		"createNamespace": false,
	}, spec)
}

func TestDeployServiceDefaultsTheInstanceNameToTheService(t *testing.T) {
	api := newServicesAPI(t)

	rec := api.do(http.MethodPost, "/api/projects/demo/services", `{"service":"jupyterhub","tag":"1.1.0","parameters":{}}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{
		"phase":"Pending","desiredRevision":"","appliedRevision":"",
		"name":"jupyterhub","releaseName":"demo-jupyterhub","service":"jupyterhub","serviceTag":"1.1.0",
		"status":"Installing","targetNamespace":"demo","url":"https://demo-jupyterhub.okdp.local",
		"operation":{"outcome":"accepted","desiredRevision":""}
	}`, rec.Body.String())
}

func TestDeployServiceErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want string
	}{
		{"duplicate", `{"service":"jupyterhub","instanceName":"jh","parameters":{}}`, http.StatusConflict,
			`{"error":"Instance 'jh' already exists in project 'demo'","code":"instance-exists",
			"operation":{"outcome":"rejected","desiredRevision":"","problems":[
				{"code":"instance-exists","message":"releases.kubocd.kubotal.io \"demo-jh\" already exists"}]}}`},
		{"not in the catalog", `{"service":"nope"}`, http.StatusInternalServerError,
			`{"error":"service \"nope\" is not available in the platform"}`},
		{"missing service", `{}`, http.StatusBadRequest,
			`{"error":"Key: 'ServiceRequest.Service' Error:Field validation for 'Service' failed on the 'required' tag","code":"invalid-request"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newServicesAPI(t, readyRelease()...)
			rec := api.do(http.MethodPost, "/api/projects/demo/services", tt.body)
			assert.Equal(t, tt.code, rec.Code)
			assert.JSONEq(t, tt.want, rec.Body.String())
		})
	}
}

// Whatever the engine makes of a failed write, the console reads the message
// the API server gave.
func TestFailedWritesKeepTheAPIServerMessage(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "kubocd.kubotal.io", Kind: "Release"}, "demo-jh", nil)
	timeout := apierrors.NewServerTimeout(crd.GetReleaseGVR().GroupResource(), "update", 1)
	forbidden := apierrors.NewForbidden(crd.GetReleaseGVR().GroupResource(), "demo-jh", errors.New("denied"))
	missing := apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "demo")
	tests := []struct {
		name, verb          string
		err                 error
		method, path, in    string
		want, code, outcome string
		status              int
	}{
		{"create refused", "create", invalid, http.MethodPost, "/api/projects/demo/services",
			`{"service":"jupyterhub","instanceName":"jh","parameters":{}}`, invalid.Error(), "rejected", "rejected", http.StatusInternalServerError},
		{"update timed out", "update", timeout, http.MethodPatch, "/api/projects/demo/services/jh/parameters",
			`{"tag":"1.1.0"}`, "failed to update release: " + timeout.Error(), "outcome-unknown", "unknown", http.StatusGatewayTimeout},
		{"delete forbidden", "delete", forbidden, http.MethodDelete, "/api/projects/demo/services/jh", "", forbidden.Error(), "rejected", "rejected", http.StatusInternalServerError},
		{"create in a missing namespace", "create", missing, http.MethodPost, "/api/projects/demo/services",
			`{"service":"jupyterhub","instanceName":"jh","parameters":{}}`, missing.Error(), "", "", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := readyRelease()
			if tt.verb == "create" {
				objects = objects[1:]
			}
			api := newServicesAPI(t, objects...)
			api.cluster.PrependReactor(tt.verb, "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.err
			})

			rec := api.do(tt.method, tt.path, tt.in)

			assert.Equal(t, tt.status, rec.Code)
			var body models.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tt.want, body.Error)
			assert.Equal(t, tt.code, body.Code)
			if tt.outcome == "" {
				assert.Nil(t, body.Operation)
			} else {
				require.NotNil(t, body.Operation)
				assert.Equal(t, tt.outcome, body.Operation.Outcome)
			}
		})
	}
}

func TestDeployServiceRejectsParametersTheSchemaRefuses(t *testing.T) {
	api := newServicesAPI(t)

	rec := api.do(http.MethodPost, "/api/projects/demo/services", `{"service":"jupyterhub","parameters":{"replicas":"two"}}`)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var body models.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, strings.HasPrefix(body.Error, "parameter validation failed: invalid parameters: "), body.Error)
	assertSchemaRejection(t, body)
	_, err := api.cluster.Resource(crd.GetReleaseGVR()).Namespace("demo").Get(context.Background(), "demo-jupyterhub", metav1.GetOptions{})
	assert.Error(t, err, "nothing is created")
}

// assertSchemaRejection checks the body of a write the schema refused: a
// rejected operation, without a revision, whose problem is the validation.
func assertSchemaRejection(t *testing.T, body models.ErrorResponse) {
	t.Helper()
	assert.Equal(t, "invalid-parameters", body.Code)
	require.NotNil(t, body.Operation)
	assert.Equal(t, "rejected", body.Operation.Outcome)
	assert.Empty(t, body.Operation.DesiredRevision)
	require.Len(t, body.Operation.Problems, 1)
	assert.Equal(t, "invalid-parameters", body.Operation.Problems[0].Code)
	assert.True(t, strings.HasPrefix(body.Operation.Problems[0].Message, "invalid parameters: "), body.Operation.Problems[0].Message)
}

// schemaUnreachable is a registry that cannot serve the schema.
type schemaUnreachable struct{ schemaStub }

func (schemaUnreachable) GetParameterSchema(context.Context, string, string) (map[string]any, error) {
	return nil, errors.New("registry unreachable")
}

// A schema that cannot be fetched says nothing about the parameters: the
// failure is the server's, not a refusal of the write.
func TestAnUnreachableSchemaIsNotARejection(t *testing.T) {
	for _, call := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/projects/demo/services", `{"service":"jupyterhub","parameters":{}}`},
		{http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`},
	} {
		api := newServicesAPIWith(t, schemaUnreachable{}, readyRelease()...)

		rec := api.do(call.method, call.path, call.body)

		assert.Equal(t, http.StatusInternalServerError, rec.Code, call.method)
		var body models.ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.True(t, strings.HasPrefix(body.Error, "parameter validation failed: could not fetch schema for jupyterhub@"), body.Error)
		assert.True(t, strings.HasSuffix(body.Error, ": registry unreachable"), body.Error)
		assert.Empty(t, body.Code, call.method)
		assert.Nil(t, body.Operation, call.method)
	}
}

func TestGetServiceReadsTheReconciledRelease(t *testing.T) {
	api := newServicesAPI(t, readyRelease()...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{
		"phase":"Ready","desiredRevision":"3","appliedRevision":"",
		"name":"jh","releaseName":"demo-jh","service":"jupyterhub","serviceTag":"1.0.0",
		"status":"Ready","targetNamespace":"demo","url":"https://jupyterhub-demo.okdp.local",
		"roles":["notebook"],"parameters":{"replicas":1,"profile":"small"},
		"connections":[
			{"name":"pg","namespace":"demo","kind":"Connection","resolved":true,"state":"bound"},
			{"name":"s3","namespace":"demo","kind":"Connection","resolved":false}
		],
		"createdAt":"`+createdAt+`"
	}`, rec.Body.String())
}

func TestConnectionStatesFollowTheConnectionsAndTheirProviders(t *testing.T) {
	provider := object(crd.GetReleaseGVR(), crd.ReleaseKind, "demo", "demo-pgsql", map[string]any{
		"status": map[string]any{"phase": "READY"},
	})
	provider.SetLabels(map[string]string{crd.LabelProject: "demo"})
	published := object(crd.GetConnectionGVR(), crd.ConnectionKind, "demo", "pg", map[string]any{
		"spec":   map[string]any{"contract": "database-server", "outputName": "main"},
		"status": map[string]any{"parent": "demo-pgsql"},
	})
	declared := object(crd.GetConnectionGVR(), crd.ConnectionKind, "demo", "s3", map[string]any{
		"spec": map[string]any{"contract": "s3"},
	})
	api := newServicesAPI(t, append(readyRelease(), provider, published, declared)...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body models.ServiceInstance
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Connections, 2)
	assert.Equal(t, "ready", body.Connections[0].State, "bound, and its provider is ready")
	assert.True(t, body.Connections[0].Resolved)
	assert.Equal(t, "declared", body.Connections[1].State, "exists, declared by a user, not bound yet")
	assert.False(t, body.Connections[1].Resolved)
}

func TestConnectionsAreReadOncePerList(t *testing.T) {
	second := readyRelease()[0].(*unstructured.Unstructured).DeepCopy()
	second.SetName("demo-jh2")
	second.SetUID("uid-jh2")
	second.SetLabels(map[string]string{crd.LabelProject: "demo", crd.LabelService: "jupyterhub", crd.LabelInstanceName: "jh2"})
	api := newServicesAPI(t, append(readyRelease(), second)...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services", "")

	require.Equal(t, http.StatusOK, rec.Code)
	lists := map[string]int{}
	for _, action := range api.cluster.Actions() {
		if action.GetVerb() == "list" {
			lists[action.GetResource().Resource]++
		}
	}
	assert.Equal(t, 1, lists["connections"])
	assert.Equal(t, 1, lists["clusterconnections"])
	assert.Equal(t, 1, lists["releases"], "the providers' phases come from the list already read")
}

func TestGetServiceExplainsAFailedRelease(t *testing.T) {
	objects := readyRelease()
	release := objects[0].(*unstructured.Unstructured)
	require.NoError(t, unstructured.SetNestedField(release.Object, "ERROR", "status", "phase"))
	warning := object(eventGVR, "Event", "demo", "demo-jh-main.1", map[string]any{
		"type":           "Warning",
		"message":        "Helm upgrade failed: timed out",
		"involvedObject": map[string]any{"name": "demo-jh-main"},
		"lastTimestamp":  "2026-10-01T09:00:00Z",
	})
	api := newServicesAPI(t, append(objects, warning)...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "Error", body["status"])
	assert.Equal(t, "Helm upgrade failed: timed out", body["statusMessage"])
}

func TestGetServiceReportsCrashingPods(t *testing.T) {
	objects := readyRelease()
	pod := objects[2].(*unstructured.Unstructured)
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{map[string]any{
		"name": "hub", "ready": false, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}},
	}}, "status", "containerStatuses"))
	api := newServicesAPI(t, objects...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "Error", body["status"])
	assert.Equal(t, "Degraded", body["phase"], "the engine says Ready, the pods say otherwise")
}

func TestAReleaseBeingDeletedKeepsItsStatus(t *testing.T) {
	objects := readyRelease()
	release := objects[0].(*unstructured.Unstructured)
	now := metav1.Now()
	release.SetDeletionTimestamp(&now)
	release.SetFinalizers([]string{"kubocd.kubotal.io/finalizer"})
	api := newServicesAPI(t, objects...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "Ready", body["status"])
	assert.Equal(t, "Deleting", body["phase"])
}

func TestMissingServiceAnswers404(t *testing.T) {
	api := newServicesAPI(t)
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/projects/demo/services/ghost", ""},
		{http.MethodPatch, "/api/projects/demo/services/ghost/parameters", `{"tag":"1.1.0"}`},
		{http.MethodDelete, "/api/projects/demo/services/ghost", ""},
	} {
		rec := api.do(call.method, call.path, call.body)
		assert.Equal(t, http.StatusNotFound, rec.Code, call.method)
		assert.JSONEq(t, `{"error":"Service 'ghost' not found in project 'demo'","code":"not-found"}`, rec.Body.String(), call.method)
	}
}

func TestListServices(t *testing.T) {
	api := newServicesAPI(t, readyRelease()...)

	rec := api.do(http.MethodGet, "/api/projects/demo/services", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{
		"phase":"Ready","desiredRevision":"3","appliedRevision":"",
		"name":"jh","releaseName":"demo-jh","service":"jupyterhub","serviceTag":"1.0.0",
		"status":"Ready","targetNamespace":"demo","url":"https://jupyterhub-demo.okdp.local",
		"roles":["notebook"],"parameters":{"replicas":1,"profile":"small"},
		"connections":[
			{"name":"pg","namespace":"demo","kind":"Connection","resolved":true,"state":"bound"},
			{"name":"s3","namespace":"demo","kind":"Connection","resolved":false}
		],
		"createdAt":"`+createdAt+`"
	}]`, rec.Body.String())
}

func TestListServicesOfAnEmptyProjectIsNull(t *testing.T) {
	api := newServicesAPI(t, readyRelease()...)

	rec := api.do(http.MethodGet, "/api/projects/other/services", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "null", rec.Body.String())
}

func TestUpdateServiceParametersMergesTopLevelKeys(t *testing.T) {
	api := newServicesAPI(t, readyRelease()...)

	rec := api.do(http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0","parameters":{"replicas":3}}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{
		"phase":"Ready","desiredRevision":"3","appliedRevision":"",
		"name":"jh","releaseName":"demo-jh","service":"jupyterhub","serviceTag":"1.1.0",
		"status":"Ready","targetNamespace":"demo","url":"https://jupyterhub-demo.okdp.local",
		"roles":["notebook"],"parameters":{"replicas":3,"profile":"small"},
		"connections":[
			{"name":"pg","namespace":"demo","kind":"Connection","resolved":true,"state":"bound"},
			{"name":"s3","namespace":"demo","kind":"Connection","resolved":false}
		],
		"createdAt":"`+createdAt+`",
		"operation":{"outcome":"accepted","desiredRevision":"3"}
	}`, rec.Body.String())

	stored := api.release(t, "demo", "demo-jh")
	tag, _, _ := unstructured.NestedString(stored.Object, "spec", "package", "tag")
	params, _, _ := unstructured.NestedMap(stored.Object, "spec", "parameters")
	assert.Equal(t, "1.1.0", tag)
	assert.Equal(t, map[string]any{"replicas": float64(3), "profile": "small"}, params)
}

func TestUpdateServiceParametersValidatesTheMergedResult(t *testing.T) {
	api := newServicesAPI(t, readyRelease()...)

	rec := api.do(http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"parameters":{"profile":7}}`)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var body models.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.True(t, strings.HasPrefix(body.Error, "parameter validation failed: invalid parameters: "), body.Error)
	assertSchemaRejection(t, body)
	params, _, _ := unstructured.NestedMap(api.release(t, "demo", "demo-jh").Object, "spec", "parameters")
	assert.Equal(t, "small", params["profile"], "the release is left untouched")
}

func (a *servicesAPI) doIfMatch(method, path, body, ifMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", ifMatch)
	rec := httptest.NewRecorder()
	a.router.ServeHTTP(rec, req)
	return rec
}

func TestIfMatchGuardsUpdatesAndDeletions(t *testing.T) {
	tests := []struct {
		name, method, path, body, ifMatch string
		code                              int
		errorCode                         string
	}{
		{"update at the current revision", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `"3"`, http.StatusOK, ""},
		{"update with any revision", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `*`, http.StatusOK, ""},
		{"update at a stale revision", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `"2"`, http.StatusConflict, "conflict"},
		{"update with a weak revision", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `W/"3"`, http.StatusBadRequest, "invalid-precondition"},
		{"update with two revisions", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `"3", "4"`, http.StatusBadRequest, "invalid-precondition"},
		{"update with an empty revision", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `""`, http.StatusBadRequest, "invalid-precondition"},
		{"update with an opening quote only", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `"3`, http.StatusBadRequest, "invalid-precondition"},
		{"update with a closing quote only", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `3"`, http.StatusBadRequest, "invalid-precondition"},
		{"update with doubled quotes", http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `""3""`, http.StatusBadRequest, "invalid-precondition"},
		{"delete at the current revision", http.MethodDelete, "/api/projects/demo/services/jh", "", `3`, http.StatusNoContent, ""},
		{"delete at a stale revision", http.MethodDelete, "/api/projects/demo/services/jh", "", `"2"`, http.StatusConflict, "conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userVolume := object(pvcGVR, "PersistentVolumeClaim", "demo", "demo-jh-claim-alice", nil)
			api := newServicesAPI(t, append(readyRelease(), userVolume)...)

			rec := api.doIfMatch(tt.method, tt.path, tt.body, tt.ifMatch)

			require.Equal(t, tt.code, rec.Code, rec.Body.String())
			if tt.errorCode == "" {
				return
			}
			var body models.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tt.errorCode, body.Code)
			stored := api.release(t, "demo", "demo-jh")
			tag, _, _ := unstructured.NestedString(stored.Object, "spec", "package", "tag")
			assert.Equal(t, "1.0.0", tag, "nothing was written")
			assert.Empty(t, api.provisioner.deleted, "nothing was cleaned up")
			pvcs, err := api.cluster.Resource(pvcGVR).Namespace("demo").List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			assert.Len(t, pvcs.Items, 1)
		})
	}
}

// The instance changes after the If-Match check: between the core's read and
// the engine's, or between the engine's read and its write.
func TestIfMatchHoldsAgainstAConcurrentChange(t *testing.T) {
	for _, race := range []string{"between the reads", "between read and write"} {
		t.Run(race, func(t *testing.T) {
			api := newServicesAPI(t, readyRelease()...)
			gets := 0
			api.cluster.PrependReactor("get", "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
				gets++
				if race != "between the reads" || gets < 2 {
					return false, nil, nil
				}
				stored, err := api.cluster.Tracker().Get(crd.GetReleaseGVR(), "demo", "demo-jh")
				require.NoError(t, err)
				newer := stored.(*unstructured.Unstructured).DeepCopy()
				newer.SetGeneration(4)
				return true, newer, nil
			})
			api.cluster.PrependReactor("update", "releases", func(k8stesting.Action) (bool, runtime.Object, error) {
				if race == "between read and write" {
					return true, nil, apierrors.NewConflict(crd.GetReleaseGVR().GroupResource(), "demo-jh", errors.New("modified"))
				}
				return false, nil, nil
			})

			rec := api.doIfMatch(http.MethodPatch, "/api/projects/demo/services/jh/parameters", `{"tag":"1.1.0"}`, `"3"`)

			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			var body models.ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "conflict", body.Code)
			for _, action := range api.cluster.Actions() {
				if race == "between the reads" {
					assert.NotEqual(t, "update", action.GetVerb(), "nothing is written")
				}
			}
		})
	}
}

func TestDeleteServiceCleansUpWhatTheReleaseLeftBehind(t *testing.T) {
	neighbour := object(crd.GetReleaseGVR(), crd.ReleaseKind, "demo", "demo-jh-ds", nil)
	neighbour.SetLabels(map[string]string{crd.LabelProject: "demo"})
	userPod := object(podGVR, "Pod", "demo", "demo-jh-jupyter-alice", nil)
	neighbourPod := object(podGVR, "Pod", "demo", "demo-jh-ds-jupyter-bob", nil)
	userVolume := object(pvcGVR, "PersistentVolumeClaim", "demo", "demo-jh-claim-alice", nil)
	api := newServicesAPI(t, append(readyRelease(), neighbour, userPod, neighbourPod, userVolume)...)

	rec := api.do(http.MethodDelete, "/api/projects/demo/services/jh", "")

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"demo-jh"}, api.provisioner.deleted)
	ctx := context.Background()
	_, err := api.cluster.Resource(crd.GetReleaseGVR()).Namespace("demo").Get(ctx, "demo-jh", metav1.GetOptions{})
	assert.Error(t, err)
	pods, err := api.cluster.Resource(podGVR).Namespace("demo").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	var left []string
	for _, p := range pods.Items {
		left = append(left, p.GetName())
	}
	// The release's own pod goes too: it carries the release name as a prefix.
	assert.ElementsMatch(t, []string{"demo-jh-ds-jupyter-bob"}, left)
	pvcs, err := api.cluster.Resource(pvcGVR).Namespace("demo").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pvcs.Items)
}

func TestStreamServicesSendsEachReleaseChange(t *testing.T) {
	objects := readyRelease()
	api := newServicesAPI(t, objects[1:]...)
	server := httptest.NewServer(api.router)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/projects/demo/services/stream", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	created := objects[0].(*unstructured.Unstructured)
	go func() {
		// The watch is opened by the handler before the first flush, so a short
		// pause is enough for the event to reach it.
		time.Sleep(200 * time.Millisecond)
		_, _ = api.cluster.Resource(crd.GetReleaseGVR()).Namespace("demo").Create(context.Background(), created, metav1.CreateOptions{})
	}()

	reader := bufio.NewReader(resp.Body)
	var data string
	for data == "" {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "data:") {
			data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	assert.JSONEq(t, `{"type":"ADDED","object":{
		"phase":"Ready","desiredRevision":"3","appliedRevision":"",
		"name":"jh","releaseName":"demo-jh","service":"jupyterhub","serviceTag":"1.0.0",
		"status":"Ready","targetNamespace":"demo","url":"https://jupyterhub-demo.okdp.local",
		"roles":["notebook"],"parameters":{"replicas":1,"profile":"small"},
		"createdAt":"`+createdAt+`"
	}}`, data)
}

func TestCatalogRoutes(t *testing.T) {
	api := newServicesAPI(t)

	rec := api.do(http.MethodGet, "/api/platform-services", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"name":"jupyterhub","versions":["1.0.0","1.1.0","2.0.0"],"defaultVersion":"1.0.0","description":"","category":"Notebooks"}]`, rec.Body.String())

	rec = api.do(http.MethodGet, "/api/platform-categories", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"key":"Notebooks","label":"Notebooks","order":1}]`, rec.Body.String())

	rec = api.do(http.MethodGet, "/api/platform-services/jupyterhub/versions", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"versions":["1.0.0","1.1.0"],"default":"1.0.0"}`, rec.Body.String())

	rec = api.do(http.MethodGet, "/api/platform-services/jupyterhub/schema?tag=1.0.0", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"type":"object","properties":{"replicas":{"type":"integer"},"profile":{"type":"string"}}}`, rec.Body.String())

	rec = api.do(http.MethodGet, "/api/platform-services/jupyterhub/inputs", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"alias":"db","contract":"database-server","optional":false}]`, rec.Body.String())

	rec = api.do(http.MethodGet, "/api/profile-images", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"python":[{"label":"Python 3","image":"jupyter/python:3"}]}`, rec.Body.String())
}

func TestCatalogWrites(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		code                     int
		want                     string
		writes                   []string
	}{
		{"add", http.MethodPost, "/api/platform-services",
			`{"name":"trino","category":"SQL","versions":["1.0.0"]}`, http.StatusCreated,
			`{"name":"trino","category":"SQL","versions":["1.0.0"],"defaultVersion":"1.0.0","description":""}`, []string{"add:trino"}},
		{"add an existing one", http.MethodPost, "/api/platform-services",
			`{"name":"jupyterhub","category":"Notebooks","versions":["1.0.0"]}`, http.StatusConflict,
			`{"error":"service already exists in the catalog: \"jupyterhub\""}`, nil},
		{"add with an unpublished version", http.MethodPost, "/api/platform-services",
			`{"name":"trino","category":"SQL","versions":["9.9.9"]}`, http.StatusBadRequest,
			`{"error":"invalid catalog service: version(s) [9.9.9] not found in the registry for \"trino\" (available: [1.0.0 1.1.0])"}`, nil},
		{"add without a category", http.MethodPost, "/api/platform-services",
			`{"name":"trino","versions":["1.0.0"]}`, http.StatusBadRequest,
			`{"error":"invalid catalog service: category is required, it names the console section the service belongs to"}`, nil},
		{"update", http.MethodPut, "/api/platform-services/jupyterhub",
			`{"category":"Notebooks","versions":["1.1.0"],"description":"hub"}`, http.StatusOK,
			`{"name":"jupyterhub","category":"Notebooks","versions":["1.1.0"],"defaultVersion":"1.1.0","description":"hub"}`, []string{"update:jupyterhub"}},
		{"update a missing one", http.MethodPut, "/api/platform-services/ghost",
			`{"category":"X","versions":["1.0.0"]}`, http.StatusNotFound,
			`{"error":"service not found in the catalog: \"ghost\""}`, nil},
		{"remove", http.MethodDelete, "/api/platform-services/jupyterhub", "", http.StatusNoContent, "", []string{"remove:jupyterhub"}},
		{"remove a missing one", http.MethodDelete, "/api/platform-services/ghost", "", http.StatusNotFound,
			`{"error":"service not found in the catalog: \"ghost\""}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newServicesAPI(t)
			rec := api.do(tt.method, tt.path, tt.body)
			require.Equal(t, tt.code, rec.Code, rec.Body.String())
			if tt.want != "" {
				assert.JSONEq(t, tt.want, rec.Body.String())
			}
			assert.Equal(t, tt.writes, api.writer.writes)
			if tt.writes != nil {
				assert.Equal(t, 1, api.contextRepo.invalidated, "the catalog cache is dropped after a write")
			}
		})
	}
}
