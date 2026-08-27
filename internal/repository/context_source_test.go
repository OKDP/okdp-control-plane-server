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

// contextWithOidc builds the platform Context with the given flat oidc block.
func contextWithOidc(t *testing.T, oidc map[string]interface{}) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{contextGVR: "ContextList"}

	platform := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubocd.kubotal.io/v1alpha1",
		"kind":       "Context",
		"metadata":   map[string]interface{}{"name": "platform", "namespace": "okdp-system"},
		"spec": map[string]interface{}{
			"context": map[string]interface{}{"oidc": oidc},
		},
	}}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, platform)
}

// The issuer is what the API trusts to authenticate every caller, so a value
// that is there but malformed must not read as a value that is absent. Reported
// as absent, it is answered with "this platform names no provider", and whoever
// reads that goes looking for a setting that is right there and wrong.
func TestAMalformedIssuerIsReportedInsteadOfBeingReadAsAbsent(t *testing.T) {
	client := contextWithOidc(t, map[string]interface{}{"issuerUri": map[string]interface{}{"url": "https://issuer"}})
	repo := NewContextRepository(client, "platform", "okdp-system")

	issuer, err := repo.GetOidcIssuerURI(context.Background())
	if err == nil {
		t.Fatalf("a malformed issuerUri was read as %q with no error", issuer)
	}
	if !strings.Contains(err.Error(), "spec.context.oidc.issuerUri") {
		t.Fatalf("the error does not name the field: %v", err)
	}
}

func TestAnAbsentIssuerIsNotAnError(t *testing.T) {
	repo := NewContextRepository(emptyPlatformContext(t), "platform", "okdp-system")

	issuer, err := repo.GetOidcIssuerURI(context.Background())
	if err != nil || issuer != "" {
		t.Fatalf("wanted an empty issuer and no error, got %q and %v", issuer, err)
	}
}

func TestTheIssuerIsReadFromTheFlatOidcBlock(t *testing.T) {
	client := contextWithOidc(t, map[string]interface{}{"issuerUri": "https://issuer.example/realms/main"})
	repo := NewContextRepository(client, "platform", "okdp-system")

	issuer, err := repo.GetOidcIssuerURI(context.Background())
	if err != nil || issuer != "https://issuer.example/realms/main" {
		t.Fatalf("got %q and %v", issuer, err)
	}
}
