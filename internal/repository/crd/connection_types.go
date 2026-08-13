package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// KuboCD connections. The CRDs ship with kubocd v0.3.2 as an experimental
// feature, so a platform may run without them: every access goes through
// ConnectionRepository.Available() and degrades to an empty result instead of
// an error. The shapes below mirror the upstream v0.3.2 definitions.
const (
	ConnectionAPIVersion  = "kubocd.kubotal.io/v1alpha1"
	ConnectionKind        = "Connection"
	ClusterConnectionKind = "ClusterConnection"

	// LabelManagedBy marks the Connections this server owns.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the value of LabelManagedBy for resources we create.
	ManagedByValue = "okdp-control-plane"
)

func GetConnectionGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "kubocd.kubotal.io",
		Version:  "v1alpha1",
		Resource: "connections",
	}
}

func GetClusterConnectionGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "kubocd.kubotal.io",
		Version:  "v1alpha1",
		Resource: "clusterconnections",
	}
}

type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionSpec   `json:"spec,omitempty"`
	Status ConnectionStatus `json:"status,omitempty"`
}

type ConnectionSpec struct {
	// Contract names the Contract (or ClusterContract) whose schema the values
	// must satisfy.
	Contract string `json:"contract"`
	// Priority arbitrates between several connections satisfying the same
	// contract. KuboCD defaults it to 100.
	Priority int `json:"priority,omitempty"`
	// Values holds the connection settings. Credentials are never stored here:
	// they live in a Kubernetes Secret whose name the values carry under the
	// secretRef key, which the Contracts declare for that purpose.
	Values map[string]any `json:"values,omitempty"`
	// OutputName is set by the KuboCD release controller on the connections it
	// manages, and is empty on the ones a user declared. It is what separates
	// the internal connections of a project from the external ones.
	OutputName  string `json:"outputName,omitempty"`
	Description string `json:"description,omitempty"`
}

type ConnectionStatus struct {
	Phase string `json:"phase,omitempty"`
	// Parent is the release owning a managed connection.
	Parent          string `json:"parent,omitempty"`
	Message         string `json:"message,omitempty"`
	ContractKind    string `json:"contractKind,omitempty"`
	ContractDisplay string `json:"contractDisplay,omitempty"`
}

// IsManaged reports whether the connection was produced by the KuboCD release
// controller rather than declared by a user.
func (c *Connection) IsManaged() bool {
	return c.Spec.OutputName != ""
}
