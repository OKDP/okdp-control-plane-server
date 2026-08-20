package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
	"gopkg.in/yaml.v3"
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
	contextRepo        repository.ContextRepository
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
	// inputs is the package's declared connection inputs, read from the same
	// dump as the schema so a package is fetched once for both.
	inputs    []models.PackageInput
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

	ociRef := fmt.Sprintf("oci://%s/%s:%s", packageRepo, serviceName, tag)
	doc, err := s.fetchGroomedDoc(ociRef, insecureOCIHost(packageRepo, s.insecureRegistries))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch schema for %s: %w", ociRef, err)
	}

	schema, err := parametersOf(doc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch schema for %s: %w", ociRef, err)
	}

	// connectionRef parameters and the hand-written stanza coexist (an alias
	// collision is a groom-time error upstream), so both sources are offered.
	entry := &schemaCacheEntry{
		schema:    parseTitleMetadata(schema),
		inputs:    append(inputsFromMarkers(doc), inputsOf(doc)...),
		fetchedAt: time.Now(),
	}
	s.cache.Store(cacheKey, entry)
	return entry, nil
}

// dumpTimeout bounds one `kubocd dump package`, which pulls an OCI artifact.
// Generous on purpose: a cold registry over a slow link is not a failure.
const dumpTimeout = 60 * time.Second

