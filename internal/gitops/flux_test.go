package gitops

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The golden files pin the Flux format byte for byte. They are the server's
// rendering of the contract description until okdp-sandbox publishes
// gitops/scripts/render-flux.sh; then they must be copied from its output
// (see the TODO on DefaultFluxRenderer).
func TestFluxRenderingMatchesTheGoldenFiles(t *testing.T) {
	renderer := NewDefaultFluxRenderer()
	cases := map[string]Instance{
		"demo-trino": {
			Name: "trino", Project: "demo", Service: "trino",
			Chart: "oci://quay.io/okdp/platform-charts/trino", Version: "476-1.0.0",
			Connections: []string{"lake", "warehouse"},
		},
		"demo-hive": {
			Name: "hive", Project: "demo", Service: "hive-metastore",
			Chart: "oci://quay.io/okdp/platform-charts/hive-metastore", Version: "1.0",
		},
	}
	for name, inst := range cases {
		files, err := renderer.RenderInstance(inst)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for file, got := range files {
			golden(t, filepath.Join("testdata", "flux", name, file), got)
		}
	}

	files, err := renderer.RenderConnections("demo", []string{"lake", "warehouse"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, filepath.Join("testdata", "flux", "demo-connections", KustomizationFile), files[KustomizationFile])
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden file: %v (run go test -update)", err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

func TestEncodeInstanceIsStable(t *testing.T) {
	data, err := EncodeInstance(Instance{Name: "hive", Project: "demo", Service: "hive-metastore", Chart: "oci://r/hive-metastore", Version: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := "name: hive\nproject: demo\nservice: hive-metastore\nchart: oci://r/hive-metastore\nversion: \"1.0\"\nconnections: []\n"
	if string(data) != want {
		t.Fatalf("instance.yaml:\n%s\nwant:\n%s", data, want)
	}
}
