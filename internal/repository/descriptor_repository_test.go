package repository

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func labelledConfigMap(name, release string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "demo",
			Labels:    map[string]string{LabelDescriptorInstance: release, LabelAppInstance: release},
		},
		Data: data,
	}
}

// The values ConfigMaps of okdp.vendor.render and a chart's own ConfigMaps
// carry okdp.io/instance (okdp.labels) but are not descriptors: listing them
// as such would replace the instance's descriptor with an empty one.
func TestDescriptorListSkipsOtherLabelledConfigMaps(t *testing.T) {
	client := k8sfake.NewSimpleClientset(
		labelledConfigMap("demo-trino-okdp", "demo-trino", map[string]string{"service": "trino", "url": "https://trino"}),
		labelledConfigMap("demo-trino-trino-values", "demo-trino", map[string]string{"values.yaml": "a: 1\n"}),
		labelledConfigMap("demo-trino-catalogs", "demo-trino", map[string]string{"bronze": "x"}),
	)
	list, err := NewDescriptorRepository(client).List(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].URL != "https://trino" {
		t.Fatalf("want only the descriptor demo-trino-okdp, got %+v", list)
	}
}

func TestDescriptorName(t *testing.T) {
	if got := DescriptorName("demo-trino"); got != "demo-trino-okdp" {
		t.Errorf("DescriptorName = %q", got)
	}
	// okdp.fullname: cut at 63 characters, then a trailing "-" dropped.
	long := strings.Repeat("a", 58) + "-b"
	if got := DescriptorName(long); got != long+"-ok" {
		t.Errorf("DescriptorName(60 chars) = %q", got)
	}
	release := strings.Repeat("a", 62)
	if got := DescriptorName(release); got != release {
		t.Errorf("DescriptorName(62 chars) = %q, want the release without the trailing -", got)
	}
}
