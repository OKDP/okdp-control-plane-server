package service

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInputsOfReadsAPackageDeclaration(t *testing.T) {
	var doc map[string]any
	raw := `
inputs:
  - contract: s3
    namedConnection:
      name: '{{ .Parameters.s3Connection | default "-" }}'
    alias: storage
    optional: "true"
  - contract: postgresql
    namedConnection:
      name: "{{ .Parameters.pgConnection }}"
  - contract: trino
    release:
      name: other
`
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}

	inputs := inputsOf(doc)
	if len(inputs) != 3 {
		t.Fatalf("got %d inputs, want 3", len(inputs))
	}
	if inputs[0].Alias != "storage" || inputs[0].Parameter != "s3Connection" || !inputs[0].Optional {
		t.Errorf("input 0 = %+v", inputs[0])
	}
	// Alias defaults to the contract name.
	if inputs[1].Alias != "postgresql" || inputs[1].Parameter != "pgConnection" {
		t.Errorf("input 1 = %+v", inputs[1])
	}
	// Bound to a release output: the user has no choice to make, so no parameter.
	if inputs[2].Parameter != "" {
		t.Errorf("input 2 should offer no choice: %+v", inputs[2])
	}
}

func TestInputsOfToleratesAPackageWithout(t *testing.T) {
	if got := inputsOf(map[string]any{"schema": map[string]any{}}); got != nil {
		t.Errorf("got %v, want nil, no inputs is the normal case", got)
	}
}

func TestInputsFromMarkersReadsAConnectionRef(t *testing.T) {
	var doc map[string]any
	raw := `
schema:
  parameters:
    required: [metadataDb]
    properties:
      workers: { type: integer }
      metadataDb:
        type: string
        description: "Metadata database"
        x-kubocd-connection-ref: { contract: postgresql }
      pgConnection:
        type: string
        x-kubocd-connection-ref: { contract: postgresql }
      databases:
        type: string
        x-kubocd-something-else: { contract: postgresql, required: true }
`
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}

	inputs := inputsFromMarkers(doc)

	if len(inputs) != 2 {
		t.Fatalf("got %d inputs, want 2 (a marker we do not know is not an input): %+v", len(inputs), inputs)
	}
	// Sorted by parameter name.
	if inputs[0].Parameter != "metadataDb" || inputs[0].Optional {
		t.Errorf("metadataDb = %+v, want required (listed in the parent's required array)", inputs[0])
	}
	if inputs[0].Contract != "postgresql" || inputs[0].Description != "Metadata database" {
		t.Errorf("metadataDb detail = %+v", inputs[0])
	}
	if inputs[1].Parameter != "pgConnection" || !inputs[1].Optional {
		t.Errorf("pgConnection = %+v, want optional", inputs[1])
	}
}

func TestInputsFromMarkersToleratesAPackageWithout(t *testing.T) {
	var doc map[string]any
	raw := `
schema:
  parameters:
    properties:
      workers: { type: integer }
`
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	if got := inputsFromMarkers(doc); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
