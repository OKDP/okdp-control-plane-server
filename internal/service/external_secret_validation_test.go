package service

import (
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func importRequest(mutate func(*models.ExternalSecretRequest)) models.ExternalSecretRequest {
	req := models.ExternalSecretRequest{
		Name:            "mon-import",
		SecretStoreRef:  "store",
		RefreshInterval: "1m",
		Target:          models.ExternalSecretTarget{Name: "mon-secret"},
		Data: []models.ExternalSecretDataEntry{
			{SecretKey: "pwd", RemoteRef: models.ExternalSecretRemote{Key: "client-externe", Property: "db_password"}},
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
		"nom absent":              func(r *models.ExternalSecretRequest) { r.Name = "" },
		"store absent":            func(r *models.ExternalSecretRequest) { r.SecretStoreRef = "" },
		"cible absente":           func(r *models.ExternalSecretRequest) { r.Target.Name = "" },
		"intervalle absent":       func(r *models.ExternalSecretRequest) { r.RefreshInterval = "" },
		"aucune donnee":           func(r *models.ExternalSecretRequest) { r.Data = nil },
		"cle applicative absente": func(r *models.ExternalSecretRequest) { r.Data[0].SecretKey = "" },
		"cle distante absente":    func(r *models.ExternalSecretRequest) { r.Data[0].RemoteRef.Key = "" },
	}

	for name, mutate := range cases {
		err := validateExternalSecretRequest(importRequest(mutate))
		if err == nil {
			t.Fatalf("%s: accepte", name)
		}
		if !IsValidationError(err) {
			t.Fatalf("%s: rapporte comme une panne serveur, pas comme une saisie fautive: %v", name, err)
		}
	}
}

// The ESO admission webhook answers a raw Go parser message under a status the
// handler cannot tell from a platform failure, so the duration is parsed here.
func TestRefreshIntervalMustBeADuration(t *testing.T) {
	for _, v := range []string{"abc", "1", "5 minutes", "-", "1mn"} {
		err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.RefreshInterval = v }))
		if err == nil {
			t.Fatalf("refreshInterval %q accepte", v)
		}
		if !IsValidationError(err) || !strings.Contains(err.Error(), "duration") {
			t.Fatalf("refreshInterval %q: message inutilisable: %v", v, err)
		}
	}
	for _, v := range []string{"1m", "30s", "1h30m", "10m0s"} {
		if err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.RefreshInterval = v })); err != nil {
			t.Fatalf("refreshInterval %q rejete: %v", v, err)
		}
	}
}

// A name the API server will reject must be named here, where the message can
// say which field is wrong, instead of coming back as a schema error.
func TestNamesMustBeValidKubernetesNames(t *testing.T) {
	for _, v := range []string{"Mon_Import", "mon import", "-debut", "fin-", "MAJUSCULES"} {
		if err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.Name = v })); !IsValidationError(err) {
			t.Fatalf("nom %q accepte ou mal rapporte: %v", v, err)
		}
		if err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) { r.Target.Name = v })); !IsValidationError(err) {
			t.Fatalf("cible %q acceptee ou mal rapportee: %v", v, err)
		}
	}
}

// Two mappings writing the same key keep one value and drop the other, and the
// resulting Secret says nothing about which one survived.
func TestDuplicateSecretKeyIsRefused(t *testing.T) {
	err := validateExternalSecretRequest(importRequest(func(r *models.ExternalSecretRequest) {
		r.Data = append(r.Data, models.ExternalSecretDataEntry{
			SecretKey: "pwd",
			RemoteRef: models.ExternalSecretRemote{Key: "autre", Property: "p"},
		})
	}))
	if !IsValidationError(err) || !strings.Contains(err.Error(), "already mapped") {
		t.Fatalf("doublon de cle accepte ou mal rapporte: %v", err)
	}
}
