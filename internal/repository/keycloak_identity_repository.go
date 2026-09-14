package repository

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/config"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// keycloakIdentityRepository implements IdentityRepository against the
// Keycloak Admin REST API. Users map to Keycloak users; Groups map to
// top-level Keycloak groups (exposed in tokens through the realm `groups`
// client scope); GroupBindings map to group memberships.
type keycloakIdentityRepository struct {
	kc *keycloakClient
}

// Resources used to surface backend lookups as Kubernetes-style NotFound
// errors, so handlers keep relying on apierrors.IsNotFound.
var (
	userResource  = schema.GroupResource{Group: "identity.okdp.io", Resource: "users"}
	groupResource = schema.GroupResource{Group: "identity.okdp.io", Resource: "groups"}
)

// listPageSize bounds admin list calls. The identity admin console operates
// on platform users; paging beyond this is out of scope for now.
const listPageSize = 1000

// NewKeycloakIdentityRepository manages users and groups in the Keycloak realm
// named by cfg (KEYCLOAK_*); issuer locates the realm when KEYCLOAK_URL or
// KEYCLOAK_REALM is unset, and may be nil.
func NewKeycloakIdentityRepository(cfg *config.Config, issuer KeycloakIssuerSource) IdentityRepository {
	return &keycloakIdentityRepository{kc: newKeycloakClient(cfg, issuer)}
}

// Available reports whether the admin client has credentials and a realm.
func (r *keycloakIdentityRepository) Available(ctx context.Context) bool {
	return r.kc.configured(ctx)
}

// --- Keycloak API representations ---

type kcUser struct {
	ID         string              `json:"id,omitempty"`
	Username   string              `json:"username"`
	FirstName  string              `json:"firstName,omitempty"`
	LastName   string              `json:"lastName,omitempty"`
	Email      string              `json:"email,omitempty"`
	Enabled    bool                `json:"enabled"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type kcGroup struct {
	ID         string              `json:"id,omitempty"`
	Name       string              `json:"name"`
	Path       string              `json:"path,omitempty"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type kcCredential struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Temporary bool   `json:"temporary"`
}

// --- Mapping helpers ---

func attr(attrs map[string][]string, key string) string {
	if vals, ok := attrs[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func toModelUser(u *kcUser) models.User {
	displayName := strings.TrimSpace(strings.TrimSpace(u.FirstName) + " " + strings.TrimSpace(u.LastName))

	var emails []string
	if u.Email != "" {
		emails = []string{u.Email}
	}

	uid := 0
	if rawUID := attr(u.Attributes, "uid"); rawUID != "" {
		if parsed, err := strconv.Atoi(rawUID); err == nil {
			uid = parsed
		}
	}

	return models.User{
		Username: u.Username,
		Name:     displayName,
		Email:    emails,
		Comment:  attr(u.Attributes, "comment"),
		UID:      uid,
		Disabled: !u.Enabled,
	}
}

func toKcUser(username string, user *models.User) *kcUser {
	// The display name is stored in firstName/lastName, split on the last space.
	firstName := strings.TrimSpace(user.Name)
	lastName := ""
	if idx := strings.LastIndex(firstName, " "); idx > 0 {
		lastName = firstName[idx+1:]
		firstName = firstName[:idx]
	}

	email := ""
	if len(user.Email) > 0 {
		email = user.Email[0]
	}

	attrs := map[string][]string{}
	if user.Comment != "" {
		attrs["comment"] = []string{user.Comment}
	}
	if user.UID > 0 {
		attrs["uid"] = []string{strconv.Itoa(user.UID)}
	}

	return &kcUser{
		Username:   username,
		FirstName:  firstName,
		LastName:   lastName,
		Email:      email,
		Enabled:    !user.Disabled,
		Attributes: attrs,
	}
}

func toModelGroup(g *kcGroup) models.Group {
	comment := attr(g.Attributes, "comment")
	return models.Group{
		Name:        g.Name,
		Comment:     comment,
		Description: comment,
	}
}

func toKcGroup(name string, group *models.Group) *kcGroup {
	comment := group.Comment
	if comment == "" {
		comment = group.Description
	}

	attrs := map[string][]string{}
	if comment != "" {
		attrs["comment"] = []string{comment}
	}

	return &kcGroup{
		Name:       name,
		Attributes: attrs,
	}
}

func (r *keycloakIdentityRepository) findUser(ctx context.Context, username string) (*kcUser, error) {
	var users []kcUser
	path := fmt.Sprintf("/users?username=%s&exact=true&briefRepresentation=false", url.QueryEscape(username))
	if err := r.kc.doAdmin(ctx, http.MethodGet, path, nil, &users); err != nil {
		return nil, err
	}
	for i := range users {
		if strings.EqualFold(users[i].Username, username) {
			return &users[i], nil
		}
	}
	return nil, apierrors.NewNotFound(userResource, username)
}

// findGroup resolves a top-level group by name. Keycloak's group search is
// fuzzy, so the exact match is applied client-side.
func (r *keycloakIdentityRepository) findGroup(ctx context.Context, name string) (*kcGroup, error) {
	var groups []kcGroup
	path := fmt.Sprintf("/groups?search=%s&briefRepresentation=false&max=%d", url.QueryEscape(name), listPageSize)
	if err := r.kc.doAdmin(ctx, http.MethodGet, path, nil, &groups); err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].Name == name {
			return &groups[i], nil
		}
	}
	return nil, apierrors.NewNotFound(groupResource, name)
}

