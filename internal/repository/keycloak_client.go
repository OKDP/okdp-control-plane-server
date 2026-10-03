package repository

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/config"
)

// KeycloakIssuerSource returns the issuer of the platform realm
// (<keycloak url>/realms/<realm>) and whether its certificate is taken on
// trust. It locates Keycloak when KEYCLOAK_URL/KEYCLOAK_REALM are unset.
type KeycloakIssuerSource func(ctx context.Context) (issuer string, insecureSkipVerify bool, err error)

// errKeycloakNotConfigured is returned by calls made while the admin client
// has no credentials or no endpoint.
var errKeycloakNotConfigured = errors.New("keycloak user management is not configured: set KEYCLOAK_CLIENT_SECRET, and KEYCLOAK_URL/KEYCLOAK_REALM unless the platform issuer is a Keycloak realm")

// keycloakClient is the HTTP client for the Keycloak Admin REST API. It
// authenticates with the client_credentials grant of the control-plane
// service account and caches the access token until shortly before expiry.
type keycloakClient struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	tlsInsecure  bool
	issuer       KeycloakIssuerSource

	httpClient         *http.Client
	insecureHTTPClient *http.Client

	mu          sync.Mutex
	token       string
	tokenKey    string
	tokenExpiry time.Time
}

// keycloakEndpoint is where the realm lives, resolved per call: the platform
// issuer may change without a restart.
type keycloakEndpoint struct {
	baseURL  string
	realm    string
	insecure bool
}

// kcAPIError carries the HTTP status of a failed Keycloak call.
type kcAPIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *kcAPIError) Error() string {
	return fmt.Sprintf("keycloak request %s %s failed (%d): %s", e.Method, e.Path, e.Status, e.Body)
}

func newKeycloakClient(cfg *config.Config, issuer KeycloakIssuerSource) *keycloakClient {
	// Cloned, not built from scratch: a bare Transport has no Proxy, and the
	// chart passes the egress proxy the pod must go through.
	insecureTransport := http.DefaultTransport.(*http.Transport).Clone()
	insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- opt-in, sandboxes

	return &keycloakClient{
		baseURL:            strings.TrimRight(cfg.KeycloakURL, "/"),
		realm:              cfg.KeycloakRealm,
		clientID:           cfg.KeycloakClientID,
		clientSecret:       cfg.KeycloakClientSecret,
		tlsInsecure:        cfg.KeycloakTLSInsecure,
		issuer:             issuer,
		httpClient:         &http.Client{Timeout: 30 * time.Second},
		insecureHTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: insecureTransport},
	}
}

// endpoint resolves the Keycloak base URL and realm: the explicit settings
// first, the platform issuer for whatever they leave out.
func (c *keycloakClient) endpoint(ctx context.Context) (*keycloakEndpoint, error) {
	if c.clientSecret == "" {
		return nil, errKeycloakNotConfigured
	}
	ep := &keycloakEndpoint{baseURL: c.baseURL, realm: c.realm, insecure: c.tlsInsecure}
	if ep.baseURL != "" && ep.realm != "" {
		return ep, nil
	}
	if c.issuer == nil {
		return nil, errKeycloakNotConfigured
	}
	issuer, insecure, err := c.issuer(ctx)
	if err != nil {
		return nil, fmt.Errorf("locating keycloak from the platform issuer: %w", err)
	}
	base, realm, found := strings.Cut(strings.TrimRight(issuer, "/"), "/realms/")
	if !found || base == "" || realm == "" || strings.Contains(realm, "/") {
		return nil, fmt.Errorf("%w (the platform issuer %q is not <url>/realms/<realm>)", errKeycloakNotConfigured, issuer)
	}
	if ep.baseURL == "" {
		ep.baseURL = base
		ep.insecure = ep.insecure || insecure
	}
	if ep.realm == "" {
		ep.realm = realm
	}
	return ep, nil
}

// configured reports whether calls can be made at all.
func (c *keycloakClient) configured(ctx context.Context) bool {
	_, err := c.endpoint(ctx)
	return err == nil
}

func (c *keycloakClient) client(ep *keycloakEndpoint) *http.Client {
	if ep.insecure {
		return c.insecureHTTPClient
	}
	return c.httpClient
}

func (c *keycloakClient) getToken(ctx context.Context, ep *keycloakEndpoint) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := ep.baseURL + "|" + ep.realm
	if c.token != "" && c.tokenKey == key && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", ep.baseURL, url.PathEscape(ep.realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client(ep).Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak token request failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to decode keycloak token response: %w", err)
	}

	c.token = tokenResp.AccessToken
	c.tokenKey = key
	// Refresh slightly before expiry
	c.tokenExpiry = time.Now().Add(time.Duration(tokenResp.ExpiresIn-30) * time.Second)
	return c.token, nil
}

// do performs an authenticated request; path is relative to the Keycloak
// base URL. out may be nil. On 401 (token revoked or expired server-side)
// the cached token is discarded and the request retried once with a fresh
// token.
func (c *keycloakClient) do(ctx context.Context, ep *keycloakEndpoint, method, path string, payload, out interface{}) error {
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}

	for attempt := 0; ; attempt++ {
		token, err := c.getToken(ctx, ep)
		if err != nil {
			return err
		}

		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(data)
		}

		req, err := http.NewRequestWithContext(ctx, method, ep.baseURL+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.client(ep).Do(req)
		if err != nil {
			return fmt.Errorf("keycloak request %s %s failed: %w", method, path, err)
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &kcAPIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(respBody)}
		}

		if out != nil && len(respBody) > 0 {
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("failed to decode keycloak response for %s %s: %w", method, path, err)
			}
		}
		return nil
	}
}

// doAdmin performs a request against the Admin API; path is relative to
// /admin/realms/{realm}.
func (c *keycloakClient) doAdmin(ctx context.Context, method, path string, payload, out interface{}) error {
	ep, err := c.endpoint(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, ep, method, fmt.Sprintf("/admin/realms/%s%s", url.PathEscape(ep.realm), path), payload, out)
}
