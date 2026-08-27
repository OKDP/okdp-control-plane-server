package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// A remote key goes straight into the Vault URL, so it may only carry what a
// path segment carries. Without this, "a?b" turns the read into a request for
// another path with a query string, and ".." walks out of the store's mount:
// the check would then report on a key nobody asked about.
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

var vaultKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

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
	key := strings.Trim(req.RemoteRef.Key, "/")
	if !vaultKeyPattern.MatchString(key) {
		return nil, invalid("remoteRef.key %q is not a valid path", req.RemoteRef.Key)
	}

	store, err := s.secretStoreRepo.Get(ctx, namespace, req.SecretStoreRef)
	if err != nil {
		return nil, err
	}
	vault := store.Spec.Provider.Vault
	if vault == nil {
		return nil, invalid("secret store %q is not backed by vault", req.SecretStoreRef)
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

	properties, status, err := readVaultKey(ctx, vault, key, token)
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
func readVaultKey(ctx context.Context, vault *crd.ESOVaultProvider, key, token string) ([]string, int, error) {
	// Converted explicitly: the CRD field is a string today and becomes a
	// []byte with the base64 fix, and this reads the same either way.
	client, err := vaultHTTPClient(string(vault.CABundle))
	if err != nil {
		return nil, 0, err
	}

	mount := strings.Trim(vault.Path, "/")
	if mount == "" {
		mount = "secret"
	}
	url := strings.TrimSuffix(vault.Server, "/") + "/v1/" + mount
	if vault.Version == "v1" {
		url += "/" + key
	} else {
		url += "/data/" + key
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("X-Vault-Token", token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}

	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	// The server address comes from the store, so it is caller-supplied. The
	// client timeout bounds how long a hostile host can stream, not how much,
	// and a secret that needs more than this is not one an import can carry.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVaultResponseBytes)).Decode(&body); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("vault answered something that is not a secret: %w", err)
	}

	fields := body.Data
	if vault.Version != "v1" {
		var inner struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if raw, ok := body.Data["data"]; ok {
			if err := json.Unmarshal(raw, &inner.Data); err != nil {
				return nil, resp.StatusCode, fmt.Errorf("vault answered something that is not a KV v2 secret: %w", err)
			}
			fields = inner.Data
		}
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	// Stable, so the same key always reads the same way in the form.
	sort.Strings(names)
	return names, resp.StatusCode, nil
}
