package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// A secret holding more than this is not one an import can carry, and the
// figure bounds what a hostile or misconfigured host can make the control
// plane hold in memory.
const maxVaultResponseBytes = 1 << 20

// countProperties keeps the message readable for one property as well as many.
func countProperties(n int) string {
	if n == 1 {
		return "1 property"
	}
	return fmt.Sprintf("%d properties", n)
}

// normalizeRemoteKey is the single reading of a remote key, shared by the
// check, the validation and the CR that is written. Without one, the check
// answered about "foo" while the import stored " foo " and external-secrets
// looked up a key nobody had confirmed.
//
// A path segment that walks out of the mount is refused elsewhere; every other
// character is kept, because Vault accepts them and external-secrets passes the
// key to a client that percent-encodes the path. Measured against the running
// cluster: a key literally named "avec espace" syncs, so banning the character
// would have blocked an import that works today.
func normalizeRemoteKey(key string) string {
	// Both cut sets at once, and not one Trim after the other: trimming the
	// spaces first leaves "//  //" as two spaces, trimming the slashes first
	// leaves "/ foo" with its leading space. Either way the result clears every
	// emptiness guard and still names nothing.
	return strings.TrimFunc(key, func(r rune) bool {
		return r == '/' || unicode.IsSpace(r)
	})
}

func hasTraversal(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// vaultURL builds the read URL so the caller-supplied mount and key reach Vault
// as path segments and nothing else. Concatenated raw, a "?" in either one
// turned the rest of the path into a query string and the check reported on an
// endpoint nobody asked about.
func vaultURL(server, mount, key string, kvV2 bool) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(server, "/"))
	if err != nil {
		return "", fmt.Errorf("the store's server address is not a URL: %w", err)
	}
	segments := u.Path + "/v1/" + mount
	if kvV2 {
		segments += "/data"
	}
	u.Path = segments + "/" + key
	return u.String(), nil
}

