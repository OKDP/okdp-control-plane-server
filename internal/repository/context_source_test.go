package repository

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// emptyPlatformContext builds the single platform Context, with no values.
func emptyPlatformContext(t *testing.T) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{contextGVR: "ContextList"}

	platform := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubocd.kubotal.io/v1alpha1",
		"kind":       "Context",
		"metadata":   map[string]interface{}{"name": "platform", "namespace": "okdp-system"},
		"spec":       map[string]interface{}{"context": map[string]interface{}{}},
	}}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, platform)
}

func TestMissingKeysNameTheContext(t *testing.T) {
	repo := NewContextRepository(emptyPlatformContext(t), "platform", "okdp-system")

	cases := []struct {
		name string
		call func() error
	}{
		{"ingress.suffix", func() error {
			_, err := repo.GetIngressSuffix(context.Background())
			return err
		}},
		{"sparkOperator", func() error {
			_, err := repo.GetSparkConfig(context.Background())
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected an error naming the Context")
			}
			if !strings.Contains(err.Error(), "okdp-system/platform") {
				t.Errorf("expected the platform Context to be named, got %q", err.Error())
			}
		})
	}
}
