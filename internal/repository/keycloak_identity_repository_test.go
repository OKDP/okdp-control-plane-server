package repository

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// newFakeKeycloak starts an httptest server answering the token endpoint
// plus the routes registered on the returned mux, and returns a config
// pointing at it. tokenCalls counts token requests (for 401-retry tests).
func newFakeKeycloak(t *testing.T, register func(mux *http.ServeMux)) (*config.Config, *int) {
	t.Helper()

	tokenCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/okdp/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token": "test-token",
			"expires_in":   300,
		})
	})
	if register != nil {
		register(mux)
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &config.Config{
		KeycloakURL:          srv.URL,
		KeycloakRealm:        "okdp",
		KeycloakClientID:     "okdp-control-plane",
		KeycloakClientSecret: "secret",
	}, &tokenCalls
}

func TestKeycloakGetUserMapsAttributes(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "jdoe", r.URL.Query().Get("username"))
			_ = json.NewEncoder(w).Encode([]kcUser{{
				ID:        "u1",
				Username:  "jdoe",
				FirstName: "John",
				LastName:  "Doe",
				Email:     "jdoe@example.com",
				Enabled:   false,
				Attributes: map[string][]string{
					"uid":     {"1234"},
					"comment": {"a comment"},
				},
			}})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)
	user, err := repo.GetUser(context.Background(), "jdoe")

	require.NoError(t, err)
	assert.Equal(t, "jdoe", user.Username)
	assert.Equal(t, "John Doe", user.Name)
	assert.Equal(t, []string{"jdoe@example.com"}, user.Email)
	assert.Equal(t, 1234, user.UID)
	assert.Equal(t, "a comment", user.Comment)
	assert.True(t, user.Disabled)
}

func TestKeycloakGetUserNotFound(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)
	_, err := repo.GetUser(context.Background(), "ghost")

	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "expected a NotFound error, got %v", err)
}

func TestKeycloakCreateUserSetsPassword(t *testing.T) {
	var createdUser kcUser
	var setCredential kcCredential

	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&createdUser))
				w.WriteHeader(http.StatusCreated)
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
			}
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1/reset-password", func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPut, r.Method)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&setCredential))
			w.WriteHeader(http.StatusNoContent)
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)
	err := repo.CreateUser(context.Background(), &models.User{
		Username: "jdoe",
		Name:     "John Doe",
		Email:    []string{"jdoe@example.com"},
		Password: "s3cret",
	})

	require.NoError(t, err)
	assert.Equal(t, "jdoe", createdUser.Username)
	assert.Equal(t, "John", createdUser.FirstName)
	assert.Equal(t, "Doe", createdUser.LastName)
	assert.Equal(t, "jdoe@example.com", createdUser.Email)
	assert.True(t, createdUser.Enabled)
	assert.Equal(t, "password", setCredential.Type)
	assert.Equal(t, "s3cret", setCredential.Value)
	assert.False(t, setCredential.Temporary)
}

func TestKeycloakListGroupBindingsForUser(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g1", Name: "team-a"}, {ID: "g2", Name: "team-b"}})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)
	bindings, err := repo.ListGroupBindings(context.Background(), "jdoe")

	require.NoError(t, err)
	require.Len(t, bindings, 2)
	assert.Equal(t, "jdoe", bindings[0].User)
	assert.Equal(t, "team-a", bindings[0].Group)
	assert.Equal(t, "team-b", bindings[1].Group)
}

func TestKeycloakGroupCRUDUsesGroupIDs(t *testing.T) {
	var updated kcGroup
	deleted := false

	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{
				ID:         "g1",
				Name:       "team-a",
				Attributes: map[string][]string{"comment": {"the team"}},
			}})
		})
		mux.HandleFunc("/admin/realms/okdp/groups/g1", func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPut:
				require.NoError(t, json.NewDecoder(r.Body).Decode(&updated))
			case http.MethodDelete:
				deleted = true
			}
			w.WriteHeader(http.StatusNoContent)
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)

	group, err := repo.GetGroup(context.Background(), "team-a")
	require.NoError(t, err)
	assert.Equal(t, "team-a", group.Name)
	assert.Equal(t, "the team", group.Comment)

	require.NoError(t, repo.UpdateGroup(context.Background(), "team-a", &models.Group{Name: "team-a", Comment: "updated"}))
	assert.Equal(t, "g1", updated.ID)
	assert.Equal(t, []string{"updated"}, updated.Attributes["comment"])

	require.NoError(t, repo.DeleteGroup(context.Background(), "team-a"))
	assert.True(t, deleted)
}

