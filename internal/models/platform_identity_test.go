package models

import "testing"

func TestExistingNeedsNothingElse(t *testing.T) {
	identity := &PlatformIdentity{ClientProvisioning: ClientProvisioningExisting}

	if err := identity.Validate(); err != nil {
		t.Fatalf("expected the existing mode to be self-sufficient, got %v", err)
	}
}

func TestDcrIsValid(t *testing.T) {
	identity := &PlatformIdentity{ClientProvisioning: ClientProvisioningDcr}
	if err := identity.Validate(); err != nil {
		t.Fatalf("expected dcr to be valid, got %v", err)
	}
}

// An unknown mode is a typo (or a mode OKDP no longer supports), not a third
// behaviour to guess at.
func TestUnknownModeIsRejected(t *testing.T) {
	for _, mode := range []string{"", "Existing", "dcr-", "none", "oidcclient"} {
		identity := &PlatformIdentity{ClientProvisioning: mode}
		if err := identity.Validate(); err == nil {
			t.Errorf("expected %q to be rejected", mode)
		}
	}
}
