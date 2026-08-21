package models

import "fmt"

// Who makes the OAuth client Secret. Says nothing about where the provider is:
// those coordinates live in platform.oidc either way.
const (
	// Somebody else made the Secret and the package is handed its name.
	ClientProvisioningExisting = "existing"
	// The package generates it and posts its own OidcClient CR.
	ClientProvisioningKubauth = "kubauth"
	// The package registers its client against the provider at deploy time.
	ClientProvisioningDcr = "dcr"
)

// PlatformIdentity is the oidc client-provisioning block, written rather than inferred
// from the CRDs present: "kubauth is installed" is not "kubauth provisions our
// clients".
type PlatformIdentity struct {
	ClientProvisioning string `json:"clientProvisioning"`
	// Where the OidcClient CRs go. Only meaningful in the kubauth mode.
	KubauthNamespace string `json:"kubauthNamespace,omitempty"`
}

// Validate rejects a block that cannot be acted on, at startup rather than when
// a user first tries to log in.
func (i *PlatformIdentity) Validate() error {
	switch i.ClientProvisioning {
	case ClientProvisioningExisting, ClientProvisioningDcr:
		return nil
	case ClientProvisioningKubauth:
		if i.KubauthNamespace == "" {
			return fmt.Errorf("oidc.kubauth.namespace is required when clientProvisioning is %q", ClientProvisioningKubauth)
		}
		return nil
	default:
		return fmt.Errorf("oidc.clientProvisioning must be %q, %q or %q, got %q",
			ClientProvisioningExisting, ClientProvisioningKubauth, ClientProvisioningDcr, i.ClientProvisioning)
	}
}

// ProvisionsWithKubauth reports whether this platform posts OidcClient CRs.
func (i *PlatformIdentity) ProvisionsWithKubauth() bool {
	return i != nil && i.ClientProvisioning == ClientProvisioningKubauth
}
