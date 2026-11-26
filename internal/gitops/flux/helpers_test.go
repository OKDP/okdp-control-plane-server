package flux

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

const (
	testNS   = "okdp"
	repoName = "okdp-state"
	ksName   = "okdp-state"
	shaA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var testRepo = gitops.Repository{
	URL:    "https://git.example.com/platform/state.git",
	Branch: "main",
	Path:   "clusters/prod",
}

var t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{
		Namespace:          testNS,
		GitRepositoryName:  repoName,
		KustomizationName:  ksName,
		ExpectedRepository: testRepo,
		Now:                func() time.Time { return t0 },
	}
}

func newClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			GitRepositoryResource: "GitRepositoryList",
			KustomizationResource: "KustomizationList",
		}, objs...)
}

func newAdapter(t *testing.T, client *dynamicfake.FakeDynamicClient) *Adapter {
	t.Helper()
	a, err := New(client, testConfig())
	require.NoError(t, err)
	return a
}

func cond(typ, status, reason, message string, gen int64) map[string]any {
	return map[string]any{
		"type": typ, "status": status, "reason": reason, "message": message,
		"observedGeneration": gen, "lastTransitionTime": "2026-11-22T10:00:00Z",
	}
}

func readyCond(gen int64) map[string]any {
	return cond(readyCondition, "True", "Succeeded", "done", gen)
}

// gitRepository is a fetched, Ready GitRepository at generation 1.
func gitRepository(rev string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       gitRepositoryKind,
		"metadata":   map[string]any{"name": repoName, "namespace": testNS, "generation": int64(1)},
		"spec": map[string]any{
			"url":      "https://git.example.com/platform/state",
			"ref":      map[string]any{"branch": "main"},
			"interval": "10m",
		},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"artifact":           map[string]any{"revision": "main@sha1:" + rev},
			"conditions":         []any{readyCond(1)},
		},
	}}
}

// kustomization is an applied, Ready Kustomization at generation 1.
func kustomization(rev string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1",
		"kind":       kustomizationKind,
		"metadata":   map[string]any{"name": ksName, "namespace": testNS, "generation": int64(1)},
		"spec": map[string]any{
			"path":               "./clusters/prod",
			"sourceRef":          map[string]any{"kind": gitRepositoryKind, "name": repoName},
			"interval":           "10m",
			"prune":              true,
			"serviceAccountName": "okdp-gitops",
		},
		"status": map[string]any{
			"observedGeneration":    int64(1),
			"lastAppliedRevision":   "main@sha1:" + rev,
			"lastAttemptedRevision": "main@sha1:" + rev,
			"conditions":            []any{readyCond(1)},
		},
	}}
}

func set(t *testing.T, obj *unstructured.Unstructured, value any, fields ...string) *unstructured.Unstructured {
	t.Helper()
	require.NoError(t, unstructured.SetNestedField(obj.Object, value, fields...))
	return obj
}

func setConditions(t *testing.T, obj *unstructured.Unstructured, conds ...map[string]any) *unstructured.Unstructured {
	t.Helper()
	list := make([]any, len(conds))
	for i, c := range conds {
		list[i] = c
	}
	return set(t, obj, list, "status", "conditions")
}
