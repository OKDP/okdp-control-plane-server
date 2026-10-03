package crd

import "testing"

// ESO 2.x serves external-secrets.io/v1 only. A client still on v1beta1 finds
// no CRD in discovery and the secret routes answer 501 on a cluster that has
// ESO installed.
func TestESOResourcesUseTheServedVersion(t *testing.T) {
	for _, tc := range []struct {
		name, apiVersion, gvrVersion string
	}{
		{"SecretStore", SecretStoreAPIVersion, GetSecretStoreGVR().Version},
		{"ExternalSecret", ExternalSecretAPIVersion, GetExternalSecretGVR().Version},
	} {
		if tc.apiVersion != "external-secrets.io/v1" {
			t.Errorf("%s apiVersion = %q, want external-secrets.io/v1", tc.name, tc.apiVersion)
		}
		if tc.gvrVersion != "v1" {
			t.Errorf("%s GVR version = %q, want v1", tc.name, tc.gvrVersion)
		}
	}
}
