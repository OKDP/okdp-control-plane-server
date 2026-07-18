package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okdp/okdp-server-new/internal/repository"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

type ServiceVersionsResponse struct {
	Versions []string `json:"versions"`
	Default  string   `json:"default"`
}

type PackageSchemaService interface {
	GetParameterSchema(ctx context.Context, serviceName, tag string) (map[string]any, error)
	GetServiceVersions(ctx context.Context, serviceName string) (*ServiceVersionsResponse, error)
	// ListPackageTags returns the tags published in the OCI registry for a service's
	// package, even if the service is not (yet) in the catalog. repositoryOverride
	// takes precedence over the Context's global package repository when non-empty.
	ListPackageTags(ctx context.Context, serviceName, repositoryOverride string) ([]string, error)
}

type DefaultPackageSchemaService struct {
	contextRepo repository.ContextRepository
	cache       sync.Map
	cacheTTL    time.Duration
}

type schemaCacheEntry struct {
	schema    map[string]any
	fetchedAt time.Time
}

func NewDefaultPackageSchemaService(contextRepo repository.ContextRepository) *DefaultPackageSchemaService {
	return &DefaultPackageSchemaService{
		contextRepo: contextRepo,
		cacheTTL:    15 * time.Minute,
	}
}

func (s *DefaultPackageSchemaService) GetServiceVersions(ctx context.Context, serviceName string) (*ServiceVersionsResponse, error) {
	services, err := s.contextRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get platform services: %w", err)
	}

	var defaultVersion, svcRepository string
	found := false
	for _, svc := range services {
		if svc.Name == serviceName {
			defaultVersion = svc.DefaultVersion
			svcRepository = svc.Repository
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("service %q not found in platform services", serviceName)
	}

	packageRepo, err := s.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get package repository: %w", err)
	}
	if svcRepository != "" {
		packageRepo = svcRepository
	}

	versions, err := s.listOCITags(packageRepo, serviceName)
	if err != nil {
		logrus.WithError(err).Warnf("failed to list OCI tags for %s, falling back to default version", serviceName)
		if defaultVersion != "" {
			versions = []string{defaultVersion}
		}
	}

	return &ServiceVersionsResponse{
		Versions: versions,
		Default:  defaultVersion,
	}, nil
}

// ListPackageTags resolves the package repository from the Context (or the
// per-service override when set) and lists the OCI tags published for the given
// service's package.
func (s *DefaultPackageSchemaService) ListPackageTags(ctx context.Context, serviceName, repositoryOverride string) ([]string, error) {
	packageRepo, err := s.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get package repository: %w", err)
	}
	if repositoryOverride != "" {
		packageRepo = repositoryOverride
	}
	return s.listOCITags(packageRepo, serviceName)
}

// listOCITags fetches available tags from the OCI registry for a given package.
func (s *DefaultPackageSchemaService) listOCITags(packageRepo, serviceName string) ([]string, error) {
	// packageRepo is like "quay.io/kubotal/packages-dev"
	registryURL := fmt.Sprintf("https://%s/v2/%s/tags/list",
		strings.SplitN(packageRepo, "/", 2)[0],
		strings.SplitN(packageRepo, "/", 2)[1]+"/"+serviceName,
	)

	resp, err := registryGet(registryURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tags from %s: %w", registryURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned status %d for %s", resp.StatusCode, registryURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read registry response: %w", err)
	}

	var tagsResp struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &tagsResp); err != nil {
		return nil, fmt.Errorf("failed to parse registry response: %w", err)
	}

	sort.Sort(sort.Reverse(sort.StringSlice(tagsResp.Tags)))
	return tagsResp.Tags, nil
}

