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
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

type ServiceVersionsResponse struct {
	Versions []string `json:"versions"`
	Default  string   `json:"default"`
}

type PackageSchemaService interface {
	GetParameterSchema(ctx context.Context, serviceName, tag string) (map[string]any, error)
	// GetPackageInputs returns the connections a package declares it needs, so
	// the console can offer a choice for each one at deployment time.
	GetPackageInputs(ctx context.Context, serviceName, tag string) ([]models.PackageInput, error)
	GetServiceVersions(ctx context.Context, serviceName string) (*ServiceVersionsResponse, error)
	ListVersionsForServices(ctx context.Context, services []models.PlatformService) map[string][]string
	// ListPackageTags returns the tags published in the OCI registry for a service's
	// package, even if the service is not (yet) in the catalog. repositoryOverride
	// takes precedence over the Context's global package repository when non-empty.
	ListPackageTags(ctx context.Context, serviceName, repositoryOverride string) ([]string, error)
}

type DefaultPackageSchemaService struct {
	contextRepo        repository.PlatformRepository
	charts             ChartSchemaFetcher
	cache              sync.Map
	inflight           singleflight.Group
	tagsCache          sync.Map
	cacheTTL           time.Duration
	insecureRegistries []string
}

// SetInsecureRegistries declares the plain-HTTP registry hosts, so package
// dumps and tag listings against them do not attempt TLS.
func (s *DefaultPackageSchemaService) SetInsecureRegistries(hosts []string) {
	s.insecureRegistries = hosts
}

type schemaCacheEntry struct {
	schema map[string]any
	// inputs is the chart's declared connection inputs, read from the same
	// schema so a chart is fetched once for both.
	inputs    []models.PackageInput
	fetchedAt time.Time
}

func NewDefaultPackageSchemaService(contextRepo repository.PlatformRepository) *DefaultPackageSchemaService {
	return &DefaultPackageSchemaService{
		contextRepo: contextRepo,
		charts:      NewOCIChartSchemaFetcher(),
		cacheTTL:    15 * time.Minute,
	}
}

// SetChartFetcher replaces how chart schemas are pulled (tests).
func (s *DefaultPackageSchemaService) SetChartFetcher(f ChartSchemaFetcher) {
	s.charts = f
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

	versions, err := s.listOCITagsCached(packageRepo, serviceName)
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
const tagsCacheTTL = 5 * time.Minute

type tagsCacheEntry struct {
	tags      []string
	fetchedAt time.Time
}

func (s *DefaultPackageSchemaService) cachedTags(repo, name string) ([]string, bool) {
	if e, ok := s.tagsCache.Load(repo + "/" + name); ok {
		ce := e.(*tagsCacheEntry)
		if time.Since(ce.fetchedAt) < tagsCacheTTL {
			return ce.tags, true
		}
	}
	return nil, false
}

func (s *DefaultPackageSchemaService) listOCITagsCached(repo, name string) ([]string, error) {
	if tags, ok := s.cachedTags(repo, name); ok {
		return tags, nil
	}
	tags, err := s.listOCITags(repo, name)
	if err != nil {
		return nil, err
	}
	s.tagsCache.Store(repo+"/"+name, &tagsCacheEntry{tags: tags, fetchedAt: time.Now()})
	return tags, nil
}

func (s *DefaultPackageSchemaService) ListVersionsForServices(ctx context.Context, services []models.PlatformService) map[string][]string {
	defaultRepo, err := s.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		logrus.WithError(err).Warn("failed to get package repository, skipping OCI version discovery")
		return map[string][]string{}
	}

	type result struct {
		name string
		tags []string
	}

	results := make(chan result, len(services))
	var wg sync.WaitGroup
	for _, svc := range services {
		wg.Add(1)
		go func(svc models.PlatformService) {
			defer wg.Done()
			repo := defaultRepo
			if svc.Repository != "" {
				repo = svc.Repository
			}
			tags, err := s.listOCITagsCached(repo, svc.Name)
			if err != nil {
				logrus.WithError(err).Warnf("failed to list OCI tags for %s, falling back to default version", svc.Name)
				if svc.DefaultVersion != "" {
					results <- result{name: svc.Name, tags: []string{svc.DefaultVersion}}
				}
				return
			}
			results <- result{name: svc.Name, tags: tags}
		}(svc)
	}
	wg.Wait()
	close(results)

	versions := make(map[string][]string, len(services))
	for r := range results {
		versions[r.name] = r.tags
	}
	return versions
}

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
	scheme := "https"
	if insecureOCIHost(packageRepo, s.insecureRegistries) {
		scheme = "http"
	}
	host, path, found := strings.Cut(packageRepo, "/")
	if !found || path == "" {
		return nil, fmt.Errorf("package repository %q carries no path, it must be like registry/namespace", packageRepo)
	}
	registryURL := fmt.Sprintf("%s://%s/v2/%s/tags/list", scheme, host, path+"/"+serviceName)

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
// its realm without credentials, registries grant pull tokens anonymously
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
	packageRepo, tag, err := s.resolvePackage(ctx, serviceName, tag)
	if err != nil {
		return nil, err
	}
	entry, err := s.load(serviceName, tag, packageRepo)
	if err != nil {
		return nil, err
	}
	return entry.schema, nil
}

// resolvePackage turns a service name and an optional tag into the OCI
// repository and the tag actually to fetch, defaulting the tag to the version
// the platform catalog declares.
func (s *DefaultPackageSchemaService) resolvePackage(ctx context.Context, serviceName, tag string) (string, string, error) {
	services, err := s.contextRepo.GetPlatformServices(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get platform services: %w", err)
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
		return "", "", fmt.Errorf("service %q not found in platform services", serviceName)
	}

	packageRepo, err := s.contextRepo.GetPackageRepository(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to get package repository: %w", err)
	}
	if svcRepository != "" {
		packageRepo = svcRepository
	}

	return packageRepo, tag, nil
}