func TestKeycloakRetriesOnceOn401(t *testing.T) {
	adminCalls := 0
	cfg, tokenCalls := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			adminCalls++
			if adminCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode([]kcUser{})
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)
	users, err := repo.ListUsers(context.Background())

	require.NoError(t, err)
	assert.Empty(t, users)
	assert.Equal(t, 2, adminCalls, "the 401 should be retried once")
	assert.Equal(t, 2, *tokenCalls, "a fresh token should be fetched for the retry")
}

// Without a client secret there is nothing to authenticate with: the API is
// reported absent rather than failing on every call.
func TestKeycloakUnavailableWithoutCredentials(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, nil)
	cfg.KeycloakClientSecret = ""

	repo := NewKeycloakIdentityRepository(cfg, nil)

	assert.False(t, repo.Available(context.Background()))
	_, err := repo.ListUsers(context.Background())
	assert.ErrorIs(t, err, errKeycloakNotConfigured)
}

// KEYCLOAK_URL and KEYCLOAK_REALM unset: the realm is the platform issuer's,
// <url>/realms/<realm>, so the chart only has to provide the credentials.
func TestKeycloakRealmFollowsThePlatformIssuer(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g1", Name: "team-a"}})
		})
	})
	base := cfg.KeycloakURL
	cfg.KeycloakURL, cfg.KeycloakRealm = "", ""

	repo := NewKeycloakIdentityRepository(cfg, func(context.Context) (string, bool, error) {
		return base + "/realms/okdp/", false, nil
	})

	require.True(t, repo.Available(context.Background()))
	groups, err := repo.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "team-a", groups[0].Name)
}

// An issuer that is not a Keycloak realm cannot locate the Admin API.
func TestKeycloakUnavailableWhenTheIssuerIsNotARealm(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, nil)
	cfg.KeycloakURL, cfg.KeycloakRealm = "", ""

	for _, issuer := range []string{"", "https://idp.example", "https://idp.example/realms/"} {
		repo := NewKeycloakIdentityRepository(cfg, func(context.Context) (string, bool, error) {
			return issuer, false, nil
		})
		assert.False(t, repo.Available(context.Background()), "issuer %q", issuer)
	}
	assert.False(t, NewKeycloakIdentityRepository(cfg, nil).Available(context.Background()))
}

// Removing a user from a group goes through the membership, keyed by ids.
func TestKeycloakDeleteGroupBindingUsesIDs(t *testing.T) {
	removed := false
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/admin/realms/okdp/users", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]kcUser{{ID: "u1", Username: "jdoe"}})
		})
		mux.HandleFunc("/admin/realms/okdp/groups", func(w http.ResponseWriter, r *http.Request) {
			// Keycloak's search is fuzzy: the exact name must be picked.
			_ = json.NewEncoder(w).Encode([]kcGroup{{ID: "g0", Name: "team-a-ops"}, {ID: "g1", Name: "team-a"}})
		})
		mux.HandleFunc("/admin/realms/okdp/users/u1/groups/g1", func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodDelete, r.Method)
			removed = true
			w.WriteHeader(http.StatusNoContent)
		})
	})

	repo := NewKeycloakIdentityRepository(cfg, nil)
	require.NoError(t, repo.DeleteGroupBindingByRef(context.Background(), "jdoe", "team-a"))
	assert.True(t, removed)
}

// An empty realm lists as [] in JSON, not null: the console iterates the answer.
func TestKeycloakEmptyListsAreEmptyNotNil(t *testing.T) {
	cfg, _ := newFakeKeycloak(t, func(mux *http.ServeMux) {
		for _, route := range []string{"/admin/realms/okdp/users", "/admin/realms/okdp/groups"} {
			mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("[]"))
			})
		}
	})
	repo := NewKeycloakIdentityRepository(cfg, nil)

	users, err := repo.ListUsers(context.Background())
	require.NoError(t, err)
	groups, err := repo.ListGroups(context.Background())
	require.NoError(t, err)

	for name, list := range map[string]any{"users": users, "groups": groups} {
		data, err := json.Marshal(list)
		require.NoError(t, err)
		assert.Equal(t, "[]", string(data), name)
	}
}