// registryGet performs a Docker Registry v2 GET, honoring the anonymous
// bearer-token challenge some registries issue even for public repositories
// (ghcr.io always does; quay.io serves public reads without it): on 401,
// fetch a pull token from the advertised realm and replay the request.
func registryGet(url string) (*http.Response, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()

	token, err := fetchAnonymousToken(challenge)
	if err != nil {
		return nil, fmt.Errorf("registry requires authentication and the anonymous token flow failed: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

// parseBearerChallenge extracts the realm and query parameters (service,
// scope, ...) from a WWW-Authenticate header such as:
//
//	Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:org/repo:pull"
func parseBearerChallenge(header string) (realm string, params url.Values, err error) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", nil, fmt.Errorf("unsupported auth challenge %q", header)
	}

	params = url.Values{}
	for _, part := range strings.Split(rest, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		value = strings.Trim(value, `"`)
		if key == "realm" {
			realm = value
		} else {
			params.Set(key, value)
		}
	}
	if realm == "" {
		return "", nil, fmt.Errorf("no realm in auth challenge %q", header)
	}
	return realm, params, nil
}

// fetchAnonymousToken resolves a bearer challenge by requesting a token from
// its realm without credentials — registries grant pull tokens anonymously
// for public repositories.
func fetchAnonymousToken(challenge string) (string, error) {
	realm, params, err := parseBearerChallenge(challenge)
	if err != nil {
		return "", err
	}

	resp, err := http.Get(realm + "?" + params.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s returned status %d", realm, resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}
	if tokenResp.Token != "" {
		return tokenResp.Token, nil
	}
	if tokenResp.AccessToken != "" {
		return tokenResp.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint %s returned no token", realm)
}

func (s *DefaultPackageSchemaService) GetParameterSchema(ctx context.Context, serviceName, tag string) (map[string]any, error) {
	services, err := s.contextRepo.GetPlatformServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get platform services: %w", err)
	}

	var svcRepository string
	for _, svc := range services {
		if svc.Name == serviceName {
			if tag == "" {
				tag = svc.DefaultVersion
			}
			svcRepository = svc.Repository
			break
		}
	}
	if tag == "" {
		return nil, fmt.Errorf("service %q not found in platform services", serviceName)
	}

	packageRepo, err := s.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get package repository: %w", err)
	}
	if svcRepository != "" {
		packageRepo = svcRepository
	}

	cacheKey := fmt.Sprintf("%s:%s", serviceName, tag)
	if entry, ok := s.cache.Load(cacheKey); ok {
		ce := entry.(*schemaCacheEntry)
		if time.Since(ce.fetchedAt) < s.cacheTTL {
			return ce.schema, nil
		}
	}

	ociRef := fmt.Sprintf("oci://%s/%s:%s", packageRepo, serviceName, tag)
	schema, err := s.fetchSchemaFromOCI(ociRef)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch schema for %s: %w", ociRef, err)
	}

	enriched := parseTitleMetadata(schema)

	s.cache.Store(cacheKey, &schemaCacheEntry{schema: enriched, fetchedAt: time.Now()})

	return enriched, nil
}

func (s *DefaultPackageSchemaService) fetchSchemaFromOCI(ociRef string) (map[string]any, error) {
	tmpDir, err := os.MkdirTemp("", "kubocd-dump-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	cmd := exec.Command("kubocd", "dump", "package", ociRef, "--anonymous", "-o", tmpDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		logrus.WithError(err).WithField("output", string(output)).Error("kubocd dump failed")
		return nil, fmt.Errorf("kubocd dump failed: %s", string(output))
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("no dump output found in %s", tmpDir)
	}

	groomedPath := fmt.Sprintf("%s/%s/groomed.yaml", tmpDir, entries[0].Name())
	data, err := os.ReadFile(groomedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read groomed.yaml: %w", err)
	}

	var groomedDoc map[string]any
	if err := yaml.Unmarshal(data, &groomedDoc); err != nil {
		return nil, fmt.Errorf("failed to parse groomed.yaml: %w", err)
	}

	schemaSection, ok := groomedDoc["schema"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no 'schema' section in groomed output")
	}

	parameters, ok := schemaSection["parameters"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no 'schema.parameters' in groomed output")
	}

	return parameters, nil
}

// parseTitleMetadata reads the `title` field from each property and expands it
// into `x-ui-*` fields.
//
// Title format: "Group | Label | widget | key:value key:value..."
//   - Segment 1: group name (becomes x-ui-group)
//   - Segment 2: display label (replaces title)
//   - Segment 3: widget name (becomes x-ui-widget, empty = auto-detect)
//   - Segment 4+: space-separated key:value pairs (become x-ui-<key>)
//
// If title has no "|" separators, it's treated as a plain label (no UI hints).
func parseTitleMetadata(schema map[string]any) map[string]any {
	result := deepCopyMap(schema)

	props, ok := result["properties"].(map[string]any)
	if !ok {
		return result
	}

	for _, propDef := range props {
		propMap, ok := propDef.(map[string]any)
		if !ok {
			continue
		}

		titleRaw, ok := propMap["title"]
		if !ok {
			continue
		}
		title, ok := titleRaw.(string)
		if !ok || !strings.Contains(title, "|") {
			continue
		}

		parts := strings.SplitN(title, "|", 4)
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}

		if len(parts) >= 1 && parts[0] != "" {
			propMap["x-ui-group"] = parts[0]
		}

		if len(parts) >= 2 && parts[1] != "" {
			propMap["title"] = parts[1]
		} else {
			delete(propMap, "title")
		}

		if len(parts) >= 3 && parts[2] != "" {
			propMap["x-ui-widget"] = parts[2]
		}

		if len(parts) >= 4 && parts[3] != "" {
			for _, kv := range strings.Fields(parts[3]) {
				eqIdx := strings.Index(kv, ":")
				if eqIdx < 0 {
					continue
				}
				key := kv[:eqIdx]
				val := kv[eqIdx+1:]

				switch key {
				case "condition":
					eqParts := strings.SplitN(val, "=", 2)
					if len(eqParts) == 2 {
						condVal := parseValue(eqParts[1])
						propMap["x-ui-condition"] = map[string]any{
							"field": eqParts[0],
							"value": condVal,
						}
					}
				default:
					propMap["x-ui-"+key] = parseValue(val)
				}
			}
		}
	}

	return result
}

func parseValue(s string) any {
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func deepCopyMap(src map[string]any) map[string]any {
	raw, _ := json.Marshal(src)
	var dst map[string]any
	_ = json.Unmarshal(raw, &dst)
	return dst
}

