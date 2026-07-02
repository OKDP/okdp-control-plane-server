package service

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ingress builds a minimal unstructured Ingress with the given rule hosts.
func ingress(hosts ...string) unstructured.Unstructured {
	rules := make([]any, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, map[string]any{"host": h})
	}
	return unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"rules": rules},
	}}
}

func TestIngressHostsFromItems(t *testing.T) {
	t.Run("collects hosts across ingresses and rules", func(t *testing.T) {
		got := ingressHostsFromItems([]unstructured.Unstructured{
			ingress("test-jupyterhub.okdp.dev-sandbox"),
			ingress("a.example", "b.example"),
		})
		for _, want := range []string{"test-jupyterhub.okdp.dev-sandbox", "a.example", "b.example"} {
			if !got[want] {
				t.Errorf("expected host %q to be present, got %v", want, got)
			}
		}
		if len(got) != 3 {
			t.Errorf("expected 3 hosts, got %d (%v)", len(got), got)
		}
	})

	t.Run("empty when no ingresses (service without a web UI)", func(t *testing.T) {
		if got := ingressHostsFromItems(nil); len(got) != 0 {
			t.Errorf("expected no hosts, got %v", got)
		}
	})

	t.Run("skips rules with no or empty host", func(t *testing.T) {
		noHost := unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"rules": []any{map[string]any{}, map[string]any{"host": ""}}},
		}}
		if got := ingressHostsFromItems([]unstructured.Unstructured{noHost}); len(got) != 0 {
			t.Errorf("expected no hosts, got %v", got)
		}
	})
}
