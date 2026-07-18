package models

// Capabilities describes the optional features the platform is configured
// with, so the UI can adapt (e.g. hide the Identity section when user
// management is not available). Derived from the Context at request time.
type Capabilities struct {
	Identity         IdentityCapability         `json:"identity"`
	OidcProvisioning OidcProvisioningCapability `json:"oidcProvisioning"`
}

// IdentityCapability describes how the platform integrates with its identity provider.
type IdentityCapability struct {
	// Provider is the configured identity provider: "external" (BYO OIDC,
	// default) or "kubauth".
	Provider string `json:"provider"`
	// UserManagement is true when the kubauth-specific user/group management
	// API (/api/v1/identity) is available.
	UserManagement bool `json:"userManagement"`
	// Oidc is the OIDC client the console UI should authenticate with,
	// resolved from the Context (identity.oidc). Absent when the platform
	// does not publish it: the UI falls back to its build-time configuration.
	Oidc *IdentityOidcConfig `json:"oidc,omitempty"`
}

// IdentityOidcConfig is the OIDC client configuration served to the console UI.
type IdentityOidcConfig struct {
	// Authority is the OIDC issuer the UI redirects to.
	Authority string `json:"authority"`
	// ClientID is the public (PKCE) client registered for the console.
	ClientID string `json:"clientId"`
	// Scope overrides the UI default scopes when set.
	Scope string `json:"scope,omitempty"`
}

// OidcProvisioningCapability describes the OIDC client provisioning backend.
type OidcProvisioningCapability struct {
	// Provider is the configured provisioning backend: "none" (default),
	// "kubauth" or "keycloak".
	Provider string `json:"provider"`
}
