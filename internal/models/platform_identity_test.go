package models

import "testing"

func TestExistingNeedsNothingElse(t *testing.T) {
	identity := &PlatformIdentity{ClientProvisioning: ClientProvisioningExisting}

	if err := identity.Validate(); err != nil {
		t.Fatalf("expected the existing mode to be self-sufficient, got %v", err)
	}
	if identity.ProvisionsWithKubauth() {
		t.Error("the existing mode must not post OidcClient CRs")
	}
}

// The namespace is what the OidcClient CRs are posted into. Accepting the mode
// without it would mean services deploy and their clients are never created,
// which only surfaces when a user tries to log in.
func TestKubauthRequiresItsNamespace(t *testing.T) {
	identity := &PlatformIdentity{ClientProvisioning: ClientProvisioningKubauth}

	err := identity.Validate()
	if err == nil {
		t.Fatal("expected the kubauth mode to require a namespace")
	}
	if !identity.ProvisionsWithKubauth() {
		t.Error("the kubauth mode must post OidcClient CRs")
	}
}

func TestKubauthWithItsNamespaceIsValid(t *testing.T) {
	identity := &PlatformIdentity{
		ClientProvisioning: ClientProvisioningKubauth,
		KubauthNamespace:   "kubauth",
	}

	if err := identity.Validate(); err != nil {
		t.Fatalf("expected a complete kubauth block to be valid, got %v", err)
	}
}

func TestDcrIsValid(t *testing.T) {
	identity := &PlatformIdentity{ClientProvisioning: ClientProvisioningDcr}
	if err := identity.Validate(); err != nil {
		t.Fatalf("expected dcr to be valid, got %v", err)
	}
}

// An unknown mode is a typo, not a fourth behaviour to guess at.
func TestUnknownModeIsRejected(t *testing.T) {
	for _, mode := range []string{"", "Existing", "kubauth-", "none"} {
		identity := &PlatformIdentity{ClientProvisioning: mode}
		if err := identity.Validate(); err == nil {
			t.Errorf("expected %q to be rejected", mode)
		}
	}
}
