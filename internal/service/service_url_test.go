package service

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/okdp/okdp-server-new/internal/models"
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

func TestCandidateHosts(t *testing.T) {
	t.Run("release name only, when the instance has no role convention", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-jupyterhub", TargetNamespace: "demo"}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-jupyterhub.okdp.sandbox"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("release name first, then the storage role convention", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-rustfs", TargetNamespace: "demo", Roles: []string{"storage"}}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-rustfs.okdp.sandbox", "storage-demo.okdp.sandbox"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("roles with no known convention contribute no extra candidate", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-spark-operator", TargetNamespace: "demo", Roles: []string{"compute"}, Service: "spark-operator"}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-spark-operator.okdp.sandbox", "spark-operator-console-demo.okdp.sandbox", "spark-operator-demo.okdp.sandbox"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("falls back to the <service>-console-<namespace> convention (Polaris split main/console)", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-polaris", TargetNamespace: "demo", Service: "polaris"}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-polaris.okdp.sandbox", "polaris-console-demo.okdp.sandbox", "polaris-demo.okdp.sandbox"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("falls back to the <service>-<namespace> convention (single-ingress services like Trino)", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-trino", TargetNamespace: "demo", Service: "trino"}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-trino.okdp.sandbox", "trino-console-demo.okdp.sandbox", "trino-demo.okdp.sandbox"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("no service name contributes no extra candidate", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-something", TargetNamespace: "demo"}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-something.okdp.sandbox"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}
