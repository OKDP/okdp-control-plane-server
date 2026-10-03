package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/repository/provisioning"
)

// The provisioning provider is read from identity.provisioning.provider only:
// oidc.clientProvisioning (existing or dcr) never makes the server unmake
// clients itself.
func TestProvisioningProvider(t *testing.T) {
	cases := []struct {
		name         string
		body         map[string]interface{}
		wantProvider string
	}{
		{
			name: "keycloak",
			body: map[string]interface{}{
				"identity": map[string]interface{}{
					"provisioning": map[string]interface{}{"provider": "keycloak"},
				},
			},
			wantProvider: provisioning.ProviderKeycloak,
		},
		{
			name: "dcr packages register their own clients",
			body: map[string]interface{}{
				"oidc": map[string]interface{}{"clientProvisioning": "dcr"},
			},
			wantProvider: provisioning.ProviderNone,
		},
		{
			name:         "nothing declared",
			body:         map[string]interface{}{},
			wantProvider: provisioning.ProviderNone,
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
		})
	}
}
