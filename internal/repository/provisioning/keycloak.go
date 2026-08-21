package provisioning

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Admin REST API rather than RFC 7591 dynamic registration: 7591 requires
// persisting a per-client registration access token to update or delete a
// client later, while the Admin API allows an idempotent delete-by-clientId.
type KeycloakConfig struct {
	IssuerURI             string
	CredentialsSecret     KeycloakSecretRef
	InsecureSkipTLSVerify bool
}

type KeycloakSecretRef struct {
	Namespace       string
	Name            string
	ClientIDKey     string
	ClientSecretKey string
}

type keycloakProvisioner struct {
	cfgFn   func(ctx context.Context) (*KeycloakConfig, error)
	credsFn func(ctx context.Context, cfg *KeycloakConfig) (clientID, clientSecret string, err error)
}

func newKeycloakProvisioner(client dynamic.Interface, cfgFn func(ctx context.Context) (*KeycloakConfig, error)) *keycloakProvisioner {
	return &keycloakProvisioner{
		cfgFn: cfgFn,
		credsFn: func(ctx context.Context, cfg *KeycloakConfig) (string, string, error) {
			return readSecretCredentials(ctx, client, cfg.CredentialsSecret)
		},
	}
}

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

func readSecretCredentials(ctx context.Context, client dynamic.Interface, ref KeycloakSecretRef) (string, string, error) {
	u, err := client.Resource(secretGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("reading credentials secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	data, _, _ := unstructured.NestedStringMap(u.Object, "data")
	id, err := decodeSecretKey(data, ref.ClientIDKey)
	if err != nil {
		return "", "", fmt.Errorf("secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	secret, err := decodeSecretKey(data, ref.ClientSecretKey)
	if err != nil {
		return "", "", fmt.Errorf("secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return id, secret, nil
}

func decodeSecretKey(data map[string]string, key string) (string, error) {
	raw, ok := data[key]
	if !ok {
		return "", fmt.Errorf("missing key %q", key)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decoding key %q: %w", key, err)
	}
	return string(decoded), nil
}

func (p *keycloakProvisioner) EnsureClient(ctx context.Context, spec OidcClientSpec) error {
	cfg, session, err := p.connect(ctx)
	if err != nil {
		return err
	}

	existing, err := session.findClient(ctx, spec.Name)
	if err != nil {
		return err
	}

	if existing == nil {
		representation := map[string]any{
			"clientId":     spec.Name,
			"enabled":      true,
			"protocol":     "openid-connect",
			"redirectUris": spec.RedirectURIs,
		}
		return session.do(ctx, http.MethodPost, session.adminBase+"/clients", representation, http.StatusCreated)
	}

	existing["redirectUris"] = spec.RedirectURIs
	uuid, _ := existing["id"].(string)
	if uuid == "" {
		return fmt.Errorf("keycloak client %q has no id in its representation (issuer %s)", spec.Name, cfg.IssuerURI)
	}
	return session.do(ctx, http.MethodPut, session.adminBase+"/clients/"+uuid, existing, http.StatusNoContent)
}

func (p *keycloakProvisioner) DeleteClient(ctx context.Context, name string) error {
	_, session, err := p.connect(ctx)
	if err != nil {
		return err
	}

	existing, err := session.findClient(ctx, name)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	uuid, _ := existing["id"].(string)
	return session.do(ctx, http.MethodDelete, session.adminBase+"/clients/"+uuid, nil, http.StatusNoContent)
}

func (p *keycloakProvisioner) connect(ctx context.Context) (*KeycloakConfig, *keycloakSession, error) {
	cfg, err := p.cfgFn(ctx)
	if err != nil {
		return nil, nil, err
	}

	adminBase, err := adminBaseURL(cfg.IssuerURI)
	if err != nil {
		return nil, nil, err
	}

	clientID, clientSecret, err := p.credsFn(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	if cfg.InsecureSkipTLSVerify {
		// Cloned, not built from scratch: a bare Transport has no Proxy, and the
		// chart passes the egress proxy the pod must go through.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		httpClient.Transport = transport
	}

	// A fresh token per operation keeps the adapter stateless. Client cleanup is
	// rare enough that caching is not worth the invalidation logic.
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.IssuerURI+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("requesting keycloak admin token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, nil, fmt.Errorf("keycloak token endpoint returned %d: %s", resp.StatusCode, body)
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, nil, fmt.Errorf("decoding keycloak token response: %w", err)
	}

	return cfg, &keycloakSession{httpClient: httpClient, adminBase: adminBase, token: tokenResp.AccessToken}, nil
}

func adminBaseURL(issuer string) (string, error) {
	base, realm, found := strings.Cut(issuer, "/realms/")
	if !found || realm == "" {
		return "", fmt.Errorf("keycloak issuerUri %q does not match .../realms/{realm}", issuer)
	}
	return base + "/admin/realms/" + realm, nil
}

type keycloakSession struct {
	httpClient *http.Client
	adminBase  string
	token      string
}

func (s *keycloakSession) findClient(ctx context.Context, clientID string) (map[string]any, error) {
	endpoint := s.adminBase + "/clients?clientId=" + url.QueryEscape(clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("looking up keycloak client %q: %w", clientID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("keycloak clients lookup returned %d: %s", resp.StatusCode, body)
	}

	var clients []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		return nil, fmt.Errorf("decoding keycloak clients response: %w", err)
	}
	// The clientId filter can match partially. Require an exact match.
	for _, c := range clients {
		if id, _ := c["clientId"].(string); id == clientID {
			return c, nil
		}
	}
	return nil, nil
}

func (s *keycloakSession) do(ctx context.Context, method, endpoint string, payload any, expectedStatus int) error {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != expectedStatus {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s returned %d (expected %d): %s", method, endpoint, resp.StatusCode, expectedStatus, raw)
	}
	return nil
}
