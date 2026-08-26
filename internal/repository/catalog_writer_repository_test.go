package repository

import (
	"context"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

func TestPlatformServiceToMap(t *testing.T) {
	false_ := false
	true_ := true

	t.Run("serializes label and exposesUI when set", func(t *testing.T) {
		m := platformServiceToMap(models.PlatformService{
			Name:           "trino",
			Versions:       []string{"0.3.0", "0.2.0"},
			DefaultVersion: "0.3.0",
			Label:          "Trino SQL",
			ExposesUI:      &false_,
		})
		if m["label"] != "Trino SQL" {
			t.Errorf("label = %v, want %q", m["label"], "Trino SQL")
		}
		if m["exposesUI"] != false {
			t.Errorf("exposesUI = %v, want false", m["exposesUI"])
		}
		versions, ok := m["versions"].([]interface{})
		if !ok || len(versions) != 2 || versions[0] != "0.3.0" {
			t.Errorf("versions = %v, want [0.3.0 0.2.0]", m["versions"])
		}
		if m["default"] != "0.3.0" {
			t.Errorf("default = %v, want 0.3.0", m["default"])
		}
	})

	t.Run("omits label and exposesUI when unset", func(t *testing.T) {
		m := platformServiceToMap(models.PlatformService{
			Name:           "seaweedfs",
			Versions:       []string{"0.1.0"},
			DefaultVersion: "0.1.0",
		})
		if _, ok := m["label"]; ok {
			t.Errorf("label should be omitted when empty, got %v", m["label"])
		}
		if _, ok := m["exposesUI"]; ok {
			t.Errorf("exposesUI should be omitted when nil, got %v", m["exposesUI"])
		}
	})

	t.Run("writes exposesUI true explicitly", func(t *testing.T) {
		m := platformServiceToMap(models.PlatformService{
			Name:           "jupyterhub",
			Versions:       []string{"0.6.0"},
			DefaultVersion: "0.6.0",
			ExposesUI:      &true_,
		})
		if m["exposesUI"] != true {
			t.Errorf("exposesUI = %v, want true", m["exposesUI"])
		}
	})
}

// newWriterWith builds a catalog in Git whose categories are the given list,
// and a writer bound to it.
func newWriterWith(t *testing.T, categories []interface{}) (CatalogWriterRepository, func() []interface{}) {
	t.Helper()
	raw, err := gitops.MarshalYAML(map[string]interface{}{"defaultRepository": "quay.io/okdp/platform-charts", "categories": categories})
	if err != nil {
		t.Fatal(err)
	}
	store := gitops.NewMemoryStore(map[string]string{gitops.CatalogPath: string(raw)})
	d := gitops.NewDeployments(store, nil)
	w := NewCatalogWriterRepository(d)
	read := func() []interface{} {
		doc, err := d.ReadCatalog(context.Background())
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if doc["defaultRepository"] != "quay.io/okdp/platform-charts" {
			t.Fatalf("the writer lost the rest of the catalog: %v", doc)
		}
		cats, _ := doc["categories"].([]interface{})
		return cats
	}
	return w, read
}

func namesIn(cats []interface{}, title string) []string {
	for _, raw := range cats {
		cat := raw.(map[string]interface{})
		if cat["title"] != title {
			continue
		}
		var names []string
		for _, s := range categoryServices(cat) {
			names = append(names, s.(map[string]interface{})["name"].(string))
		}
		return names
	}
	return nil
}

func TestCatalogWriterNestsServicesUnderTheirSection(t *testing.T) {
	w, read := newWriterWith(t, []interface{}{
		map[string]interface{}{"title": "Data Catalog", "icon": "pi-database", "services": []interface{}{
			map[string]interface{}{"name": "hive-metastore", "versions": []interface{}{"4.0.1"}, "default": "4.0.1"},
		}},
	})
	ctx := context.Background()

	// Add into an existing section.
	if err := w.AddPlatformService(ctx, models.PlatformService{Name: "polaris", Versions: []string{"1.3.0"}, DefaultVersion: "1.3.0", Category: "Data Catalog"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := namesIn(read(), "Data Catalog"); len(got) != 2 || got[1] != "polaris" {
		t.Errorf("after add, Data Catalog = %v, want [hive-metastore polaris]", got)
	}

	// Add into a section that does not exist yet: it is created at the end.
	if err := w.AddPlatformService(ctx, models.PlatformService{Name: "trino", Versions: []string{"480"}, DefaultVersion: "480", Category: "Interactive Query"}); err != nil {
		t.Fatalf("add new section: %v", err)
	}
	cats := read()
	if len(cats) != 2 || cats[1].(map[string]interface{})["title"] != "Interactive Query" {
		t.Fatalf("after add, categories = %v, want a second section Interactive Query", cats)
	}

	// Update moves the service when its category changes.
	if err := w.UpdatePlatformService(ctx, "polaris", models.PlatformService{Name: "polaris", Versions: []string{"1.3.0"}, DefaultVersion: "1.3.0", Category: "Interactive Query"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	cats = read()
	if got := namesIn(cats, "Data Catalog"); len(got) != 1 || got[0] != "hive-metastore" {
		t.Errorf("after move, Data Catalog = %v, want [hive-metastore]", got)
	}
	if got := namesIn(cats, "Interactive Query"); len(got) != 2 || got[1] != "polaris" {
		t.Errorf("after move, Interactive Query = %v, want [trino polaris]", got)
	}

	// Remove drops it from its section and keeps the section.
	if err := w.RemovePlatformService(ctx, "trino"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	cats = read()
	if got := namesIn(cats, "Interactive Query"); len(got) != 1 || got[0] != "polaris" {
		t.Errorf("after remove, Interactive Query = %v, want [polaris]", got)
	}
	if len(cats) != 2 {
		t.Errorf("sections = %d, want 2 (an emptied section is kept)", len(cats))
	}
}

func TestPlatformServiceToMapDoesNotRepeatTheCategory(t *testing.T) {
	m := platformServiceToMap(models.PlatformService{Name: "trino", Versions: []string{"480"}, DefaultVersion: "480", Category: "Interactive Query", Icon: "pi-bolt"})
	if _, ok := m["category"]; ok {
		t.Errorf("category must not be written on the service, the section carries it: %v", m)
	}
	if _, ok := m["icon"]; ok {
		t.Errorf("icon is not part of the service shape: %v", m)
	}
}
