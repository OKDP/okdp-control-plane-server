package repository

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func helmRelease(name, target string, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2",
		"kind":       "HelmRelease",
		"metadata":   map[string]any{"name": name, "namespace": "okdp-releases"},
		"spec":       map[string]any{"releaseName": name, "targetNamespace": target},
		"status":     status,
	}}
	return u
}

func cond(t, status, reason, message string) map[string]any {
	return map[string]any{"type": t, "status": status, "reason": reason, "message": message}
}

func TestFluxStatusMapsTheConditions(t *testing.T) {
	history := []any{map[string]any{"chartVersion": "1.0.0"}}
	cases := []struct {
		name   string
		status map[string]any
		phase  string
		msg    string
	}{
		{"never reconciled", map[string]any{}, PhaseInstalling, ""},
		{"first install running", map[string]any{"conditions": []any{cond("Ready", "Unknown", "Progressing", "Fresh install in progress")}}, PhaseInstalling, "Fresh install in progress"},
		{"upgrade running", map[string]any{"history": history, "conditions": []any{cond("Ready", "Unknown", "Progressing", "")}}, PhaseUpdating, "Progressing"},
		{"ready", map[string]any{"history": history, "conditions": []any{cond("Ready", "True", "UpgradeSucceeded", "ok")}}, PhaseReady, ""},
		{"install failed", map[string]any{"conditions": []any{cond("Ready", "False", "InstallFailed", "values don't meet the schema")}}, PhaseError, "values don't meet the schema"},
		{"chart pull failed", map[string]any{"conditions": []any{cond("Ready", "False", "ArtifactFailed", "chart not found")}}, PhaseError, "chart not found"},
		{"waiting on a dependency", map[string]any{"conditions": []any{cond("Ready", "False", "DependencyNotReady", "x")}}, PhaseInstalling, "x"},
		{"stalled", map[string]any{"history": history, "conditions": []any{cond("Ready", "True", "", ""), cond("Stalled", "True", "RetriesExceeded", "gave up")}}, PhaseError, "gave up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := FluxStatus(helmRelease("demo-hive", "demo", tc.status))
			if st.Phase != tc.phase || (tc.msg != "" && st.Message != tc.msg) {
				t.Fatalf("got %s %q, want %s %q", st.Phase, st.Message, tc.phase, tc.msg)
			}
		})
	}
	if st := FluxStatus(helmRelease("demo-hive", "demo", map[string]any{"history": history})); st.Revision != "1.0.0" {
		t.Errorf("revision = %q", st.Revision)
	}
}

func application(name, dest string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "argocd"},
		"spec": map[string]any{
			"destination": map[string]any{"namespace": dest},
			"sources": []any{
				map[string]any{"repoURL": "quay.io/okdp/platform-charts", "chart": "hive-metastore", "targetRevision": "1.0.0"},
				map[string]any{"repoURL": "https://git.example.com/deployments.git", "ref": "values"},
			},
		},
		"status": status,
	}}
}

func TestArgoCDStatusMapsSyncAndHealth(t *testing.T) {
	cases := []struct {
		name   string
		status map[string]any
		phase  string
	}{
		{"new", map[string]any{"sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Missing"}}, PhaseInstalling},
		{"healthy", map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Healthy"}}, PhaseReady},
		{"changing", map[string]any{"history": []any{map[string]any{}}, "sync": map[string]any{"status": "OutOfSync"}, "health": map[string]any{"status": "Healthy"}}, PhaseUpdating},
		{"render error", map[string]any{"conditions": []any{map[string]any{"type": "ComparisonError", "message": "helm template failed"}}}, PhaseError},
		{"sync failed", map[string]any{"operationState": map[string]any{"phase": "Failed", "message": "one or more objects failed"}}, PhaseError},
		{"degraded", map[string]any{"sync": map[string]any{"status": "Synced"}, "health": map[string]any{"status": "Degraded", "message": "CrashLoopBackOff"}}, PhaseError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := ArgoCDStatus(application("demo-hive", "demo", tc.status))
			if st.Phase != tc.phase {
				t.Fatalf("got %s (%q), want %s", st.Phase, st.Message, tc.phase)
			}
			if st.Revision != "1.0.0" {
				t.Errorf("revision = %q, want the chart source's targetRevision", st.Revision)
			}
		})
	}
}

func TestAdaptersKeepToTheirProject(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		helmReleaseGVR: "HelmReleaseList", applicationGVR: "ApplicationList",
	},
		helmRelease("demo-hive", "demo", map[string]any{}),
		helmRelease("other-hive", "other", map[string]any{}),
		application("demo-hive", "demo", map[string]any{}),
	)
	flux := NewFluxAdapter(client, "okdp-releases")
	list, err := flux.List(context.Background(), "demo")
	if err != nil || len(list) != 1 || !list["demo-hive"].Found {
		t.Fatalf("flux list = %v, %v", list, err)
	}
	if st, _ := flux.Get(context.Background(), "demo", "other-hive"); st.Found {
		t.Errorf("a release of another project leaked in")
	}
	if st, _ := flux.Get(context.Background(), "demo", "missing"); st.Found {
		t.Errorf("a missing HelmRelease is not found")
	}
	argo := NewArgoCDAdapter(client, "argocd")
	if st, err := argo.Get(context.Background(), "demo", "demo-hive"); err != nil || !st.Found {
		t.Fatalf("argo get = %v, %v", st, err)
	}
}

func TestDescriptorFromConfigMap(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-hive-okdp", Namespace: "demo", Labels: map[string]string{
			LabelDescriptorInstance: "demo-hive", LabelDescriptorService: "hive-metastore",
		}},
		Data: map[string]string{
			"version": "1.0.0",
			"url":     "",
			"usage":   "# Hive\n",
			"outputs.yaml": `- name: demo-hive
  contract: hive
  values: {thriftUri: "thrift://demo-hive-hive-metastore.demo.svc:9083"}
  secretRef: {name: demo-hive-hive-credentials}
`,
		},
	}
	d := DescriptorFromConfigMap(cm)
	if d.Release != "demo-hive" || d.Service != "hive-metastore" || d.Version != "1.0.0" || d.Usage != "# Hive\n" {
		t.Fatalf("descriptor = %+v", d)
	}
	if len(d.Outputs) != 1 || d.Outputs[0].Contract != "hive" || d.Outputs[0].SecretRef.Name != "demo-hive-hive-credentials" ||
		d.Outputs[0].Values["thriftUri"] != "thrift://demo-hive-hive-metastore.demo.svc:9083" {
		t.Fatalf("outputs = %+v", d.Outputs)
	}

	cm.Data["outputs.yaml"] = "{not: a list"
	if d := DescriptorFromConfigMap(cm); d.Outputs != nil || d.Release != "demo-hive" {
		t.Fatalf("a malformed outputs.yaml must drop the outputs only: %+v", d)
	}
}