func (r *keycloakIdentityRepository) setPassword(ctx context.Context, userID, password string) error {
	cred := kcCredential{Type: "password", Value: password, Temporary: false}
	return r.kc.doAdmin(ctx, http.MethodPut, fmt.Sprintf("/users/%s/reset-password", userID), cred, nil)
}

// --- Users ---

func (r *keycloakIdentityRepository) ListUsers(ctx context.Context) ([]models.User, error) {
	var kcUsers []kcUser
	path := fmt.Sprintf("/users?max=%d&briefRepresentation=false", listPageSize)
	if err := r.kc.doAdmin(ctx, http.MethodGet, path, nil, &kcUsers); err != nil {
		return nil, err
	}

	users := make([]models.User, 0, len(kcUsers))
	for i := range kcUsers {
		users = append(users, toModelUser(&kcUsers[i]))
	}
	return users, nil
}

func (r *keycloakIdentityRepository) GetUser(ctx context.Context, name string) (*models.User, error) {
	kcU, err := r.findUser(ctx, name)
	if err != nil {
		return nil, err
	}
	user := toModelUser(kcU)
	return &user, nil
}

func (r *keycloakIdentityRepository) CreateUser(ctx context.Context, user *models.User) error {
	username := user.Username
	if username == "" {
		username = user.Name
	}

	kcU := toKcUser(username, user)
	if err := r.kc.doAdmin(ctx, http.MethodPost, "/users", kcU, nil); err != nil {
		return err
	}

	if user.Password != "" {
		created, err := r.findUser(ctx, username)
		if err != nil {
			return fmt.Errorf("user created but lookup for password setup failed: %w", err)
		}
		if err := r.setPassword(ctx, created.ID, user.Password); err != nil {
			return fmt.Errorf("user created but failed to set password: %w", err)
		}
	}
	return nil
}

func (r *keycloakIdentityRepository) UpdateUser(ctx context.Context, name string, user *models.User) error {
	existing, err := r.findUser(ctx, name)
	if err != nil {
		return err
	}

	kcU := toKcUser(name, user)
	kcU.ID = existing.ID
	if err := r.kc.doAdmin(ctx, http.MethodPut, "/users/"+existing.ID, kcU, nil); err != nil {
		return err
	}

	if user.Password != "" {
		if err := r.setPassword(ctx, existing.ID, user.Password); err != nil {
			return fmt.Errorf("user updated but failed to set password: %w", err)
		}
	}
	return nil
}

func (r *keycloakIdentityRepository) DeleteUser(ctx context.Context, name string) error {
	existing, err := r.findUser(ctx, name)
	if err != nil {
		return err
	}
	// Keycloak cascades group memberships on user deletion.
	return r.kc.doAdmin(ctx, http.MethodDelete, "/users/"+existing.ID, nil, nil)
}

