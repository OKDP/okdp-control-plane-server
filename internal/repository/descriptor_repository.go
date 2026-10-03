package repository

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	sigsyaml "sigs.k8s.io/yaml"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Labels and keys of the instance descriptor ConfigMap every OKDP service
// chart renders (<release>-okdp, shared no-kubocd contract).
const (
	LabelDescriptorInstance = "okdp.io/instance"
	LabelDescriptorService  = "okdp.io/service"
	LabelProvidesPrefix     = "okdp.io/provides-"
	LabelAppInstance        = "app.kubernetes.io/instance"

	descriptorKeyService = "service"
	descriptorKeyVersion = "version"
	descriptorKeyURL     = "url"
	descriptorKeyUsage   = "usage"
	descriptorKeyOutputs = "outputs.yaml"
)

// Descriptor is what a deployed instance says about itself.
type Descriptor struct {
	// Release is the Helm release name (<project>-<instance>).
	Release   string
	Namespace string
	Service   string
	Version   string
	URL       string
	Usage     string
	Outputs   []DescriptorOutput
	CreatedAt time.Time
}

// DescriptorOutput is a connection an instance provides.
type DescriptorOutput struct {
	Name     string         `json:"name"`
	Contract string         `json:"contract"`
	Values   map[string]any `json:"values,omitempty"`
	// SecretRef names the Secret, in the instance namespace, holding the
	// secret fields of the contract.
	SecretRef struct {
		Name string `json:"name"`
	} `json:"secretRef"`
}

// DescriptorRepository reads the instance descriptors of a project.
type DescriptorRepository interface {
	List(ctx context.Context, namespace string) ([]Descriptor, error)
	Watch(ctx context.Context, namespace string) (watch.Interface, error)
}

type k8sDescriptorRepository struct {
	client kubernetes.Interface
}

func NewDescriptorRepository(client kubernetes.Interface) DescriptorRepository {
	return &k8sDescriptorRepository{client: client}
}

func (r *k8sDescriptorRepository) List(ctx context.Context, namespace string) ([]Descriptor, error) {
	list, err := r.client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{LabelSelector: LabelDescriptorInstance})
	if err != nil {
		return nil, err
	}
	out := make([]Descriptor, 0, len(list.Items))
	for i := range list.Items {
		if !IsDescriptorConfigMap(&list.Items[i]) {
			continue
		}
		out = append(out, DescriptorFromConfigMap(&list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Release < out[j].Release })
	return out, nil
}

func (r *k8sDescriptorRepository) Watch(ctx context.Context, namespace string) (watch.Interface, error) {
	return r.client.CoreV1().ConfigMaps(namespace).Watch(ctx, metav1.ListOptions{LabelSelector: LabelDescriptorInstance})
}

// IsDescriptorConfigMap tells the descriptor <release>-okdp (okdp.fullname
// with suffix "okdp") from the other ConfigMaps an OKDP chart labels with
// okdp.labels, which carry okdp.io/instance too: the values ConfigMaps of
// okdp.vendor.render (<release>-<chart>-values), a chart's own ConfigMaps.
// Taking one of them for the descriptor would blank the instance's URL, usage
// and outputs.
func IsDescriptorConfigMap(cm *corev1.ConfigMap) bool {
	release := cm.Labels[LabelDescriptorInstance]
	return release != "" && cm.Name == DescriptorName(release)
}

// DescriptorName is the name of the descriptor ConfigMap of a release:
// "<release>-okdp" cut at 63 characters, without a trailing "-".
func DescriptorName(release string) string {
	name := release + "-okdp"
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimSuffix(name, "-")
}

// DescriptorFromConfigMap parses a descriptor ConfigMap. A malformed
// outputs.yaml drops the outputs, not the instance.
func DescriptorFromConfigMap(cm *corev1.ConfigMap) Descriptor {
	d := Descriptor{
		Release:   cm.Labels[LabelDescriptorInstance],
		Namespace: cm.Namespace,
		Service:   cm.Data[descriptorKeyService],
		Version:   cm.Data[descriptorKeyVersion],
		URL:       cm.Data[descriptorKeyURL],
		Usage:     cm.Data[descriptorKeyUsage],
		CreatedAt: cm.CreationTimestamp.Time,
	}
	if d.Service == "" {
		d.Service = cm.Labels[LabelDescriptorService]
	}
	if raw := cm.Data[descriptorKeyOutputs]; raw != "" {
		if err := sigsyaml.Unmarshal([]byte(raw), &d.Outputs); err != nil {
			logrus.WithError(err).WithField("configmap", cm.Namespace+"/"+cm.Name).Warn("Ignoring the malformed outputs of an instance descriptor")
			d.Outputs = nil
		}
	}
	return d
}
