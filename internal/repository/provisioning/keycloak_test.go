package provisioning

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeKeycloak simulates the two Keycloak surfaces the adapter touches: the
// realm token endpoint and the Admin API clients resource.
type fakeKeycloak struct {
	server *httptest.Server
	// clients indexed by internal uuid
	clients map[string]map[string]any

	tokenRequests int
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	f := &fakeKeycloak{clients: map[string]map[string]any{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/test/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenRequests++
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") != "okdp-provisioner" || r.Form.Get("client_secret") != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
	})

	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux.HandleFunc("GET /admin/realms/test/clients", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		filter := r.URL.Query().Get("clientId")
		var out []map[string]any
		for _, c := range f.clients {
			if filter == "" || c["clientId"] == filter {
				out = append(out, c)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /admin/realms/test/clients", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		var c map[string]any
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		uuid := "uuid-" + c["clientId"].(string)
		c["id"] = uuid
		f.clients[uuid] = c
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("PUT /admin/realms/test/clients/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		uuid := r.PathValue("uuid")
		if _, ok := f.clients[uuid]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var c map[string]any
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.clients[uuid] = c
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /admin/realms/test/clients/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		uuid := r.PathValue("uuid")
		if _, ok := f.clients[uuid]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.clients, uuid)
		w.WriteHeader(http.StatusNoContent)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeKeycloak) provisioner() *keycloakProvisioner {
	cfg := &KeycloakConfig{IssuerURI: f.server.URL + "/realms/test"}
	return &keycloakProvisioner{
		cfgFn: func(ctx context.Context) (*KeycloakConfig, error) { return cfg, nil },
		credsFn: func(ctx context.Context, cfg *KeycloakConfig) (string, string, error) {
			return "okdp-provisioner", "s3cret", nil
		},
	}
}

func TestKeycloakEnsureClientCreates(t *testing.T) {
	kc := newFakeKeycloak(t)
	p := kc.provisioner()

	spec := OidcClientSpec{Name: "demo-superset", RedirectURIs: []string{"https://superset-demo.okdp.sandbox/*"}}
	if err := p.EnsureClient(context.Background(), spec); err != nil {
		t.Fatalf("EnsureClient: %v", err)
	}

	created, ok := kc.clients["uuid-demo-superset"]
	if !ok {
		t.Fatalf("client was not created: %v", kc.clients)
	}
	uris, _ := created["redirectUris"].([]any)
	if len(uris) != 1 || uris[0] != "https://superset-demo.okdp.sandbox/*" {
		t.Fatalf("unexpected redirectUris: %v", created["redirectUris"])
	}
}

func TestKeycloakEnsureClientUpdatesExisting(t *testing.T) {
	kc := newFakeKeycloak(t)
	kc.clients["uuid-demo-superset"] = map[string]any{
		"id":           "uuid-demo-superset",
		"clientId":     "demo-superset",
		"enabled":      true,
		"redirectUris": []any{"https://old.example.com/*"},
	}
	p := kc.provisioner()

	spec := OidcClientSpec{Name: "demo-superset", RedirectURIs: []string{"https://new.example.com/*"}}
	if err := p.EnsureClient(context.Background(), spec); err != nil {
		t.Fatalf("EnsureClient: %v", err)
	}

	uris, _ := kc.clients["uuid-demo-superset"]["redirectUris"].([]any)
	if len(uris) != 1 || uris[0] != "https://new.example.com/*" {
		t.Fatalf("redirectUris not updated: %v", uris)
	}
	if len(kc.clients) != 1 {
		t.Fatalf("expected a single client, got %d", len(kc.clients))
	}
}

func TestKeycloakDeleteClient(t *testing.T) {
	kc := newFakeKeycloak(t)
	kc.clients["uuid-demo-superset"] = map[string]any{
		"id":       "uuid-demo-superset",
		"clientId": "demo-superset",
	}
	p := kc.provisioner()

	if err := p.DeleteClient(context.Background(), "demo-superset"); err != nil {
		t.Fatalf("DeleteClient: %v", err)
	}
	if len(kc.clients) != 0 {
		t.Fatalf("client not deleted: %v", kc.clients)
	}
}

func TestKeycloakDeleteClientIsIdempotent(t *testing.T) {
	kc := newFakeKeycloak(t)
	p := kc.provisioner()

	if err := p.DeleteClient(context.Background(), "does-not-exist"); err != nil {
		t.Fatalf("DeleteClient on absent client should be a no-op, got: %v", err)
	}
}

func TestAdminBaseURL(t *testing.T) {
	base, err := adminBaseURL("https://keycloak.okdp.sandbox/realms/master")
	if err != nil {
		t.Fatalf("adminBaseURL: %v", err)
	}
	if base != "https://keycloak.okdp.sandbox/admin/realms/master" {
		t.Fatalf("unexpected admin base: %s", base)
	}

	if _, err := adminBaseURL("https://keycloak.okdp.sandbox"); err == nil {
		t.Fatal("expected an error for an issuer without /realms/")
	}
}
