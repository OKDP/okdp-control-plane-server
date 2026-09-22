package service

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// ingress builds a minimal unstructured Ingress owned by the given
// HelmRelease (helm.toolkit.fluxcd.io/name label), with the given rule hosts.
func ingress(owner string, hosts ...string) unstructured.Unstructured {
	rules := make([]any, 0, len(hosts))
	for _, h := range hosts {
		rules = append(rules, map[string]any{"host": h})
	}
	u := unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"rules": rules},
	}}
	if owner != "" {
		u.SetLabels(map[string]string{"helm.toolkit.fluxcd.io/name": owner})
	}
	return u
}

func TestIngressHostsFromItems(t *testing.T) {
	t.Run("collects hosts across ingresses and rules, keyed to their owning HelmRelease", func(t *testing.T) {
		got := ingressHostsFromItems([]unstructured.Unstructured{
			ingress("demo-jupyterhub-main", "test-jupyterhub.okdp.dev-sandbox"),
			ingress("demo-something-main", "a.example", "b.example"),
		})
		want := map[string]string{
			"test-jupyterhub.okdp.dev-sandbox": "demo-jupyterhub-main",
			"a.example":                        "demo-something-main",
			"b.example":                        "demo-something-main",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty when no ingresses (service without a web UI)", func(t *testing.T) {
		if got := ingressHostsFromItems(nil); len(got) != 0 {
			t.Errorf("expected no hosts, got %v", got)
		}
	})

	t.Run("skips rules with no or empty host", func(t *testing.T) {
		noHost := ingress("demo-something-main")
		noHost.Object["spec"] = map[string]any{"rules": []any{map[string]any{}, map[string]any{"host": ""}}}
		if got := ingressHostsFromItems([]unstructured.Unstructured{noHost}); len(got) != 0 {
			t.Errorf("expected no hosts, got %v", got)
		}
	})

	t.Run("skips an Ingress with no helm.toolkit.fluxcd.io/name label: unknown owner, never attributed", func(t *testing.T) {
		unowned := ingress("", "orphan.example")
		if got := ingressHostsFromItems([]unstructured.Unstructured{unowned}); len(got) != 0 {
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

	t.Run("release name first, then the spark role convention (web proxy host)", func(t *testing.T) {
		instance := &models.ServiceInstance{ReleaseName: "demo-spark-history", TargetNamespace: "demo", Roles: []string{"spark"}, Service: "spark-history-server"}
		got := candidateHosts(instance, "okdp.sandbox")
		want := []string{"demo-spark-history.okdp.sandbox", "spark-web-proxy-demo.okdp.sandbox", "spark-history-server-console-demo.okdp.sandbox", "spark-history-server-demo.okdp.sandbox"}
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
