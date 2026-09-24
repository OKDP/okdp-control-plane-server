package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/okdp/okdp-control-plane-server/internal/api/handlers"
	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

// A router over an in-memory deployments repository holding project demo
// with one instance, served without authentication (AUTH_DISABLED).
func gitBackedRouter(t *testing.T) (*gin.Engine, *gitops.MemoryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := gitops.NewMemoryStore(map[string]string{
		"projects/demo/project.yaml":                    "name: demo\n",
		"projects/demo/kustomization.yaml":              "resources: []\n",
		"projects/demo/services/trino/instance.yaml":    "name: trino\nproject: demo\nservice: trino\nchart: oci://quay.io/okdp/platform-charts/trino\nversion: 1.0.0\nconnections: []\n",
		"projects/demo/services/trino/values.yaml":      "{}\n",
		"projects/demo/connections/lake.yaml":           "connections:\n  lake:\n    contract: s3\n",
		"projects/other/project.yaml":                   "name: other\n",
		"projects/other/services/spark/instance.yaml":   "name: spark\n",
		"projects/demo/services/trino/helmrelease.yaml": "kind: HelmRelease\n",
	})
	deployments := gitops.NewDeployments(store, nil)
	projects := handlers.NewProjectHandler(service.NewDefaultProjectService(nil, deployments))
	services := handlers.NewServiceHandler(service.NewDefaultServiceService(service.ServiceDeps{Deployments: deployments}), nil)
	cfg := &config.Config{AllowedOrigins: "https://console.example"}
	return SetupRouter(cfg, nil, nil, projects, nil, nil, nil, services, nil, nil, nil), store
}

// DELETE …/services/.. used to resolve to projects/demo and remove the whole
// project from Git; …/services/. removed every instance.
func TestDotSegmentServiceNamesAreRefusedBeforeTheStore(t *testing.T) {
	r, store := gitBackedRouter(t)
	before := store.Files()

	for _, target := range []string{
		"/api/projects/demo/services/..",
		"/api/projects/demo/services/.",
		"/api/projects/demo/services/%2e%2e",
		"/api/projects/demo/services/%2E",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, target, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("DELETE %s answered %d, wanted 400", target, w.Code)
		}
	}
	// An encoded slash splits the segment: no route matches at all.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/projects/demo/services/..%2fother", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("DELETE …/services/..%%2fother answered %d, wanted 404", w.Code)
	}

	after := store.Files()
	if len(after) != len(before) || len(store.Messages) != 0 {
		t.Fatalf("the repository changed: %d files -> %d, %d commits", len(before), len(after), len(store.Messages))
	}
	for p, content := range before {
		if after[p] != content {
			t.Errorf("%s changed", p)
		}
	}
}

func TestObjectNamesInThePathAreValidated(t *testing.T) {
	r := testRouterWithoutAuth()
	for _, call := range []struct{ method, target string }{
		{http.MethodDelete, "/api/projects/demo/connections/.."},
		{http.MethodPut, "/api/projects/demo/connections/Lake"},
		{http.MethodGet, "/api/projects/demo/connections/%2e%2e/consumers"},
		{http.MethodDelete, "/api/projects/demo/secret-stores/.."},
		{http.MethodGet, "/api/projects/demo/secret-stores/./status"},
		{http.MethodDelete, "/api/projects/demo/external-secrets/."},
		{http.MethodGet, "/api/projects/demo/spark-apps/.."},
		{http.MethodGet, "/api/projects/demo/services/trino/pods/../logs"},
		{http.MethodGet, "/api/projects/demo/services/../pods"},
		{http.MethodGet, "/api/platform-services/../schema"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(call.method, call.target, nil))
		// Nil handlers would panic into a 500: a 400 proves the request was
		// stopped before any of them, RequireProject included.
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s answered %d, wanted 400", call.method, call.target, w.Code)
		}
	}
}

func testRouterWithoutAuth() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return SetupRouter(&config.Config{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}
