package gitops

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func trino() InstanceState {
	return InstanceState{
		Instance: Instance{Name: "trino", Project: "demo", Service: "trino", Chart: "oci://quay.io/okdp/platform-charts/trino", Version: "476-1.0.0", Connections: []string{"lake"}},
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
		"projects/demo/connections/kustomization.yaml",
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
	if _, ok := store.Files()["projects/demo/connections/kustomization.yaml"]; ok {
		t.Errorf("the connections kustomization outlived the last connection")
	}

	if _, err := d.DeleteInstance(ctx, "bob", "demo", "trino"); err != nil {
		t.Fatal(err)
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
