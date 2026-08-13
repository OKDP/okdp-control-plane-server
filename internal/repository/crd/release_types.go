package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	ReleaseAPIVersion = "kubocd.kubotal.io/v1alpha1"
	ReleaseKind       = "Release"

	LabelProject      = "okdp.io/project"
	LabelService      = "okdp.io/service"
	LabelInstanceName = "okdp.io/instance-name"
)

func GetReleaseGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "kubocd.kubotal.io",
		Version:  "v1alpha1",
		Resource: "releases",
	}
}

type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReleaseSpec   `json:"spec,omitempty"`
	Status ReleaseStatus `json:"status,omitempty"`
}

type ReleaseSpec struct {
	Description     string         `json:"description,omitempty"`
	Package         ReleasePackage `json:"package"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	Contexts        []ContextRef   `json:"contexts,omitempty"`
	TargetNamespace string         `json:"targetNamespace"`
	CreateNamespace bool           `json:"createNamespace"`
}

type ReleasePackage struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Interval   string `json:"interval,omitempty"`
	Timeout    string `json:"timeout,omitempty"`
	// Insecure lets KuboCD fetch the package over plain HTTP. Set only for
	// registries listed in INSECURE_OCI_REGISTRIES (development sandboxes).
	Insecure bool `json:"insecure,omitempty"`
}

type ContextRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type ReleaseStatus struct {
	Phase      string             `json:"phase,omitempty"`
	Roles      []string           `json:"roles,omitempty"`
	Conditions []ReleaseCondition `json:"conditions,omitempty"`
	// WatchedInputConnections lists the connections this release depends on,
	// whether or not they resolved. EffectiveInputConnections lists the ones it
	// actually got. The controller writes both. Without them the console can
	// only show what a service asked for, never what it is running against.
	WatchedInputConnections   []InputConnectionReference `json:"watchedInputConnections,omitempty"`
	EffectiveInputConnections []InputConnectionReference `json:"effectiveInputConnections,omitempty"`
}

// InputConnectionReference identifies one connection a release consumes. The
// kind matters: a Connection and a ClusterConnection may share a name.
type InputConnectionReference struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type ReleaseCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}