// --- Groups ---

func (r *keycloakIdentityRepository) ListGroups(ctx context.Context) ([]models.Group, error) {
	var kcGroups []kcGroup
	path := fmt.Sprintf("/groups?max=%d&briefRepresentation=false", listPageSize)
	if err := r.kc.doAdmin(ctx, http.MethodGet, path, nil, &kcGroups); err != nil {
		return nil, err
	}

	groups := make([]models.Group, 0, len(kcGroups))
	for i := range kcGroups {
		groups = append(groups, toModelGroup(&kcGroups[i]))
	}
	return groups, nil
}

func (r *keycloakIdentityRepository) GetGroup(ctx context.Context, name string) (*models.Group, error) {
	kcG, err := r.findGroup(ctx, name)
	if err != nil {
		return nil, err
	}
	group := toModelGroup(kcG)
	return &group, nil
}

func (r *keycloakIdentityRepository) CreateGroup(ctx context.Context, group *models.Group) error {
	return r.kc.doAdmin(ctx, http.MethodPost, "/groups", toKcGroup(group.Name, group), nil)
}

func (r *keycloakIdentityRepository) UpdateGroup(ctx context.Context, name string, group *models.Group) error {
	existing, err := r.findGroup(ctx, name)
	if err != nil {
		return err
	}

	kcG := toKcGroup(name, group)
	kcG.ID = existing.ID
	return r.kc.doAdmin(ctx, http.MethodPut, "/groups/"+existing.ID, kcG, nil)
}

func (r *keycloakIdentityRepository) DeleteGroup(ctx context.Context, name string) error {
	existing, err := r.findGroup(ctx, name)
	if err != nil {
		return err
	}
	return r.kc.doAdmin(ctx, http.MethodDelete, "/groups/"+existing.ID, nil, nil)
}

// --- GroupBindings (group memberships) ---

func (r *keycloakIdentityRepository) userGroups(ctx context.Context, userID string) ([]kcGroup, error) {
	var groups []kcGroup
	path := fmt.Sprintf("/users/%s/groups?max=%d", userID, listPageSize)
	if err := r.kc.doAdmin(ctx, http.MethodGet, path, nil, &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

func (r *keycloakIdentityRepository) ListGroupBindings(ctx context.Context, userFilter string) ([]models.GroupBinding, error) {
	var kcUsers []kcUser
	if userFilter != "" {
		u, err := r.findUser(ctx, userFilter)
		if err != nil {
			return nil, err
		}
		kcUsers = []kcUser{*u}
	} else {
		path := fmt.Sprintf("/users?max=%d", listPageSize)
		if err := r.kc.doAdmin(ctx, http.MethodGet, path, nil, &kcUsers); err != nil {
			return nil, err
		}
	}

	var bindings []models.GroupBinding
	for i := range kcUsers {
		groups, err := r.userGroups(ctx, kcUsers[i].ID)
		if err != nil {
			logrus.WithError(err).WithField("user", kcUsers[i].Username).Warn("Failed to list keycloak group memberships")
			continue
		}
		for _, g := range groups {
			bindings = append(bindings, models.GroupBinding{
				User:  kcUsers[i].Username,
				Group: g.Name,
			})
		}
	}
	return bindings, nil
}

func (r *keycloakIdentityRepository) CreateGroupBinding(ctx context.Context, user, group string) error {
	kcU, err := r.findUser(ctx, user)
	if err != nil {
		return err
	}
	kcG, err := r.findGroup(ctx, group)
	if err != nil {
		return err
	}
	return r.kc.doAdmin(ctx, http.MethodPut, fmt.Sprintf("/users/%s/groups/%s", kcU.ID, kcG.ID), nil, nil)
}

func (r *keycloakIdentityRepository) DeleteGroupBindingByRef(ctx context.Context, user, group string) error {
	kcU, err := r.findUser(ctx, user)
	if err != nil {
		return err
	}
	kcG, err := r.findGroup(ctx, group)
	if err != nil {
		return err
	}
	return r.kc.doAdmin(ctx, http.MethodDelete, fmt.Sprintf("/users/%s/groups/%s", kcU.ID, kcG.ID), nil, nil)
}
