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

// Bounds what a hostile or misconfigured host can make the control plane hold
// in memory. A secret larger than this is not one an import can carry.
const maxVaultResponseBytes = 1 << 20

func countProperties(n int) string {
	if n == 1 {
		return "1 property"
	}
	return fmt.Sprintf("%d properties", n)
}

// normalizeRemoteKey is the single reading of a remote key, shared by the check,
// the validation and the CR that is written.
//
// Only surrounding slashes and spaces go. Vault accepts every other character
// and external-secrets percent-encodes the path, so a key named "avec espace"
// is a valid key.
func normalizeRemoteKey(key string) string {
	// Both cut sets at once, not one Trim then the other: spaces first leaves
	// "//  //" as two spaces, slashes first leaves "/ foo" with its leading one.
	// Either clears every emptiness guard and still names nothing.
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
// as path segments and nothing else. Concatenated raw, a "?" in either turns
// the rest of the path into a query string.
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
// It returns property names, never values: returning values would turn a form
// helper into a way to read every secret the store can reach.
func (s *DefaultExternalSecretService) CheckRemoteRef(ctx context.Context, namespace string, req models.ExternalSecretCheckRequest) (*models.ExternalSecretCheckResponse, error) {
	if req.SecretStoreRef == "" {
		return nil, invalid("secretStoreRef is required")
	}
	if req.RemoteRef.Key == "" {
		return nil, invalid("remoteRef.key is required")
	}
	key := normalizeRemoteKey(req.RemoteRef.Key)
	// After the trim, not before: "/" and "//" clear the guard above and then
	// read the mount root instead of a key.
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

	// No token does not mean Kubernetes auth: a store can carry neither, and
	// that is a fault of the store.
	if vault.Auth.TokenSecretRef == nil && vault.Auth.Kubernetes == nil {
		return nil, invalid("secret store %q carries no vault authentication, neither a token nor the kubernetes method", req.SecretStoreRef)
	}

	// Kubernetes auth logs in as the store's ServiceAccount, an identity the
	// control plane cannot borrow without the right to mint tokens for any
	// account in the project.
	if vault.Auth.TokenSecretRef == nil {
		return &models.ExternalSecretCheckResponse{
			Verifiable: false,
			Message:    "this store authenticates with the Kubernetes method, which the control plane cannot use on the store's behalf, so the key cannot be checked before the import runs",
		}, nil
	}

	data, err := s.secretStoreRepo.GetSecretData(ctx, namespace, vault.Auth.TokenSecretRef.Name)
	if err != nil {
		// Not wrapped: the handler maps a Kubernetes NotFound to "secret store
		// not found", and this one is the credentials Secret, not the store.
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
		// answered.
		return &models.ExternalSecretCheckResponse{
			Verifiable: false,
			Message:    fmt.Sprintf("the key could not be checked: %v", err),
		}, nil
	}

	switch status {
	case http.StatusOK:
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
			// Not Found:true: the key is confirmed, the path is not, and it may
			// well lead nowhere.
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

// external-secrets resolves a property with gjson. A property of ordinary
// characters can only match a top-level name, all of which are known at the
// call site, so its absence is a fact. Two forms can reach further:
//
//   - a wildcard or an escape names a top-level field the literal comparison
//     missed, whatever the values hold;
//   - a dotted path descends into a value that is itself JSON, so it reaches
//     somewhere only when the key holds one.
func propertyCouldReachFurther(property string, nested bool) bool {
	if strings.ContainsAny(property, `\*?#@|`) {
		return true
	}
	return nested && strings.Contains(property, ".")
}

// notFoundMessage names the mistake when it can. A KV v2 mount is read at
// <mount>/data/<key>, but an import writes the logical key without that prefix,
// so a pasted API path is held by Vault and unreachable by the import.
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
	// CABundle is a string here and a []byte once the base64 fix lands.
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
	// The server address comes from the store, so it is caller-supplied, and the
	// client timeout bounds how long a hostile host can stream, not how much.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVaultResponseBytes)).Decode(&body); err != nil {
		return nil, false, resp.StatusCode, fmt.Errorf("vault answered something that is not a secret: %w", err)
	}

	fields := body.Data
	if vault.Version != "v1" {
		raw, ok := body.Data["data"]
		if !ok {
			// A KV v2 read always wraps the secret in data.data. Keeping the
			// outer envelope would report "version" and "metadata" as
			// properties and call the key found.
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
		// A value that is itself JSON holds paths this check cannot enumerate.
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
