package service

import (
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func importRequest(mutate func(*models.ExternalSecretRequest)) models.ExternalSecretRequest {
	req := models.ExternalSecretRequest{
		Name:            "my-import",
		SecretStoreRef:  "store",
		RefreshInterval: "1m",
		Target:          models.ExternalSecretTarget{Name: "my-secret"},
		Data: []models.ExternalSecretDataEntry{
			{SecretKey: "pwd", RemoteRef: models.ExternalSecretRemote{Key: "external-client", Property: "db_password"}},
		},
	}
	if mutate != nil {
		mutate(&req)
	}
	return req
}

func TestValidImportIsAccepted(t *testing.T) {
	if err := validateExternalSecretRequest(importRequest(nil)); err != nil {
		t.Fatalf("a valid import was rejected: %v", err)
	}
}

// Every refusal here is the caller's to fix. Returned as a plain error they all
// reached the handler as a platform failure and went out as 500, so the console
// could only report a server fault when the fix was to correct a field.
func TestEveryRefusalIsABadRequest(t *testing.T) {
	cases := map[string]func(*models.ExternalSecretRequest){
		"missing name":        func(r *models.ExternalSecretRequest) { r.Name = "" },
		"missing store":       func(r *models.ExternalSecretRequest) { r.SecretStoreRef = "" },
		"missing target":      func(r *models.ExternalSecretRequest) { r.Target.Name = "" },
		"missing interval":    func(r *models.ExternalSecretRequest) { r.RefreshInterval = "" },
		"no data mapping":     func(r *models.ExternalSecretRequest) { r.Data = nil },
		"missing secret key":  func(r *models.ExternalSecretRequest) { r.Data[0].SecretKey = "" },
		"missing remote key":  func(r *models.ExternalSecretRequest) { r.Data[0].RemoteRef.Key = "" },
		"invalid secret key":  func(r *models.ExternalSecretRequest) { r.Data[0].SecretKey = "not a key!" },
		"negative interval":   func(r *models.ExternalSecretRequest) { r.RefreshInterval = "-1h" },
		"unparsable interval": func(r *models.ExternalSecretRequest) { r.RefreshInterval = "abc" },
	}

	for name, mutate := range cases {
		err := validateExternalSecretRequest(importRequest(mutate))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !IsValidationError(err) {
			t.Fatalf("%s: reported as a platform failure, not as a rejected input: %v", name, err)
		}
	}
}

// The ESO admission webhook answers a raw Go parser message under a status the
// handler cannot tell from a platform failure, so the duration is parsed here.
func TestRefreshIntervalMustBeADuration(t *testing.T) {
	for _, v := range []string{"abc", "1", "5 minutes", "-", "1mn"} {
		err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.RefreshInterval = v }))
		if err == nil {
			t.Fatalf("refreshInterval %q was accepted", v)
		}
		if !IsValidationError(err) || !strings.Contains(err.Error(), "duration") {
			t.Fatalf("refreshInterval %q: unusable message: %v", v, err)
		}
	}
	// Zero is legal: external-secrets reads it as "do not refresh".
	for _, v := range []string{"1m", "30s", "1h30m", "10m0s", "0"} {
		if err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.RefreshInterval = v })); err != nil {
			t.Fatalf("refreshInterval %q was rejected: %v", v, err)
		}
	}
}

// ParseDuration accepts a negative duration, so parsing alone let "-1h" through
// the check that claims to validate the interval.
func TestRefreshIntervalRejectsANegativeDuration(t *testing.T) {
	for _, v := range []string{"-1h", "-30s", "-1h30m"} {
		err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.RefreshInterval = v }))
		if !IsValidationError(err) {
			t.Fatalf("refreshInterval %q was accepted or misreported: %v", v, err)
		}
		if !strings.Contains(err.Error(), "negative") {
			t.Fatalf("refreshInterval %q: the message does not name the problem: %v", v, err)
		}
	}
}

// A name the API server will reject must be named here, where the message can
// say which field is wrong, instead of coming back as a schema error.
func TestNamesMustBeValidKubernetesNames(t *testing.T) {
	for _, v := range []string{"My_Import", "my import", "-leading", "trailing-", "UPPERCASE"} {
		if err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.Name = v })); !IsValidationError(err) {
			t.Fatalf("name %q was accepted or misreported: %v", v, err)
		}
		if err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.Target.Name = v })); !IsValidationError(err) {
			t.Fatalf("target %q was accepted or misreported: %v", v, err)
		}
	}
}

// Two mappings writing the same key keep one value and drop the other, and the
// resulting Secret says nothing about which one survived.
func TestDuplicateSecretKeyIsRefused(t *testing.T) {
	err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) {
		r.Data = append(r.Data, models.ExternalSecretDataEntry{
			SecretKey: "pwd",
			RemoteRef: models.ExternalSecretRemote{Key: "other", Property: "p"},
		})
	}))
	if !IsValidationError(err) || !strings.Contains(err.Error(), "already mapped") {
		t.Fatalf("a duplicate key was accepted or misreported: %v", err)
	}
}
