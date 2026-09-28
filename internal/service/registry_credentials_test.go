package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".dockerconfigjson")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRegistryCredentialForAuthField(t *testing.T) {
	// "YWxpY2U6czNjcjN0" is base64("alice:s3cr3t")
	path := writeAuthFile(t, `{"auths":{"ghcr.io":{"auth":"YWxpY2U6czNjcjN0"}}}`)

	cred, err := registryCredentialFor(path, "ghcr.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred == nil || cred.username != "alice" || cred.password != "s3cr3t" {
		t.Fatalf("cred = %+v", cred)
	}
}

func TestRegistryCredentialForUsernamePasswordAndURLKey(t *testing.T) {
	path := writeAuthFile(t, `{"auths":{"https://quay.io/v1/":{"username":"robot","password":"tok"}}}`)

	cred, err := registryCredentialFor(path, "quay.io")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred == nil || cred.username != "robot" || cred.password != "tok" {
		t.Fatalf("cred = %+v", cred)
	}
}

func TestRegistryCredentialForUnknownHost(t *testing.T) {
	path := writeAuthFile(t, `{"auths":{"ghcr.io":{"auth":"YWxpY2U6czNjcjN0"}}}`)

	cred, err := registryCredentialFor(path, "quay.io")
	if err != nil || cred != nil {
		t.Fatalf("cred = %+v, err = %v, want nil, nil", cred, err)
	}
}

func TestRegistryCredentialForMissingFile(t *testing.T) {
	if _, err := registryCredentialFor(filepath.Join(t.TempDir(), "absent"), "ghcr.io"); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestKubocdCredentialEnv(t *testing.T) {
	env := kubocdCredentialEnv("localhost:5000", &registryCredential{username: "u", password: "p"})
	want := []string{"KCD_OCI_LOCALHOST_5000_USER=u", "KCD_OCI_LOCALHOST_5000_SECRET=p"}
	if !slices.Equal(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

// privateBearerRegistry grants a pull token only to alice:s3cr3t, as ghcr.io
// does for a private repository.
func privateBearerRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if user, pass, ok := r.BasicAuth(); !ok || user != "alice" || pass != "s3cr3t" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"token":"granted"}`))
		default:
			if r.Header.Get("Authorization") != "Bearer granted" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:org/pkg:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"tags":["1.0.0"]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRegistryGetBearerWithCredential(t *testing.T) {
	srv := privateBearerRegistry(t)

	resp, err := registryGet(srv.URL+"/v2/org/pkg/tags/list", &registryCredential{username: "alice", password: "s3cr3t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestRegistryGetBearerWithoutCredential(t *testing.T) {
	srv := privateBearerRegistry(t)

	if _, err := registryGet(srv.URL+"/v2/org/pkg/tags/list", nil); err == nil {
		t.Fatal("expected an error on a private repository without credentials")
	}
}

func TestRegistryGetBasicChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "alice" || pass != "s3cr3t" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"tags":["1.0.0"]}`))
	}))
	defer srv.Close()

	resp, err := registryGet(srv.URL+"/v2/org/pkg/tags/list", &registryCredential{username: "alice", password: "s3cr3t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if _, err := registryGet(srv.URL+"/v2/org/pkg/tags/list", nil); err == nil {
		t.Fatal("expected an error on a Basic challenge without credentials")
	}
}
