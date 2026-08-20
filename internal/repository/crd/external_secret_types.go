package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ESOExternalSecret maps to the external-secrets.io/v1beta1 ExternalSecret CRD
type ESOExternalSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ESOExternalSecretSpec   `json:"spec,omitempty"`
	Status ESOExternalSecretStatus `json:"status,omitempty"`
}

// ESOExternalSecretSpec is the spec of an ExternalSecret
type ESOExternalSecretSpec struct {
	RefreshInterval string                  `json:"refreshInterval"`
	SecretStoreRef  ESOSecretStoreRef       `json:"secretStoreRef"`
	Target          ESOExternalSecretTarget `json:"target"`
	Data            []ESOExternalSecretData `json:"data"`
}

// ESOSecretStoreRef references a SecretStore
type ESOSecretStoreRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// ESOExternalSecretTarget configures the target Secret
type ESOExternalSecretTarget struct {
	Name           string `json:"name"`
	CreationPolicy string `json:"creationPolicy"`
}

// ESOExternalSecretData defines a single key mapping
type ESOExternalSecretData struct {
	SecretKey string       `json:"secretKey"`
	RemoteRef ESORemoteRef `json:"remoteRef"`
}

// ESORemoteRef points to a key in the remote secret store
type ESORemoteRef struct {
	Key      string `json:"key"`
	Property string `json:"property,omitempty"`
}

// ESOExternalSecretStatus holds the observed status from ESO
type ESOExternalSecretStatus struct {
	Conditions  []ESOCondition `json:"conditions,omitempty"`
	RefreshTime string         `json:"refreshTime,omitempty"`
}

// DeepCopyInto copies all properties into another object
func (in *ESOExternalSecret) DeepCopyInto(out *ESOExternalSecret) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ESOExternalSecretSpec) DeepCopyInto(out *ESOExternalSecretSpec) {
	*out = *in
	out.SecretStoreRef = in.SecretStoreRef
	out.Target = in.Target
	if in.Data != nil {
		in, out := &in.Data, &out.Data
		*out = make([]ESOExternalSecretData, len(*in))
		copy(*out, *in)
	}
}

func (in *ESOExternalSecretStatus) DeepCopyInto(out *ESOExternalSecretStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]ESOCondition, len(*in))
		copy(*out, *in)
	}
}

func (in *ESOExternalSecret) DeepCopy() *ESOExternalSecret {
	if in == nil {
		return nil
	}
	out := new(ESOExternalSecret)
	in.DeepCopyInto(out)
	return out
}

func (in *ESOExternalSecret) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// GetExternalSecretGVR returns the GVR for the ESO ExternalSecret CRD
func GetExternalSecretGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "external-secrets.io",
		Version:  "v1beta1",
		Resource: "externalsecrets",
	}
}

const (
	ExternalSecretAPIVersion = "external-secrets.io/v1beta1"
	ExternalSecretKind       = "ExternalSecret"
)