// CheckRemoteRef reports whether a remote key can be read through a store,
// before any import exists.
//
// Without it the only way to learn that a key is missing is to create the
// import and wait for external-secrets to fail its first sync, which reports
// "could not get secret data from provider" without naming what is missing.
//
// It never returns secret values, only the names of the properties a key
// holds. Returning values would turn a form helper into a way to read every
// secret the store can reach.
func (s *DefaultExternalSecretService) CheckRemoteRef(ctx context.Context, namespace string, req models.ExternalSecretCheckRequest) (*models.ExternalSecretCheckResponse, error) {
	if req.SecretStoreRef == "" {
		return nil, invalid("secretStoreRef is required")
	}
	if req.RemoteRef.Key == "" {
		return nil, invalid("remoteRef.key is required")
	}
	key := normalizeRemoteKey(req.RemoteRef.Key)
	// Checked after the trim, not before: "/" and "//" clear the emptiness
	// guard above and then read the mount root instead of a key.
	if key == "" {
		return nil, invalid("remoteRef.key %q names no key", req.RemoteRef.Key)
	}
	if hasTraversal(key) {
		return nil, invalid("remoteRef.key %q walks out of the store's mount", req.RemoteRef.Key)
	}

	store, err := s.secretStoreRepo.Get(ctx, namespace, req.SecretStoreRef)
	if err != nil {
		return nil, err
	}
	vault := store.Spec.Provider.Vault
	if vault == nil {
		return nil, invalid("secret store %q is not backed by vault", req.SecretStoreRef)
	}

	// Neither method is not the Kubernetes case. Reading the absence of a token
	// as "this store uses Kubernetes auth" named a method the store does not
	// carry and sent the reader looking for a ServiceAccount that is not there.
	// The store itself is what is wrong, which is what the other store faults
	// in this function answer as well.
	if vault.Auth.TokenSecretRef == nil && vault.Auth.Kubernetes == nil {
		return nil, invalid("secret store %q carries no vault authentication, neither a token nor the kubernetes method", req.SecretStoreRef)
	}

	// The Kubernetes auth method logs in as the store's ServiceAccount, an
	// identity the control plane cannot borrow without the right to mint tokens
	// for any account in the project. Saying so is the honest answer; reporting
	// success would be the false green this check exists to remove.
	if vault.Auth.TokenSecretRef == nil {
		return &models.ExternalSecretCheckResponse{
			Verifiable: false,
			Message:    "this store authenticates with the Kubernetes method, which the control plane cannot use on the store's behalf, so the key cannot be checked before the import runs",
		}, nil
	}

	data, err := s.secretStoreRepo.GetSecretData(ctx, namespace, vault.Auth.TokenSecretRef.Name)
	if err != nil {
		// Deliberately not wrapped: the handler maps a Kubernetes NotFound to
		// "secret store not found", and this one is the credentials Secret. The
		// store is right there, and saying it is missing sends the caller
		// looking for something that never disappeared.
		if apierrors.IsNotFound(err) {
			return nil, invalid("store %q references a credentials secret %q that does not exist",
				req.SecretStoreRef, vault.Auth.TokenSecretRef.Name)
		}
		return nil, fmt.Errorf("failed to read the store credentials: %w", err)
	}
	tokenKey := vault.Auth.TokenSecretRef.Key
	if tokenKey == "" {
		tokenKey = "token"
	}
	token := string(data[tokenKey])
	if token == "" {
		return nil, invalid("the credentials of store %q carry no token under key %q", req.SecretStoreRef, tokenKey)
	}

	properties, nested, status, err := readVaultKey(ctx, vault, key, token)
	if err != nil {
		// Not always a reachability problem: an unusable CA bundle fails before
		// any request leaves, and an unreadable body arrives after Vault has
		// answered. Saying "could not be reached" for either sends the caller
		// hunting a network fault that is not there.
		return &models.ExternalSecretCheckResponse{
			Verifiable: false,
			Message:    fmt.Sprintf("the key could not be checked: %v", err),
		}, nil
	}

	switch status {
	case http.StatusOK:
		// Nothing more to check: the key is there and the caller wants the
		// whole of it.
		if req.RemoteRef.Property == "" {
			return &models.ExternalSecretCheckResponse{
				Verifiable: true, Found: true, Properties: properties,
				Message: fmt.Sprintf("key %q found, holding %s", key, countProperties(len(properties))),
			}, nil
		}
		for _, p := range properties {
			if p == req.RemoteRef.Property {
				return &models.ExternalSecretCheckResponse{
					Verifiable: true, Found: true, Properties: properties,
					Message: fmt.Sprintf("key %q holds property %q", key, p),
				}, nil
			}
		}
		if propertyCouldReachFurther(req.RemoteRef.Property, nested) {
			// Not Found:true: the path may well lead nowhere, and claiming it
			// resolves would be the green this check exists to remove. The key
			// is confirmed, the property is not, and the answer says so.
			return &models.ExternalSecretCheckResponse{
				Verifiable: false, Properties: properties,
				Message: fmt.Sprintf("key %q found, but property %q is not one of its top-level names. external-secrets resolves it as a gjson path into the stored value, which this check does not follow",
					key, req.RemoteRef.Property),
			}, nil
		}
		return &models.ExternalSecretCheckResponse{
			Verifiable: true, Found: false, Properties: properties,
			Message: fmt.Sprintf("key %q exists but holds no property %q", key, req.RemoteRef.Property),
		}, nil

	case http.StatusNotFound:
		return &models.ExternalSecretCheckResponse{
			Verifiable: true, Found: false,
			Message: notFoundMessage(vault, key),
		}, nil

	case http.StatusForbidden:
		// Vault answers 403 both for a path a policy denies and for one it will
		// not admit exists, so nothing about the key itself was established.
		// Reporting Found:false would paint it as absent when it may be there.
		return &models.ExternalSecretCheckResponse{
			Verifiable: false,
			Message:    fmt.Sprintf("the store's token is not allowed to read %q, so the key could not be checked", key),
		}, nil

	default:
		return &models.ExternalSecretCheckResponse{
			Verifiable: false,
			Message:    fmt.Sprintf("vault answered %d, the key could not be checked", status),
		}, nil
	}
}

