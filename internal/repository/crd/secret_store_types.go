package crd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ESOSecretStore maps to the external-secrets.io/v1beta1 SecretStore CRD
type ESOSecretStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ESOSecretStoreSpec   `json:"spec,omitempty"`
	Status ESOSecretStoreStatus `json:"status,omitempty"`
}

// ESOSecretStoreSpec is the spec of a SecretStore
type ESOSecretStoreSpec struct {
	Provider ESOProvider `json:"provider"`
}

// ESOProvider wraps the provider-specific configuration
type ESOProvider struct {
	Vault *ESOVaultProvider `json:"vault,omitempty"`
}

// ESOVaultProvider configures the Vault backend
type ESOVaultProvider struct {
	Server  string `json:"server"`
	Path    string `json:"path"`
	Version string `json:"version"`
	// The CRD declares caBundle as `format: byte`, so the API server only accepts
	// base64. A []byte marshals that way on its own; a string would ship raw PEM
	// and every store carrying a private CA would be rejected on create.
	CABundle []byte       `json:"caBundle,omitempty"`
	Auth     ESOVaultAuth `json:"auth"`
}

// ESOVaultAuth holds the authentication method for Vault
type ESOVaultAuth struct {
	TokenSecretRef *ESOTokenSecretRef `json:"tokenSecretRef,omitempty"`
	Kubernetes     *ESOKubernetesAuth `json:"kubernetes,omitempty"`
}

// ESOTokenSecretRef references a Secret key containing the Vault token
type ESOTokenSecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// ESOKubernetesAuth configures Vault kubernetes auth method
type ESOKubernetesAuth struct {
	MountPath         string                `json:"mountPath"`
	Role              string                `json:"role"`
	ServiceAccountRef *ESOServiceAccountRef `json:"serviceAccountRef,omitempty"`
}

// ESOServiceAccountRef references a Kubernetes ServiceAccount
type ESOServiceAccountRef struct {
	Name string `json:"name"`
}

// ESOSecretStoreStatus holds the observed status from ESO
type ESOSecretStoreStatus struct {
	Conditions []ESOCondition `json:"conditions,omitempty"`
}

// ESOCondition is a standard Kubernetes-style condition
type ESOCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// DeepCopyInto copies all properties into another object
func (in *ESOSecretStore) DeepCopyInto(out *ESOSecretStore) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ESOSecretStoreSpec) DeepCopyInto(out *ESOSecretStoreSpec) {
	*out = *in
	in.Provider.DeepCopyInto(&out.Provider)
}

func (in *ESOProvider) DeepCopyInto(out *ESOProvider) {
	*out = *in
	if in.Vault != nil {
		in, out := &in.Vault, &out.Vault
		*out = new(ESOVaultProvider)
		**out = **in
		// CABundle is a slice, so the struct copy above only duplicates its
		// header and both copies would write through to the same bytes.
		if (*in).CABundle != nil {
			(*out).CABundle = append([]byte(nil), (*in).CABundle...)
		}
		(*in).Auth.DeepCopyInto(&(*out).Auth)
	}
}

func (in *ESOVaultAuth) DeepCopyInto(out *ESOVaultAuth) {
	*out = *in
	if in.TokenSecretRef != nil {
		in, out := &in.TokenSecretRef, &out.TokenSecretRef
		*out = new(ESOTokenSecretRef)
		**out = **in
	}
	if in.Kubernetes != nil {
		in, out := &in.Kubernetes, &out.Kubernetes
		*out = new(ESOKubernetesAuth)
		**out = **in
		if (*in).ServiceAccountRef != nil {
			(*out).ServiceAccountRef = new(ESOServiceAccountRef)
			*(*out).ServiceAccountRef = *(*in).ServiceAccountRef
		}
	}
}

func (in *ESOSecretStoreStatus) DeepCopyInto(out *ESOSecretStoreStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]ESOCondition, len(*in))
		copy(*out, *in)
	}
}

func (in *ESOSecretStore) DeepCopy() *ESOSecretStore {
	if in == nil {
		return nil
	}
	out := new(ESOSecretStore)
	in.DeepCopyInto(out)
	return out
}

func (in *ESOSecretStore) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// GetSecretStoreGVR returns the GVR for the ESO SecretStore CRD
func GetSecretStoreGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    "external-secrets.io",
		Version:  "v1beta1",
		Resource: "secretstores",
	}
}

const (
	SecretStoreAPIVersion = "external-secrets.io/v1beta1"
	SecretStoreKind       = "SecretStore"

	LabelDefaultStore = "okdp.io/default-secret-store"
)
