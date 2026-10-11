package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func rawSecretStore(name string, provider map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": crd.SecretStoreAPIVersion,
		"kind":       crd.SecretStoreKind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "demo"},
		"spec":       map[string]interface{}{"provider": provider},
	}}
}

func secretStoreRepo(objs ...runtime.Object) *k8sSecretStoreRepository {
	gvr := crd.GetSecretStoreGVR()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "SecretStoreList"},
		objs...,
	)
	return &k8sSecretStoreRepository{client: client, gvr: gvr}
}

func TestSecretStoreKeepsTheProviderName(t *testing.T) {
	repo := secretStoreRepo(
		rawSecretStore("vault-store", map[string]interface{}{
			"vault": map[string]interface{}{"server": "https://vault.example.com", "path": "secret", "version": "v2"},
		}),
		rawSecretStore("k8s-store", map[string]interface{}{
			"kubernetes": map[string]interface{}{"remoteNamespace": "demo"},
		}),
	)

	stores, err := repo.List(context.Background(), "demo")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]crd.ESOProvider{}
	for _, s := range stores {
		got[s.Name] = s.Spec.Provider
	}

	if p := got["vault-store"]; p.Name != "vault" || p.Vault == nil {
		t.Fatalf("vault store read as %+v, want name vault with its config", p)
	}
	if p := got["k8s-store"]; p.Name != "kubernetes" || p.Vault != nil {
		t.Fatalf("kubernetes store read as %+v, want name kubernetes and no vault config", p)
	}

	store, err := repo.Get(context.Background(), "demo", "k8s-store")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if store.Spec.Provider.Name != "kubernetes" {
		t.Fatalf("get read provider %q, want kubernetes", store.Spec.Provider.Name)
	}
}