// GetPackageInputs returns the connections a package declares it needs, so the
// console can offer a choice for each one at deployment time.
func (s *DefaultPackageSchemaService) GetPackageInputs(ctx context.Context, serviceName, tag string) ([]models.PackageInput, error) {
	packageRepo, tag, err := s.resolvePackage(ctx, serviceName, tag)
	if err != nil {
		return nil, err
	}
	entry, err := s.load(serviceName, tag, packageRepo)
	if err != nil {
		return nil, err
	}
	return entry.inputs, nil
}

// load returns the cached package document, fetching it once for both the
// parameter schema and the inputs.
func (s *DefaultPackageSchemaService) load(serviceName, tag, packageRepo string) (*schemaCacheEntry, error) {
	cacheKey := fmt.Sprintf("%s:%s", serviceName, tag)
	if cached, ok := s.cache.Load(cacheKey); ok {
		ce := cached.(*schemaCacheEntry)
		if time.Since(ce.fetchedAt) < s.cacheTTL {
			return ce, nil
		}
	}

	shared, err, _ := s.inflight.Do(cacheKey, func() (any, error) {
		return s.fetchAndCache(serviceName, tag, packageRepo, cacheKey)
	})
	if err != nil {
		return nil, err
	}
	return shared.(*schemaCacheEntry), nil
}

func (s *DefaultPackageSchemaService) fetchAndCache(serviceName, tag, packageRepo, cacheKey string) (*schemaCacheEntry, error) {
	if cached, ok := s.cache.Load(cacheKey); ok {
		ce := cached.(*schemaCacheEntry)
		if time.Since(ce.fetchedAt) < s.cacheTTL {
			return ce, nil
		}
	}

	repository := fmt.Sprintf("%s/%s", packageRepo, serviceName)
	ctx, cancel := context.WithTimeout(context.Background(), chartFetchTimeout)
	defer cancel()
	raw, err := s.charts.FetchValuesSchema(ctx, repository, tag, insecureOCIHost(packageRepo, s.insecureRegistries))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch the schema of oci://%s:%s: %w", repository, tag, err)
	}

	schema := parameterSchema(raw)
	entry := &schemaCacheEntry{
		schema:    schema,
		inputs:    inputsFromMarkers(schema),
		fetchedAt: time.Now(),
	}
	s.cache.Store(cacheKey, entry)
	return entry, nil
}

// chartFetchTimeout bounds one chart pull. Generous on purpose: a cold
// registry over a slow link is not a failure. Without a budget, a registry that
// accepts the connection and never answers pins the request forever.
const chartFetchTimeout = 60 * time.Second

// platformKeys are the root properties of every OKDP chart schema that the
// platform fills, never the user: the platform values and the external
// connection layers.
var platformKeys = []string{"global", "connections"}

// parameterSchema turns a chart's values.schema.json into the schema of the
// user parameters: the same document without the platform-filled root
// properties. The console renders it as the deployment form, and submitted
// parameters are validated against it. The x-ui-* keywords are the chart's
// own, passed through untouched.
func parameterSchema(chartSchema map[string]any) map[string]any {
	result := deepCopyMap(chartSchema)
	if props, ok := result["properties"].(map[string]any); ok {
		for _, key := range platformKeys {
			delete(props, key)
		}
	}
	if required, ok := result["required"].([]any); ok {
		kept := make([]any, 0, len(required))
		for _, r := range required {
			if name, _ := r.(string); name == "global" || name == "connections" {
				continue
			}
			kept = append(kept, r)
		}
		result["required"] = kept
	}
	return result
}

func deepCopyMap(src map[string]any) map[string]any {
	raw, _ := json.Marshal(src)
	var dst map[string]any
	_ = json.Unmarshal(raw, &dst)
	return dst
}

// ConnectionRefKeyword marks a string parameter holding the name of a
// connection of a contract: {"x-okdp-connection-ref": {"contract": "<contract>"}}.
const ConnectionRefKeyword = "x-okdp-connection-ref"

// inputsFromMarkers reads the connection inputs a chart declares: the root
// parameters carrying an x-okdp-connection-ref marker. The parameter IS the
// input, no template to reverse-engineer.
//
// Only top-level parameters are reported: a ref nested in an array produces
// one input per element, which no deployment form can offer a static choice
// for.
func inputsFromMarkers(parameters map[string]any) []models.PackageInput {
	properties, ok := parameters["properties"].(map[string]any)
	if !ok {
		return nil
	}

	// The required flag of a connection ref lives in the PARENT's required
	// array, not in the marker.
	required := map[string]bool{}
	if list, ok := parameters["required"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				required[name] = true
			}
		}
	}

	var inputs []models.PackageInput
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		marker, ok := property[ConnectionRefKeyword].(map[string]any)
		if !ok {
			continue
		}
		contract, _ := marker["contract"].(string)
		if contract == "" {
			continue
		}
		description, _ := property["description"].(string)
		// A default lets the form say the binding is inherited instead of
		// showing None, which reads as "nothing", the opposite of the truth.
		defaultValue, _ := property["default"].(string)
		inputs = append(inputs, models.PackageInput{
			Alias:       name,
			Contract:    contract,
			Parameter:   name,
			Optional:    !required[name],
			Default:     defaultValue,
			Description: description,
		})
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Parameter < inputs[j].Parameter })
	return inputs
}
