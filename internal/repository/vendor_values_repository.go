package repository

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Labels and keys of the values ConfigMaps okdp.vendor.render (okdp-lib)
// emits, one per render of a vendored upstream chart.
const (
	LabelVendorValues     = "okdp.io/vendor-values"
	AnnotationVendorChart = "okdp.io/vendor-chart"
	LabelHelmChart        = "helm.sh/chart"
	vendorValuesKeyValues = "values.yaml"
	vendorValuesSelector  = LabelVendorValues + "," + LabelAppInstance + "="
)

// VendorValues is one values ConfigMap.
type VendorValues struct {
	Name string
	// Chart is the vendored chart directory (label okdp.io/vendor-values).
	Chart string
	// ChartVersion is the vendored chart, <name>-<version>.
	ChartVersion string
	// HelmChart is the label helm.sh/chart of the wrapper chart that
	// rendered it: <chart>-<version> ("+" written "_").
	HelmChart string
	Values    string
}

// VendorValuesRepository reads the values ConfigMaps of a release.
type VendorValuesRepository interface {
	List(ctx context.Context, namespace, release string) ([]VendorValues, error)
}

type k8sVendorValuesRepository struct {
	client kubernetes.Interface
}

func NewVendorValuesRepository(client kubernetes.Interface) VendorValuesRepository {
	return &k8sVendorValuesRepository{client: client}
}

func (r *k8sVendorValuesRepository) List(ctx context.Context, namespace, release string) ([]VendorValues, error) {
	list, err := r.client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{LabelSelector: vendorValuesSelector + release})
	if err != nil {
		return nil, err
	}
	out := make([]VendorValues, 0, len(list.Items))
	for _, cm := range list.Items {
		out = append(out, VendorValues{
			Name:         cm.Name,
			Chart:        cm.Labels[LabelVendorValues],
			ChartVersion: cm.Annotations[AnnotationVendorChart],
			HelmChart:    cm.Labels[LabelHelmChart],
			Values:       cm.Data[vendorValuesKeyValues],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
