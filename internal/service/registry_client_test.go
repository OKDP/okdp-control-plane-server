package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A registry whose challenge sends the token request elsewhere: the other
// server must never be called.
func TestRegistryGetRefusesARealmOnAnotherHost(t *testing.T) {
	var hit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
		fmt.Fprint(w, `{"token":"t"}`)
	}))
	defer elsewhere.Close()
	// Same address, other host name: localhost is not 127.0.0.1.
	realm := strings.Replace(elsewhere.URL, "127.0.0.1", "localhost", 1) + "/token"

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`",service="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "http://")

	_, err := registryGet(registry.URL+"/v2/okdp/hive/tags/list", []string{host})
	if err == nil || !strings.Contains(err.Error(), "not on the registry host") {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Fatal("the token request went to the other host")
	}
}

// The anonymous flow still works for a realm on the registry itself, over
// plain HTTP only because the registry is declared insecure.
func TestRegistryGetFollowsARealmOnTheRegistryHost(t *testing.T) {
	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:okdp/hive:pull" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"token":"anon"}`)
		case r.Header.Get("Authorization") == "Bearer anon":
			fmt.Fprint(w, `{"tags":["1.0.0"]}`)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+registry.URL+`/token",scope="repository:okdp/hive:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer registry.Close()
	host := strings.TrimPrefix(registry.URL, "http://")

	resp, err := registryGet(registry.URL+"/v2/okdp/hive/tags/list", []string{host})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// Not declared insecure: a plain-HTTP realm is refused.
	if _, err := registryGet(registry.URL+"/v2/okdp/hive/tags/list", nil); err == nil || !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("plain-HTTP realm of a secure registry: err = %v", err)
	}
}

func TestRegistryGetDoesNotFollowARedirectToAnotherHost(t *testing.T) {
	var hit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit.Store(true)
	}))
	defer elsewhere.Close()
	target := strings.Replace(elsewhere.URL, "127.0.0.1", "localhost", 1)

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+"/internal", http.StatusFound)
	}))
	defer registry.Close()

	if _, err := registryGet(registry.URL+"/v2/okdp/hive/tags/list", nil); err == nil || !strings.Contains(err.Error(), "refusing a redirect") {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Fatal("the redirect was followed to another host")
	}
}
