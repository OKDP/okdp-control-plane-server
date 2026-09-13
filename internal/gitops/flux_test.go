package gitops

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// The golden fixtures under testdata/sandbox are copied verbatim from
// okdp-sandbox/gitops (projects/ and platform/components/), which
// render-flux.sh leaves untouched: the reference output of the reference
// inputs. Refresh them with a plain copy when the format changes.
const goldenRoot = "testdata/sandbox"

func readGolden(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenRoot, rel))
	if err != nil {
		t.Fatalf("golden file: %v", err)
	}
	return data
}

func goldenInstanceDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(goldenRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == InstanceFile {
			rel, _ := filepath.Rel(goldenRoot, filepath.Dir(p))
			dirs = append(dirs, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no golden instances: %v", err)
	}
	return dirs
}

func TestFluxRenderingIsByteIdenticalToRenderFlux(t *testing.T) {
	renderer := NewDefaultFluxRenderer()
	for _, dir := range goldenInstanceDirs(t) {
		t.Run(dir, func(t *testing.T) {
			inst, err := DecodeInstance(readGolden(t, path.Join(dir, InstanceFile)))
			if err != nil {
				t.Fatal(err)
			}
			files, err := renderer.RenderInstance(*inst)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{HelmReleaseFile, KustomizationFile} {
				if want := readGolden(t, path.Join(dir, name)); string(files[name]) != string(want) {
					t.Errorf("%s/%s differs\n--- want\n%s\n--- got\n%s", dir, name, want, files[name])
				}
			}
			// The server writes instance.yaml the way a person does.
			encoded, err := EncodeInstance(*inst)
			if err != nil {
				t.Fatal(err)
			}
			if want := readGolden(t, path.Join(dir, InstanceFile)); string(encoded) != string(want) {
				t.Errorf("%s/instance.yaml re-encoded differently\n--- want\n%s\n--- got\n%s", dir, want, encoded)
			}
		})
	}
}

func TestProjectKustomizationIsByteIdenticalToRenderFlux(t *testing.T) {
	got, err := NewDefaultFluxRenderer().RenderProject("example", []string{"hello"}, []string{"example-s3"})
	if err != nil {
		t.Fatal(err)
	}
	if want := readGolden(t, "projects/example/kustomization.yaml"); string(got) != string(want) {
		t.Errorf("differs\n--- want\n%s\n--- got\n%s", want, got)
	}

	empty, _ := NewDefaultFluxRenderer().RenderProject("empty", nil, nil)
	want := FluxHeader + "\napiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: okdp-releases\nresources: []\nconfigMapGenerator: []\n"
	if string(empty) != want {
		t.Errorf("empty project:\n%s", empty)
	}
}

// goldenStore loads the fixture tree into a store.
func goldenStore(t *testing.T) *MemoryStore {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(goldenRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(goldenRoot, p)
		data, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewMemoryStore(files)
}

// Writes the server makes on the reference tree change nothing that they do
// not mean to change: the console and the script agree on every byte.
func TestServerWritesOnTheReferenceTreeChangeNothing(t *testing.T) {
	store := goldenStore(t)
	before := store.Files()
	d := NewDeployments(store, nil)
	ctx := context.Background()

	if _, _, err := d.UpdateInstance(ctx, alice, "example", "hello", func(*InstanceState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	conn, err := d.GetConnection(ctx, "example", "example-s3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpdateProject(ctx, alice, Project{Name: "example", Description: "Example project used to test both GitOps engines with a public chart."}); err != nil {
		t.Fatal(err)
	}
	after := store.Files()
	for p, content := range before {
		if after[p] != content {
			t.Errorf("%s was rewritten:\n--- before\n%s\n--- after\n%s", p, content, after[p])
		}
	}
	if len(store.Messages) != 0 {
		t.Errorf("no-op writes committed: %v", store.Messages)
	}
	if conn.Contract != "s3" || conn.SecretRef != "example-s3-credentials" || conn.Values["bucket"] != "example" {
		t.Errorf("reference connection read as %+v", conn)
	}
}

// The strongest check: run the reference script itself on a tree the server
// wrote, and expect it to change nothing. Needs the okdp-sandbox checkout next
// to this repository, bash and yq v4; skipped otherwise.
func TestRenderFluxLeavesTheServerOutputAlone(t *testing.T) {
	script, _ := filepath.Abs(filepath.Join("..", "..", "..", "okdp-sandbox", "gitops", "scripts", "render-flux.sh"))
	if _, err := os.Stat(script); err != nil {
		t.Skip("no okdp-sandbox checkout next to this repository")
	}
	if out, err := exec.Command("yq", "--version").CombinedOutput(); err != nil || !strings.Contains(string(out), "version v4") {
		t.Skip("yq v4 is not installed")
	}

	store := NewMemoryStore(nil)
	d := NewDeployments(store, nil)
	ctx := context.Background()
	if _, err := d.CreateProject(ctx, alice, Project{Name: "demo", Description: "Demo"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"warehouse", "lake"} {
		conn := Connection{Name: c, Project: "demo", Contract: "hive", Values: map[string]any{"thriftUri": "thrift://" + c + ":9083"}}
		if _, err := d.PutConnection(ctx, alice, conn, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range []InstanceState{
		{Instance: Instance{Name: "sql", Project: "demo", Service: "trino", Chart: "oci://quay.io/okdp/platform-charts/trino", Version: "480.0.0-p21", Connections: []string{"warehouse", "lake"}},
			Values: map[string]any{"workers": float64(2)}},
		{Instance: Instance{Name: "hive", Project: "demo", Service: "hive-metastore", Chart: "oci://quay.io/okdp/platform-charts/hive-metastore", Version: "4.0.1-p02"}},
	} {
		if _, err := d.CreateInstance(ctx, alice, st); err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	for p, content := range store.Files() {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("bash", script, "--root", root, "--path-prefix", "").CombinedOutput()
	if err != nil {
		t.Fatalf("render-flux.sh failed: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, "rendered projects/") || strings.HasPrefix(line, "removed projects/") {
			t.Errorf("render-flux.sh changed a file the server wrote: %s", line)
		}
	}
}

func TestInstanceValidationFollowsRenderFlux(t *testing.T) {
	ok := Instance{Name: "hello", Project: "example", Service: "app-template", Chart: "oci://ghcr.io/bjw-s-labs/helm/app-template", Version: "5.2.1"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("reference instance rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Instance){
		"version range":        func(i *Instance) { i.Version = "~5.2" },
		"two-part version":     func(i *Instance) { i.Version = "6.10" },
		"build metadata":       func(i *Instance) { i.Version = "5.2.1+abc" },
		"chart not oci":        func(i *Instance) { i.Chart = "https://charts.example.com/app-template" },
		"chart of another svc": func(i *Instance) { i.Chart = "oci://ghcr.io/x/other" },
		"bad service":          func(i *Instance) { i.Service = "App" },
		"duplicate connection": func(i *Instance) { i.Connections = []string{"a", "a"} },
		"release too long":     func(i *Instance) { i.Name = strings.Repeat("a", 50) },
	} {
		inst := ok
		mutate(&inst)
		if err := inst.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, inst)
		}
	}
}

func TestEncodeInstanceIsStable(t *testing.T) {
	data, err := EncodeInstance(Instance{Name: "hive", Project: "demo", Service: "hive-metastore", Chart: "oci://r/hive-metastore", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := "name: hive\nproject: demo\nservice: hive-metastore\nchart: oci://r/hive-metastore\nversion: 1.0.0\nconnections: []\n"
	if string(data) != want {
		t.Fatalf("instance.yaml:\n%s\nwant:\n%s", data, want)
	}
}
