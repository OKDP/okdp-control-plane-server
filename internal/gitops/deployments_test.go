package gitops

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func trino() InstanceState {
	return InstanceState{
		Instance: Instance{Name: "trino", Project: "demo", Service: "trino", Chart: "oci://quay.io/okdp/platform-charts/trino", Version: "480.0.0-p21", Connections: []string{"lake"}},
		Values:   map[string]any{"workers": float64(2)},
	}
}

func TestInstanceLifecycleWritesTheContractFiles(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore(nil)
	d := NewDeployments(store, nil)

	lake := Connection{Name: "lake", Project: "demo", Contract: "s3", Description: "Data lake\nbucket", Values: map[string]any{"url": "https://s3", "region": "eu"}, SecretRef: "lake-credentials"}
	if _, err := d.PutConnection(ctx, "alice", lake, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateInstance(ctx, "alice", trino()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateInstance(ctx, "alice", trino()); !errors.Is(err, ErrExists) {
		t.Fatalf("second create: %v, want ErrExists", err)
	}

	files := store.Files()
	for _, p := range []string{
		"projects/demo/services/trino/instance.yaml",
		"projects/demo/services/trino/values.yaml",
		"projects/demo/services/trino/helmrelease.yaml",
		"projects/demo/services/trino/kustomization.yaml",
		"projects/demo/connections/lake.yaml",
		"projects/demo/kustomization.yaml",
	} {
		if _, ok := files[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	if got := files["projects/demo/services/trino/values.yaml"]; got != "workers: 2\n" {
		t.Errorf("values.yaml = %q", got)
	}
	wantConn := "# description: Data lake bucket\nconnections:\n  lake:\n    contract: s3\n    region: eu\n    url: https://s3\n    secretRef:\n      name: lake-credentials\n"
	if got := files["projects/demo/connections/lake.yaml"]; got != wantConn {
		t.Errorf("lake.yaml =\n%s\nwant\n%s", got, wantConn)
	}
	back, err := d.GetConnection(ctx, "demo", "lake")
	if err != nil || back.Description != "Data lake bucket" || back.SecretRef != "lake-credentials" || back.Values["region"] != "eu" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if store.Messages[1] != "okdp: deploy demo/trino by alice" {
		t.Errorf("commit message %q", store.Messages[1])
	}

	// In use: the file cannot go while trino layers it in.
	var inUse *ErrInUse
	if _, err := d.DeleteConnection(ctx, "alice", "demo", "lake"); !errors.As(err, &inUse) || inUse.Users[0] != "trino" {
		t.Fatalf("delete in-use connection: %v", err)
	}

	st, _, err := d.UpdateInstance(ctx, "bob", "demo", "trino", func(st *InstanceState) error {
		st.Values["workers"] = float64(3)
		st.Instance.Connections = nil
		return nil
	})
	if err != nil || st.Values["workers"] != float64(3) {
		t.Fatalf("update: %v %+v", err, st)
	}
	if strings.Contains(store.Files()["projects/demo/services/trino/helmrelease.yaml"], "conn-demo-lake") {
		t.Errorf("the generated HelmRelease was not rewritten")
	}

	if _, err := d.DeleteConnection(ctx, "alice", "demo", "lake"); err != nil {
		t.Fatalf("delete unused connection: %v", err)
	}
	if got := store.Files()["projects/demo/kustomization.yaml"]; !strings.Contains(got, "configMapGenerator: []") || !strings.Contains(got, "- services/trino") {
		t.Errorf("the project kustomization was not rewritten:\n%s", got)
	}

	if _, err := d.DeleteInstance(ctx, "bob", "demo", "trino"); err != nil {
		t.Fatal(err)
	}
	if got := store.Files()["projects/demo/kustomization.yaml"]; !strings.Contains(got, "resources: []") {
		t.Errorf("the deleted instance is still listed:\n%s", got)
	}
	if _, err := d.GetInstance(ctx, "demo", "trino"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if _, err := d.DeleteInstance(ctx, "bob", "demo", "trino"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestInstanceRefusesAnUndeclaredConnection(t *testing.T) {
	d := NewDeployments(NewMemoryStore(nil), nil)
	if _, err := d.CreateInstance(context.Background(), "alice", trino()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdateCatalogKeepsWhatItDoesNotTouch(t *testing.T) {
	store := NewMemoryStore(map[string]string{CatalogPath: "defaultRepository: quay.io/okdp/platform-charts\ncategories: []\n"})
	d := NewDeployments(store, nil)
	_, err := d.UpdateCatalog(context.Background(), "alice", "trino", func(c map[string]any) error {
		c["categories"] = []any{map[string]any{"title": "SQL"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.ReadCatalog(context.Background())
	if got["defaultRepository"] != "quay.io/okdp/platform-charts" || len(got["categories"].([]any)) != 1 {
		t.Fatalf("catalog = %v", got)
	}
}

// Project a-b instance c and project a instance b-c are both release a-b-c,
// and both engines would fight over it.
func TestCreateRefusesACollidingReleaseName(t *testing.T) {
	d := NewDeployments(NewMemoryStore(nil), nil)
	ctx := context.Background()
	mk := func(project, name string) InstanceState {
		return InstanceState{Instance: Instance{Name: name, Project: project, Service: "hive-metastore", Chart: "oci://r/hive-metastore", Version: "1.0.0"}}
	}
	if _, err := d.CreateInstance(ctx, "alice", mk("a-b", "c")); err != nil {
		t.Fatal(err)
	}
	var taken *ErrReleaseTaken
	_, err := d.CreateInstance(ctx, "alice", mk("a", "b-c"))
	if !errors.As(err, &taken) || errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrReleaseTaken and not ErrExists", err)
	}
	if err.Error() != "release name 'a-b-c' is already used by instance 'c' of project 'a-b'" {
		t.Fatalf("message = %q", err.Error())
	}
	if _, err := d.PutConnection(ctx, "alice", Connection{Name: "c", Project: "a-b", Contract: "hive"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PutConnection(ctx, "alice", Connection{Name: "b-c", Project: "a", Contract: "hive"}, true); !errors.Is(err, ErrExists) {
		t.Fatalf("conn- ConfigMap collision: err = %v, want ErrExists", err)
	}
}

func TestUpdateCatalogKeepsTheCommentsOfTheFile(t *testing.T) {
	raw := "# Licence header.\n\n# Console service catalog.\ndefaultRepository: oci://quay.io/okdp/platform-charts # the registry\ncategories:\n  - title: SQL\n    services: []\n"
	store := NewMemoryStore(map[string]string{CatalogPath: raw})
	d := NewDeployments(store, nil)
	_, err := d.UpdateCatalog(context.Background(), "alice", "trino", func(c map[string]any) error {
		c["categories"] = []any{map[string]any{"title": "SQL", "services": []any{map[string]any{"name": "trino", "versions": []any{"480.0.0-p21"}, "default": "480.0.0-p21"}}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := store.Files()[CatalogPath]
	for _, want := range []string{"# Licence header.", "# Console service catalog.", "# the registry", "name: trino"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lost:\n%s", want, got)
		}
	}
}
