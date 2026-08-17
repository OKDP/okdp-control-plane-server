package repository

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newContextWithOKDP builds a fake dynamic client holding a single KuboCD
// Context whose spec.context.okdp carries the given categories and services.
func newContextWithOKDP(t *testing.T, okdp map[string]interface{}) ContextRepository {
	t.Helper()
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubocd.kubotal.io/v1alpha1",
			"kind":       "Context",
			"metadata": map[string]interface{}{
				"name":      "default",
				"namespace": "kubocd-system",
			},
			"spec": map[string]interface{}{
				"context": map[string]interface{}{
					"okdp": okdp,
				},
			},
		},
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{contextGVR: "ContextList"},
		obj,
	)
	return NewContextRepository(client, "default", "kubocd-system")
}

func TestGetMenuCategories(t *testing.T) {
	repo := newContextWithOKDP(t, map[string]interface{}{
		"categories": []interface{}{
			map[string]interface{}{"key": "data-processing", "label": "Data Processing", "icon": "pi-cog", "order": int64(2)},
			map[string]interface{}{"key": "data-science", "label": "Data Science", "order": int64(1)},
		},
	})

	cats, err := repo.GetMenuCategories(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d (%+v)", len(cats), cats)
	}
	// Categories are returned in Context order; the console orders them via Order.
	if cats[0].Key != "data-processing" || cats[0].Label != "Data Processing" || cats[0].Icon != "pi-cog" || cats[0].Order != 2 {
		t.Errorf("category[0] = %+v, want {data-processing Data Processing pi-cog 2}", cats[0])
	}
	if cats[1].Key != "data-science" || cats[1].Order != 1 || cats[1].Icon != "" {
		t.Errorf("category[1] = %+v, want key=data-science order=1 icon empty", cats[1])
	}
}

func TestGetMenuCategoriesAbsentReturnsNil(t *testing.T) {
	repo := newContextWithOKDP(t, map[string]interface{}{
		"services": []interface{}{
			map[string]interface{}{"name": "jupyterhub", "default": "0.6.0"},
		},
	})

	cats, err := repo.GetMenuCategories(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cats != nil {
		t.Errorf("expected nil categories when okdp.categories is absent, got %+v", cats)
	}
}

func TestGetPlatformServicesLabelAndExposesUI(t *testing.T) {
	repo := newContextWithOKDP(t, map[string]interface{}{
		"services": []interface{}{
			map[string]interface{}{"name": "spark-operator", "default": "0.3.0", "exposesUI": false},
			map[string]interface{}{"name": "trino", "default": "0.3.0", "label": "Trino SQL", "category": "data-querying"},
		},
	})

	services, err := repo.GetPlatformServices(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := map[string]struct {
		label     string
		exposesUI *bool
	}{}
	for _, s := range services {
		byName[s.Name] = struct {
			label     string
			exposesUI *bool
		}{s.Label, s.ExposesUI}
	}

	sparkOp, ok := byName["spark-operator"]
	if !ok {
		t.Fatal("spark-operator not returned")
	}
	if sparkOp.exposesUI == nil || *sparkOp.exposesUI != false {
		t.Errorf("spark-operator ExposesUI = %v, want explicit false", sparkOp.exposesUI)
	}

	trino := byName["trino"]
	if trino.label != "Trino SQL" {
		t.Errorf("trino Label = %q, want %q", trino.label, "Trino SQL")
	}
	// exposesUI absent stays nil so the console can default it to "exposes a UI".
	if trino.exposesUI != nil {
		t.Errorf("trino ExposesUI = %v, want nil when unset", *trino.exposesUI)
	}
}
