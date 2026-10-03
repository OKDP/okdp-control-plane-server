package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func chartArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestValuesSchemaFromChartArchivePicksTheChartsOwn(t *testing.T) {
	archive := chartArchive(t, map[string]string{
		"trino/Chart.yaml":                    "name: trino\ndependencies:\n  - name: okdp-lib\n  - name: opa\n    alias: policy\n",
		"trino/charts/opa/values.schema.json": `{"title": "subchart"}`,
		"trino/values.schema.json":            `{"title": "trino"}`,
	})
	schema, err := valuesSchemaFromChartArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if schema.Schema["title"] != "trino" {
		t.Fatalf("got the schema titled %v", schema.Schema["title"])
	}
	if strings.Join(schema.Dependencies, ",") != "okdp-lib,policy" {
		t.Fatalf("dependencies = %v", schema.Dependencies)
	}

	_, err = valuesSchemaFromChartArchive(bytes.NewReader(chartArchive(t, map[string]string{"x/Chart.yaml": "name: x\n"})))
	if err == nil || !strings.Contains(err.Error(), "no values.schema.json") {
		t.Fatalf("err = %v", err)
	}
}

func TestVendoredValuesFromChartArchive(t *testing.T) {
	archive := chartArchive(t, map[string]string{
		"trino/values.yaml":                                 "workers: 1\n",
		"trino/vendor/trino/values.yaml":                    "image: trinodb/trino\n",
		"trino/vendor/trino/templates/x.yaml":               "kind: x\n",
		"trino/vendor/opa-kube-mgmt/values.yaml":            "replicas: 1\n",
		"trino/vendor/opa-kube-mgmt/charts/lib/values.yaml": "subchart: true\n",
	})
	values, err := vendoredValuesFromChartArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || string(values["trino"]) != "image: trinodb/trino\n" || string(values["opa-kube-mgmt"]) != "replicas: 1\n" {
		t.Fatalf("values = %q", values)
	}
}

// A minimal OCI distribution endpoint serving one chart, to exercise the pull
// the way a registry answers it.
func TestOCIChartSchemaFetcherPullsFromARegistry(t *testing.T) {
	chart := chartArchive(t, map[string]string{"hive/values.schema.json": `{"type": "object", "title": "hive"}`})
	config := []byte("{}")
	digest := func(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": digest(config), "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": helmChartContentMediaType, "digest": digest(chart), "size": len(chart)}},
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/okdp/charts/hive/manifests/1.0.0", "/v2/okdp/charts/hive/manifests/" + digest(manifest):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest(manifest))
			w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
			if r.Method != http.MethodHead {
				w.Write(manifest)
			}
		case "/v2/okdp/charts/hive/blobs/" + digest(chart):
			w.Header().Set("Content-Length", fmt.Sprint(len(chart)))
			w.Write(chart)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	host := strings.TrimPrefix(server.URL, "http://")
	schema, err := NewOCIChartSchemaFetcher().FetchValuesSchema(context.Background(), host+"/okdp/charts/hive", "1.0.0", true)
	if err != nil {
		t.Fatal(err)
	}
	if schema.Schema["title"] != "hive" {
		t.Fatalf("schema = %v", schema)
	}
}

// A few kilobytes of gzip inflate to far more than a chart: the per-file bound
// refuses the file from its header, before reading it.
func TestValuesSchemaFromChartArchiveBoundsTheInflatedFiles(t *testing.T) {
	for _, file := range []string{"x/values.schema.json", "x/Chart.yaml"} {
		huge := `{"title": "` + strings.Repeat(" ", maxChartFileSize) + `"}`
		archive := chartArchive(t, map[string]string{file: huge})
		if len(archive) > maxChartFileSize/10 {
			t.Fatalf("the test archive is %d bytes, not a bomb", len(archive))
		}
		_, err := valuesSchemaFromChartArchive(bytes.NewReader(archive))
		if err == nil || !strings.Contains(err.Error(), "more than") {
			t.Errorf("%s: err = %v", file, err)
		}
	}
}
