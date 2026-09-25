package service

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// ChartSchema is what the console needs from a chart version.
type ChartSchema struct {
	// Schema is the chart's root values.schema.json.
	Schema map[string]any
	// Dependencies are the values keys of the chart's dependencies (alias,
	// else name, from Chart.yaml): okdp-lib and the former modules. They are
	// the chart's business, never user parameters.
	Dependencies []string
}

// ChartSchemaFetcher reads the values.schema.json of a chart version.
type ChartSchemaFetcher interface {
	// FetchValuesSchema pulls repository:tag (repository without scheme, e.g.
	// quay.io/okdp/platform-charts/trino) and reads its root values.schema.json
	// and Chart.yaml.
	FetchValuesSchema(ctx context.Context, repository, tag string, plainHTTP bool) (*ChartSchema, error)
}

// Media type of the layer holding a Helm chart archive in an OCI registry.
const helmChartContentMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// maxChartSize bounds what a chart pull may read: charts are kilobytes, a
// registry serving gigabytes under that media type is not one.
const maxChartSize = 32 << 20

// maxChartUncompressedSize bounds what the archive may inflate to, and
// maxChartFileSize each file read from it: maxChartSize only limits the
// compressed stream, and a gzip bomb fits in a few kilobytes of it.
const (
	maxChartUncompressedSize = 256 << 20
	maxChartFileSize         = 4 << 20
)

// OCIChartSchemaFetcher pulls Helm charts from an OCI registry with oras-go,
// anonymously (public registries grant pull tokens without credentials).
type OCIChartSchemaFetcher struct {
	client remote.Client
}

func NewOCIChartSchemaFetcher() *OCIChartSchemaFetcher {
	return &OCIChartSchemaFetcher{client: &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache()}}
}

func (f *OCIChartSchemaFetcher) FetchValuesSchema(ctx context.Context, repository, tag string, plainHTTP bool) (*ChartSchema, error) {
	repo, err := remote.NewRepository(strings.TrimPrefix(repository, "oci://"))
	if err != nil {
		return nil, err
	}
	repo.PlainHTTP = plainHTTP
	repo.Client = f.client

	manifestDesc, rc, err := repo.FetchReference(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("resolving the chart manifest: %w", err)
	}
	manifestBytes, err := content.ReadAll(rc, manifestDesc)
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("reading the chart manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing the chart manifest: %w", err)
	}

	for _, layer := range manifest.Layers {
		if layer.MediaType != helmChartContentMediaType {
			continue
		}
		if layer.Size > maxChartSize {
			return nil, fmt.Errorf("the chart layer is %d bytes, more than the %d a chart may weigh", layer.Size, maxChartSize)
		}
		blob, err := repo.Fetch(ctx, layer)
		if err != nil {
			return nil, fmt.Errorf("pulling the chart: %w", err)
		}
		defer blob.Close()
		return valuesSchemaFromChartArchive(io.LimitReader(blob, maxChartSize))
	}
	return nil, errors.New("the artifact holds no Helm chart layer")
}

// valuesSchemaFromChartArchive extracts <chart>/values.schema.json and the
// dependencies of <chart>/Chart.yaml from a chart .tgz. Subcharts carry their
// own under <chart>/charts/, which are not the chart's.
func valuesSchemaFromChartArchive(r io.Reader) (*ChartSchema, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("the chart is not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(gz, maxChartUncompressedSize))
	result := &ChartSchema{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the chart archive: %w", err)
		}
		name := path.Clean(hdr.Name)
		dir, file := path.Split(name)
		if dir == "" || strings.Count(strings.Trim(dir, "/"), "/") != 0 {
			continue
		}
		if (file == "values.schema.json" || file == "Chart.yaml") && hdr.Size > maxChartFileSize {
			return nil, fmt.Errorf("%s is %d bytes, more than the %d it may weigh", name, hdr.Size, maxChartFileSize)
		}
		switch file {
		case "values.schema.json":
			if err := json.NewDecoder(io.LimitReader(tr, maxChartFileSize)).Decode(&result.Schema); err != nil {
				return nil, fmt.Errorf("invalid values.schema.json: %w", err)
			}
		case "Chart.yaml":
			var chart struct {
				Dependencies []struct {
					Name  string `yaml:"name"`
					Alias string `yaml:"alias"`
				} `yaml:"dependencies"`
			}
			data, err := readBounded(tr, maxChartFileSize)
			if err != nil {
				return nil, fmt.Errorf("reading Chart.yaml: %w", err)
			}
			if err := yaml.Unmarshal(data, &chart); err != nil {
				return nil, fmt.Errorf("invalid Chart.yaml: %w", err)
			}
			for _, d := range chart.Dependencies {
				key := d.Alias
				if key == "" {
					key = d.Name
				}
				result.Dependencies = append(result.Dependencies, key)
			}
		}
	}
	if result.Schema == nil {
		return nil, errors.New("the chart has no values.schema.json")
	}
	return result, nil
}
