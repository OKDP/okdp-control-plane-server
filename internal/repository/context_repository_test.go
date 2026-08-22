package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// newContextWith builds a fake dynamic client holding a single KuboCD Context
// whose spec.context carries the given body.
func newContextWith(t *testing.T, body map[string]interface{}) ContextRepository {
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
				"context": body,
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
	repo := newContextWith(t, map[string]interface{}{"serviceCatalog": map[string]interface{}{
		"categories": []interface{}{
			map[string]interface{}{"title": "Data Processing", "icon": "pi-cog", "services": []interface{}{
				map[string]interface{}{"name": "spark-operator", "default": "0.3.0"},
			}},
			map[string]interface{}{"title": "Data Science"},
		},
	}})

	cats, err := repo.GetMenuCategories(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cats) != 2 {
		t.Fatalf("expected 2 categories, got %d (%+v)", len(cats), cats)
	}
	// The section title is both key and label, the position is the order.
	if cats[0].Key != "Data Processing" || cats[0].Label != "Data Processing" || cats[0].Icon != "pi-cog" || cats[0].Order != 1 {
		t.Errorf("category[0] = %+v, want {Data Processing Data Processing pi-cog 1}", cats[0])
	}
	if cats[1].Key != "Data Science" || cats[1].Order != 2 || cats[1].Icon != "" {
		t.Errorf("category[1] = %+v, want key=Data Science order=2 icon empty", cats[1])
	}
}

func TestGetMenuCategoriesAbsentReturnsNil(t *testing.T) {
	repo := newContextWith(t, map[string]interface{}{"serviceCatalog": map[string]interface{}{
		"defaultRepository": "quay.io/okdp/platform-packages",
	}})

	cats, err := repo.GetMenuCategories(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cats != nil {
		t.Errorf("expected nil categories when serviceCatalog.categories is absent, got %+v", cats)
	}
}

func TestGetPlatformServicesLabelAndExposesUI(t *testing.T) {
	repo := newContextWith(t, map[string]interface{}{"serviceCatalog": map[string]interface{}{
		"categories": []interface{}{
			map[string]interface{}{"title": "Spark", "services": []interface{}{
				map[string]interface{}{"name": "spark-operator", "default": "0.3.0", "exposesUI": false},
			}},
			map[string]interface{}{"title": "Interactive Query", "services": []interface{}{
				map[string]interface{}{"name": "trino", "default": "0.3.0", "label": "Trino SQL"},
			}},
		},
	}})

	services, err := repo.GetPlatformServices(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := map[string]models.PlatformService{}
	for _, s := range services {
		byName[s.Name] = s
	}

	sparkOp, ok := byName["spark-operator"]
	if !ok {
		t.Fatal("spark-operator not returned")
	}
	if sparkOp.ExposesUI == nil || *sparkOp.ExposesUI != false {
		t.Errorf("spark-operator ExposesUI = %v, want explicit false", sparkOp.ExposesUI)
	}
	if sparkOp.Category != "Spark" {
		t.Errorf("spark-operator Category = %q, want the section title %q", sparkOp.Category, "Spark")
	}

	trino := byName["trino"]
	if trino.Label != "Trino SQL" {
		t.Errorf("trino Label = %q, want %q", trino.Label, "Trino SQL")
	}
	if trino.Category != "Interactive Query" {
		t.Errorf("trino Category = %q, want %q", trino.Category, "Interactive Query")
	}
	// exposesUI absent stays nil so the console can default it to "exposes a UI".
	if trino.ExposesUI != nil {
		t.Errorf("trino ExposesUI = %v, want nil when unset", *trino.ExposesUI)
	}
}
