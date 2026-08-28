package service

import (
	"encoding/json"
	"os"
	"testing"
)

func TestInputsFromMarkersReadsAConnectionRef(t *testing.T) {
	var schema map[string]any
	raw := `{
  "required": ["metadataDb"],
  "properties": {
    "workers": {"type": "integer"},
    "metadataDb": {"type": "string", "description": "Metadata database", "x-okdp-connection-ref": {"contract": "database-server"}},
    "pgConnection": {"type": "string", "default": "shared-db", "x-okdp-connection-ref": {"contract": "database-server"}},
    "legacy": {"type": "string", "x-kubocd-connection-ref": {"contract": "database-server"}}
  }
}`
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}

	inputs := inputsFromMarkers(schema)

	if len(inputs) != 2 {
		t.Fatalf("got %d inputs, want 2 (the KuboCD marker is gone): %+v", len(inputs), inputs)
	}
	// Sorted by parameter name.
	if inputs[0].Parameter != "metadataDb" || inputs[0].Path != "metadataDb" || inputs[0].Optional {
		t.Errorf("metadataDb = %+v, want required (listed in the parent's required array)", inputs[0])
	}
	if inputs[0].Contract != "database-server" || inputs[0].Description != "Metadata database" {
		t.Errorf("metadataDb detail = %+v", inputs[0])
	}
	if inputs[1].Parameter != "pgConnection" || !inputs[1].Optional || inputs[1].Default != "shared-db" {
		t.Errorf("pgConnection = %+v, want optional with its default", inputs[1])
	}
}

func TestInputsFromMarkersToleratesAChartWithout(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"workers": map[string]any{"type": "integer"}}}
	if got := inputsFromMarkers(schema); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParameterSchemaDropsThePlatformKeys(t *testing.T) {
	chart := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"global", "workers"},
		"properties": map[string]any{
			"global":      map[string]any{"type": "object"},
			"connections": map[string]any{"type": "object"},
			"workers":     map[string]any{"type": "integer", "title": "Workers", "x-ui-group": "Sizing"},
		},
	}
	got := parameterSchema(chart)
	props := got["properties"].(map[string]any)
	if _, ok := props["global"]; ok {
		t.Errorf("global is filled by the platform, not the form")
	}
	if _, ok := props["connections"]; ok {
		t.Errorf("connections are layered from files, not the form")
	}
	workers := props["workers"].(map[string]any)
	if workers["x-ui-group"] != "Sizing" || workers["title"] != "Workers" {
		t.Errorf("the UI keywords must pass through untouched: %v", workers)
	}
	if req := got["required"].([]any); len(req) != 1 || req[0] != "workers" {
		t.Errorf("required = %v", req)
	}
	// The chart's own document is not modified.
	if _, ok := chart["properties"].(map[string]any)["global"]; !ok {
		t.Errorf("parameterSchema mutated its input")
	}
}

// The trino chart of the platform-packages spike (a copy of its
// values.schema.json): refs live in the items of catalog lists.
func TestInputsFromMarkersWalksNestedRefs(t *testing.T) {
	raw, err := os.ReadFile("testdata/trino.values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var chart map[string]any
	if err := json.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	schema := parameterSchema(chart, "okdp-lib")
	for _, reserved := range []string{"global", "connections", "okdp-lib"} {
		if _, ok := schema["properties"].(map[string]any)[reserved]; ok {
			t.Errorf("%s is offered as a form field", reserved)
		}
	}

	inputs := inputsFromMarkers(schema)
	got := map[string]string{}
	for _, in := range inputs {
		got[in.Path] = in.Contract
		if in.Parameter != "" {
			t.Errorf("%s is nested, it names no root parameter: %+v", in.Path, in)
		}
	}
	want := map[string]string{
		"hiveCatalogs[].metastore":  "hive",
		"hiveCatalogs[].storage":    "s3",
		"icebergCatalogs[].catalog": "iceberg-catalog",
		"icebergCatalogs[].storage": "s3",
	}
	if len(got) != len(want) {
		t.Fatalf("inputs = %v, want %v", got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s: contract %q, want %q", p, got[p], c)
		}
	}
}