// propertyCouldReachFurther reports whether a property could resolve to
// something other than one of the top-level names the caller has just
// enumerated.
//
// external-secrets resolves a property with gjson. A property made of ordinary
// characters can only match a top-level name, and every one of those is known
// at the call site, so its absence is a fact: saying so is what turns a typo
// into an answer instead of a shrug. Anything that can reach further is not
// followed here, and calling it absent would paint a working import red:
//
//   - a wildcard or an escape names a top-level field that was compared
//     literally and missed, whatever the values hold;
//   - a dotted path descends into a value that is itself JSON, so it reaches
//     somewhere only when the key holds one. Both readings were measured
//     against the running cluster and both sync.
func propertyCouldReachFurther(property string, nested bool) bool {
	if strings.ContainsAny(property, `\*?#@|`) {
		return true
	}
	return nested && strings.Contains(property, ".")
}

// notFoundMessage names the mistake when it can. A KV v2 mount is read at
// <mount>/data/<key>, but the key written in an import is the logical one
// without that prefix: pasting the API path is the most common way to get a
// key that Vault holds but the import cannot find.
func notFoundMessage(vault *crd.ESOVaultProvider, key string) string {
	if vault.Version != "v1" && strings.HasPrefix(key, "data/") {
		return fmt.Sprintf("no key %q under mount %q. This path starts with \"data/\", which belongs to the KV v2 API and not to the key itself: try %q",
			key, vault.Path, strings.TrimPrefix(key, "data/"))
	}
	return fmt.Sprintf("no key %q under mount %q", key, vault.Path)
}

// readVaultKey returns the property names a key holds and the status Vault
// answered. The values are read to learn the names and never leave this
// function.
func readVaultKey(ctx context.Context, vault *crd.ESOVaultProvider, key, token string) (names []string, nested bool, status int, err error) {
	// Converted explicitly: the CRD field is a string today and becomes a
	// []byte with the base64 fix, and this reads the same either way.
	client, err := vaultHTTPClient(string(vault.CABundle))
	if err != nil {
		return nil, false, 0, err
	}

	mount := strings.Trim(vault.Path, "/")
	if mount == "" {
		mount = "secret"
	}
	if hasTraversal(mount) {
		return nil, false, 0, fmt.Errorf("the store's secret path %q walks out of itself", vault.Path)
	}

	address, err := vaultURL(vault.Server, mount, key, vault.Version != "v1")
	if err != nil {
		return nil, false, 0, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, false, 0, err
	}
	httpReq.Header.Set("X-Vault-Token", token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, false, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, resp.StatusCode, nil
	}

	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	// The server address comes from the store, so it is caller-supplied. The
	// client timeout bounds how long a hostile host can stream, not how much,
	// and a secret that needs more than this is not one an import can carry.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVaultResponseBytes)).Decode(&body); err != nil {
		return nil, false, resp.StatusCode, fmt.Errorf("vault answered something that is not a secret: %w", err)
	}

	fields := body.Data
	if vault.Version != "v1" {
		raw, ok := body.Data["data"]
		if !ok {
			// A KV v2 read always wraps the secret in data.data. Without it this
			// is something else answering on the path, and keeping the outer
			// envelope would report "version" and "metadata" as properties and
			// call the key found.
			return nil, false, resp.StatusCode, fmt.Errorf("the answer is not shaped like a KV v2 secret")
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, false, resp.StatusCode, fmt.Errorf("vault answered something that is not a KV v2 secret: %w", err)
		}
		fields = inner
	}

	names = make([]string, 0, len(fields))
	for name, value := range fields {
		names = append(names, name)
		// external-secrets resolves a property with gjson, so a value that is
		// itself JSON can hold nested paths this check cannot enumerate. The
		// flag says so; the value never leaves.
		if valueCouldNest(value) {
			nested = true
		}
	}
	// Stable, so the same key always reads the same way in the form.
	sort.Strings(names)
	return names, nested, resp.StatusCode, nil
}

// valueCouldNest reports whether a stored value can carry nested paths, either
// as a JSON object or as a string holding one. Only the shape is examined.
func valueCouldNest(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	if strings.HasPrefix(trimmed, "{") {
		return true
	}
	var asString string
	if json.Unmarshal(value, &asString) == nil {
		return strings.HasPrefix(strings.TrimSpace(asString), "{")
	}
	return false
}
