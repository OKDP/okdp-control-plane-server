package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
)

// The provisioning provider and the kubauth namespace must agree whichever
// vocabulary the platform values use, otherwise the deleted services keep their
// OidcClient forever.
func TestProvisioningReadsBothVocabularies(t *testing.T) {
	cases := []struct {
		name             string
		body             map[string]interface{}
		wantProvider     string
		wantNamespace    string
		wantNamespaceErr bool
	}{
		{
			name: "current keys",
			body: map[string]interface{}{
				"identity": map[string]interface{}{
					"provisioning": map[string]interface{}{"provider": "kubauth"},
					"kubauth":      map[string]interface{}{"namespace": "kubauth"},
				},
			},
			wantProvider:  provisioning.ProviderKubauth,
			wantNamespace: "kubauth",
		},
		{
			name: "legacy oidc block",
			body: map[string]interface{}{
				"oidc": map[string]interface{}{
					"clientProvisioning": "kubauth",
					"kubauth":            map[string]interface{}{"namespace": "kubauth-legacy"},
				},
			},
			wantProvider:  provisioning.ProviderKubauth,
			wantNamespace: "kubauth-legacy",
		},
		{
			name:             "nothing declared",
			body:             map[string]interface{}{},
			wantProvider:     provisioning.ProviderNone,
			wantNamespaceErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newContextWith(t, tc.body)

			provider, err := repo.GetIdentityProvisioningProvider(context.Background())
			if err != nil {
				t.Fatalf("reading the provider: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider: got %q, want %q", provider, tc.wantProvider)
			}

			namespace, err := repo.GetKubauthNamespace(context.Background())
			if tc.wantNamespaceErr {
				if err == nil {
					t.Fatalf("expected an error, got namespace %q", namespace)
				}
				return
			}
			if err != nil {
				t.Fatalf("reading the namespace: %v", err)
			}
			if namespace != tc.wantNamespace {
				t.Fatalf("namespace: got %q, want %q", namespace, tc.wantNamespace)
			}
		})
	}
}
