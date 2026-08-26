package crd

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const testPEM = `-----BEGIN CERTIFICATE-----
MIIBkTCB+wIJAOxfSMkzMd+FMA0GCSqGSIb3DQEBCwUAMBExDzANBgNVBAMMBnRl
-----END CERTIFICATE-----
`

// The SecretStore CRD declares caBundle as `format: byte`, so the API server
// rejects anything that is not base64. Marshalling has to encode it; a plain
// string field would ship raw PEM and every store carrying a private CA would
// fail on create with "must be of type byte".
func TestVaultProviderMarshalsCABundleAsBase64(t *testing.T) {
	out, err := json.Marshal(ESOVaultProvider{
		Server:   "https://vault.example.com",
		Path:     "secret",
		Version:  "v2",
		CABundle: []byte(testPEM),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	encoded, ok := got["caBundle"].(string)
	if !ok {
		t.Fatalf("caBundle is %T, want string", got["caBundle"])
	}
	if strings.Contains(encoded, "BEGIN CERTIFICATE") {
		t.Fatal("caBundle shipped as raw PEM, the API server would reject it")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("caBundle is not valid base64: %v", err)
	}
	if string(decoded) != testPEM {
		t.Fatalf("round trip changed the PEM:\ngot  %q\nwant %q", decoded, testPEM)
	}
}

// An empty bundle must stay absent, so stores that rely on the system roots do
// not carry an empty caBundle the API server would have to validate.
func TestVaultProviderOmitsEmptyCABundle(t *testing.T) {
	out, err := json.Marshal(ESOVaultProvider{
		Server:  "https://vault.example.com",
		Path:    "secret",
		Version: "v2",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "caBundle") {
		t.Fatalf("empty caBundle was emitted: %s", out)
	}
}
