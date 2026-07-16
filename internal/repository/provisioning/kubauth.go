package provisioning

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var oidcClientGVR = schema.GroupVersionResource{
	Group:    "kubauth.kubotal.io",
	Version:  "v1alpha1",
	Resource: "oidcclients",
}

type kubauthProvisioner struct {
	client      dynamic.Interface
	namespaceFn func(ctx context.Context) (string, error)
}

func newKubauthProvisioner(client dynamic.Interface, namespaceFn func(ctx context.Context) (string, error)) *kubauthProvisioner {
	return &kubauthProvisioner{client: client, namespaceFn: namespaceFn}
}

func (p *kubauthProvisioner) EnsureClient(ctx context.Context, spec OidcClientSpec) error {
	// Client creation currently happens declaratively (packages register their
	// own OidcClient CRs); the server only cleans up on service deletion.
	return errors.ErrUnsupported
}

func (p *kubauthProvisioner) DeleteClient(ctx context.Context, name string) error {
	ns, err := p.namespaceFn(ctx)
	if err != nil {
		return fmt.Errorf("resolving kubauth namespace: %w", err)
	}
	return p.client.Resource(oidcClientGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
}
