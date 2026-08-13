package service

import "testing"

// prop runs one title through the parser and hands back the expanded property.
func prop(t *testing.T, title string) map[string]any {
	t.Helper()
	out := parseTitleMetadata(map[string]any{
		"properties": map[string]any{
			"field": map[string]any{"type": "string", "title": title},
		},
	})
	props, ok := out["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties to survive the copy")
	}
	p, ok := props["field"].(map[string]any)
	if !ok {
		t.Fatal("expected the property to survive the copy")
	}
	return p
}

func TestTitleExpandsIntoUIHints(t *testing.T) {
	p := prop(t, "Storage | Storage Size | stepper | order:1 columns:2 advanced:true")

	for field, want := range map[string]any{
		"x-ui-group":   "Storage",
		"title":        "Storage Size",
		"x-ui-widget":  "stepper",
		"x-ui-order":   1,
		"x-ui-columns": 2,
	} {
		if got := p[field]; got != want {
			t.Errorf("%s: expected %v, got %v", field, want, got)
		}
	}
	if p["x-ui-advanced"] != true {
		t.Errorf("x-ui-advanced: expected true, got %v", p["x-ui-advanced"])
	}
}

// The reason for splitOptions. Every placeholder written in the wild holds a
// space, and splitting on it left the form suggesting "e.g." and nothing else.
func TestQuotedOptionKeepsItsSpaces(t *testing.T) {
	p := prop(t, `Storage | Storage Size | | order:1 placeholder:"e.g. 10Gi, 50Gi" columns:2`)

	if got := p["x-ui-placeholder"]; got != "e.g. 10Gi, 50Gi" {
		t.Errorf("expected the whole placeholder, got %v", got)
	}
	// The options after the quoted one must still be read.
	if got := p["x-ui-columns"]; got != 2 {
		t.Errorf("expected columns to survive the quoted value, got %v", got)
	}
}

// Unquoted options keep the old behaviour, so nothing already published moves.
func TestUnquotedOptionsSplitAsBefore(t *testing.T) {
	p := prop(t, "Auth | Session Timeout | | order:3 advanced:true")

	if got := p["x-ui-order"]; got != 3 {
		t.Errorf("expected order 3, got %v", got)
	}
	if got := p["x-ui-advanced"]; got != true {
		t.Errorf("expected advanced true, got %v", got)
	}
}

func TestConditionBecomesFieldAndValue(t *testing.T) {
	p := prop(t, "Storage | Shared Volume Size | | condition:enableSharedVolume=true")

	cond, ok := p["x-ui-condition"].(map[string]any)
	if !ok {
		t.Fatalf("expected a condition map, got %#v", p["x-ui-condition"])
	}
	if cond["field"] != "enableSharedVolume" || cond["value"] != true {
		t.Errorf("expected enableSharedVolume=true, got %#v", cond)
	}
}

// A title without a pipe is a plain label and must not grow UI hints.
func TestPlainTitleIsLeftAlone(t *testing.T) {
	p := prop(t, "Storage Size")

	if p["title"] != "Storage Size" {
		t.Errorf("expected the title untouched, got %v", p["title"])
	}
	for _, field := range []string{"x-ui-group", "x-ui-order", "x-ui-widget"} {
		if _, present := p[field]; present {
			t.Errorf("expected no %s on a plain title", field)
		}
	}
}

func TestUnbalancedQuoteKeepsTheOtherOptions(t *testing.T) {
	p := prop(t, `Storage | Size | | order:1 placeholder:"e.g. 10Gi columns:2`)

	if got := p["x-ui-columns"]; got != 2 {
		t.Errorf("expected columns to survive an unbalanced quote, got %v", got)
	}
	if got := p["x-ui-order"]; got != 1 {
		t.Errorf("expected order 1, got %v", got)
	}
}
