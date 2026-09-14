package models

import "fmt"

// Who makes the OAuth client Secret. Says nothing about where the provider is:
// those coordinates live in platform.oidc either way.
const (
	// Somebody else made the client in Keycloak beforehand and the package is
	// handed the name of its Secret.
	ClientProvisioningExisting = "existing"
	// The package registers its client against the provider at deploy time.
	ClientProvisioningDcr = "dcr"
)

// PlatformIdentity is the oidc client-provisioning block, written rather than
// inferred from what the cluster happens to serve.
type PlatformIdentity struct {
	ClientProvisioning string `json:"clientProvisioning"`
}

// Validate rejects a block that cannot be acted on, at startup rather than when
// a user first tries to log in.
func (i *PlatformIdentity) Validate() error {
	switch i.ClientProvisioning {
	case ClientProvisioningExisting, ClientProvisioningDcr:
		return nil
	default:
		return fmt.Errorf("oidc.clientProvisioning must be %q or %q, got %q",
			ClientProvisioningExisting, ClientProvisioningDcr, i.ClientProvisioning)
	}
}