func (s *DefaultPackageSchemaService) fetchGroomedDoc(ociRef string, insecure bool) (map[string]any, error) {
	tmpDir, err := os.MkdirTemp("", "kubocd-dump-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	args := []string{"dump", "package", ociRef, "--anonymous", "-o", tmpDir}
	if insecure {
		args = append(args, "--insecure")
	}

	// A budget, because this reaches a registry over the network. Without one, a
	// registry that accepts the connection and never answers pins the request,
	// and with it the goroutine serving it, for as long as the process lives.
	ctx, cancel := context.WithTimeout(context.Background(), dumpTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "kubocd", args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		logrus.WithField("ref", ociRef).WithField("timeout", dumpTimeout).Error("kubocd dump timed out")
		return nil, fmt.Errorf("kubocd dump timed out after %s for %s, the registry did not answer", dumpTimeout, ociRef)
	}
	if err != nil {
		if stale := staleKubocd(string(output)); stale != "" {
			logrus.WithField("ref", ociRef).Error(stale)
			return nil, errors.New(stale)
		}
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

	return groomedDoc, nil
}

// splitOptions cuts the option segment of a title on spaces, except inside
// double quotes. Unquoted values cannot hold a space.
func splitOptions(segment string) []string {
	// An unbalanced quote would swallow every option after it. Splitting on
	// spaces truncates one value instead of dropping the rest silently.
	if strings.Count(segment, `"`)%2 != 0 {
		logrus.Warnf("unbalanced quote in title options %q, falling back to splitting on spaces", segment)
		return strings.Fields(segment)
	}

	var options []string
	var current strings.Builder
	inQuotes := false

	flush := func() {
		if current.Len() > 0 {
			options = append(options, current.String())
			current.Reset()
		}
	}

	for _, r := range segment {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ' ' && !inQuotes:
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return options
}

// parseTitleMetadata reads the `title` field from each property and expands it
// into `x-ui-*` fields.
//
// Title format: "Group | Label | widget | key:value key:value..."
//   - Segment 1: group name (becomes x-ui-group)
//   - Segment 2: display label (replaces title)
//   - Segment 3: widget name (becomes x-ui-widget, empty = auto-detect)
//   - Segment 4+: space-separated key:value pairs (become x-ui-<key>).
//     Quote a value that holds spaces: placeholder:"e.g. 10Gi, 50Gi".
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
			for _, kv := range splitOptions(parts[3]) {
				eqIdx := strings.Index(kv, ":")
				if eqIdx < 0 {
					continue
				}
				key := kv[:eqIdx]
				val := strings.Trim(kv[eqIdx+1:], `"`)

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

// parametersOf pulls the parameter schema out of a package document.
func parametersOf(doc map[string]any) (map[string]any, error) {
	schemaSection, ok := doc["schema"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no 'schema' section in groomed output")
	}
	parameters, ok := schemaSection["parameters"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no 'schema.parameters' in groomed output")
	}
	return parameters, nil
}

// namedConnectionParameter finds the parameter reference in the template a
// package uses to let the user pick the connection, typically
// `{{ .Parameters.pgConnection | default "-" }}`. Pipelines around it are the
// norm (the default is how an optional input expresses "none"), so this
// searches for the reference rather than matching the whole template. An input
// bound any other way (a fixed name, a release output) has no reference and
// offers the user no choice.
var namedConnectionParameter = regexp.MustCompile(`\.Parameters\.([A-Za-z_][A-Za-z0-9_]*)`)

// inputsOf reads the connections a package declares it needs. A package with
// no inputs is the normal case today, so this never fails: it returns nothing.
func inputsOf(doc map[string]any) []models.PackageInput {
	raw, ok := doc["inputs"].([]any)
	if !ok {
		return nil
	}

	inputs := make([]models.PackageInput, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		contract, _ := entry["contract"].(string)
		if contract == "" {
			continue
		}

		input := models.PackageInput{Contract: contract}
		if alias, _ := entry["alias"].(string); alias != "" {
			input.Alias = alias
		} else {
			// The package format defaults the alias to the contract name.
			input.Alias = contract
		}
		// KcdTemplateBool fields arrive as template strings, so a literal true
		// is the string "true", not a boolean.
		switch v := entry["optional"].(type) {
		case bool:
			input.Optional = v
		case string:
			input.Optional = strings.EqualFold(strings.TrimSpace(v), "true")
		}
		input.Description, _ = entry["description"].(string)

		if named, ok := entry["namedConnection"].(map[string]any); ok {
			if name, _ := named["name"].(string); name != "" {
				if m := namedConnectionParameter.FindStringSubmatch(name); m != nil {
					input.Parameter = m[1]
				}
			}
		}
		inputs = append(inputs, input)
	}
	return inputs
}

// inputsFromMarkers reads the connection inputs a package declares through
// connectionRef parameters. The groom desugars those into plain string nodes
// carrying an `x-kubocd-connection-ref` marker, so this is the declarative
// successor of the hand-written inputs stanza: the parameter IS the input,
// no template to reverse-engineer.
//
// Only top-level parameters are reported: a ref nested in an array generates
// one input per element, which no deployment form can offer a static choice
// for. connectionSelector markers are skipped for the same reason the picker
// would skip them, the package queries by labels, the user provides nothing.
func inputsFromMarkers(doc map[string]any) []models.PackageInput {
	schemaSection, ok := doc["schema"].(map[string]any)
	if !ok {
		return nil
	}
	parameters, ok := schemaSection["parameters"].(map[string]any)
	if !ok {
		return nil
	}
	properties, ok := parameters["properties"].(map[string]any)
	if !ok {
		return nil
	}

	// For a connectionRef the required flag lives in the PARENT's required
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
		marker, ok := property["x-kubocd-connection-ref"].(map[string]any)
		if !ok {
			continue
		}
		contract, _ := marker["contract"].(string)
		if contract == "" {
			continue
		}
		description, _ := property["description"].(string)
		// The default survives the groom as an ordinary schema default. Passing
		// it on lets the form say the binding is inherited instead of showing
		// None, which reads as "nothing", the opposite of the truth.
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

// staleKubocd names the one failure that reads as a broken package but is a
// broken server: a kubocd older than the package model it is asked to read.
// The raw error blames the package, which sends the reader editing a file that
// is fine.
func staleKubocd(output string) string {
	for _, unknown := range []string{"unknown type 'connectionRef'", `unknown field "outputs"`} {
		if strings.Contains(output, unknown) {
			return "the kubocd binary shipped with this server is older than the package it reads (" +
				unknown + "). Rebuild the server image against a kubocd that supports the unified connections model."
		}
	}
	return ""
}
