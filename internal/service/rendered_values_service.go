package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"golang.org/x/sync/singleflight"
)

// RenderedValuesService shows what an instance's chart passed to the upstream
// charts it vendors: the values ConfigMaps of okdp.vendor.render, each with
// the lines that differ from the vendored chart's defaults.
type RenderedValuesService interface {
	GetRenderedValues(ctx context.Context, project, name string) ([]models.RenderedValues, error)
}

type DefaultRenderedValuesService struct {
	deployments        *gitops.Deployments
	configMaps         repository.VendorValuesRepository
	charts             ChartValuesFetcher
	insecureRegistries []string
	cache              sync.Map
	inflight           singleflight.Group
}

// vendoredValuesTTL: a published chart version does not change, the cache
// only bounds memory held for versions nobody runs any more.
const vendoredValuesTTL = time.Hour

type vendoredValuesEntry struct {
	values    map[string][]byte
	fetchedAt time.Time
}

func NewDefaultRenderedValuesService(deployments *gitops.Deployments, configMaps repository.VendorValuesRepository, insecureRegistries []string) *DefaultRenderedValuesService {
	return &DefaultRenderedValuesService{
		deployments:        deployments,
		configMaps:         configMaps,
		charts:             NewOCIChartSchemaFetcher(),
		insecureRegistries: insecureRegistries,
	}
}

// SetChartFetcher replaces how charts are pulled (tests).
func (s *DefaultRenderedValuesService) SetChartFetcher(f ChartValuesFetcher) {
	s.charts = f
}

// GetRenderedValues lists the renders of an instance, the instance's own
// chart first (trino for a trino instance), then by ConfigMap name. An
// instance whose chart vendors nothing, or predates the values ConfigMaps,
// has none. A chart that cannot be pulled leaves the values without
// defaults (DefaultsError), it does not fail the request.
func (s *DefaultRenderedValuesService) GetRenderedValues(ctx context.Context, project, name string) ([]models.RenderedValues, error) {
	st, err := s.deployments.GetInstance(ctx, project, name)
	if err != nil {
		return nil, gitError(err, name)
	}
	inst := st.Instance
	maps, err := s.configMaps.List(ctx, project, inst.ReleaseName())
	if err != nil {
		return nil, err
	}
	result := make([]models.RenderedValues, 0, len(maps))
	for _, cm := range maps {
		version := serviceVersion(cm.HelmChart, inst.Service, inst.Version)
		rendered := models.RenderedValues{
			Name:           cm.Name,
			Chart:          cm.Chart,
			ChartVersion:   cm.ChartVersion,
			ServiceVersion: version,
			Values:         cm.Values,
			ChangedLines:   []int{},
		}
		defaults, err := s.vendoredValues(inst.Chart, version)
		switch {
		case err != nil:
			rendered.DefaultsError = fmt.Sprintf("could not read the defaults of %s from %s:%s: %v", cm.Chart, inst.Chart, version, err)
		case defaults[cm.Chart] == nil:
			rendered.DefaultsError = fmt.Sprintf("%s:%s has no vendor/%s/values.yaml", inst.Chart, version, cm.Chart)
		default:
			rendered.Defaults = string(defaults[cm.Chart])
			if lines, err := ChangedLines(cm.Values, defaults[cm.Chart]); err != nil {
				rendered.DefaultsError = "could not compare with the defaults: " + err.Error()
			} else {
				rendered.ChangedLines = lines
			}
		}
		result = append(result, rendered)
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].Chart == inst.Service && result[j].Chart != inst.Service
	})
	return result, nil
}

// serviceVersion reads the version of the instance's chart from the label
// helm.sh/chart (<chart>-<version>, "+" written "_") of the ConfigMap it
// rendered, falling back to the declared version.
func serviceVersion(helmChart, service, declared string) string {
	if v, ok := strings.CutPrefix(helmChart, service+"-"); ok && v != "" {
		return strings.ReplaceAll(v, "_", "+")
	}
	return declared
}

// vendoredValues returns the vendor/<name>/values.yaml files of a chart
// version, pulled once per version. Failures are not cached.
func (s *DefaultRenderedValuesService) vendoredValues(chart, version string) (map[string][]byte, error) {
	key := chart + ":" + version
	if e, ok := s.cache.Load(key); ok {
		entry := e.(*vendoredValuesEntry)
		if time.Since(entry.fetchedAt) < vendoredValuesTTL {
			return entry.values, nil
		}
	}
	shared, err, _ := s.inflight.Do(key, func() (any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), chartFetchTimeout)
		defer cancel()
		values, err := s.charts.FetchVendoredValues(ctx, strings.TrimPrefix(chart, "oci://"), version, insecureOCIHost(chart, s.insecureRegistries))
		if err != nil {
			return nil, err
		}
		s.cache.Store(key, &vendoredValuesEntry{values: values, fetchedAt: time.Now()})
		return values, nil
	})
	if err != nil {
		return nil, err
	}
	return shared.(map[string][]byte), nil
}
