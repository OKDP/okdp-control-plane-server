package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// listOCITags fetches available tags from the OCI registry for a given package.
func (s *DefaultPackageSchemaService) listOCITags(packageRepo, serviceName string) ([]string, error) {
	// packageRepo is like "quay.io/kubotal/packages-dev"
	registryURL := fmt.Sprintf("https://%s/v2/%s/tags/list",
		strings.SplitN(packageRepo, "/", 2)[0],
		strings.SplitN(packageRepo, "/", 2)[1]+"/"+serviceName,
	)

	resp, err := http.Get(registryURL)
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

