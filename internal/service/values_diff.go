package service

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// ChangedLines returns the 1-based lines of values (the values.yaml of a
// values ConfigMap of okdp.vendor.render) that differ from defaults (the
// vendored chart's values.yaml): a key the defaults do not have, a scalar of
// another value, a list item past the end of the default list. A changed
// value marks all of its lines, from its key to the line before the next key;
// a map that differs only below marks only the lines that differ.
//
// Values are compared as Helm reads them: defaults are decoded the way Helm
// decodes values files (YAML 1.1 booleans such as yes/on), and numbers are
// compared as numbers (1.0 and 1 are equal, Helm prints both as 1).
func ChangedLines(values string, defaults []byte) ([]int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return []int{}, nil
	}
	var def any
	if err := sigsyaml.Unmarshal(defaults, &def); err != nil {
		return nil, errors.New("the default values do not parse: " + err.Error())
	}
	if def == nil {
		def = map[string]any{}
	}
	last := strings.Count(strings.TrimRight(values, "\n"), "\n") + 1
	d := &lineDiff{changed: map[int]bool{}}
	d.mark(doc.Content[0], def, true, 1, last)
	lines := make([]int, 0, len(d.changed))
	for l := 1; l <= last; l++ {
		if d.changed[l] {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

type lineDiff struct {
	changed map[int]bool
}

func (d *lineDiff) markRange(from, to int) {
	if to < from {
		to = from
	}
	for l := from; l <= to; l++ {
		d.changed[l] = true
	}
}

// mark compares node, which spans lines from..to of the document, with def.
func (d *lineDiff) mark(node *yaml.Node, def any, hasDef bool, from, to int) {
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	if !hasDef {
		d.markRange(from, to)
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		defMap, ok := def.(map[string]any)
		if !ok {
			d.markRange(from, to)
			return
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			end := to
			if i+2 < len(node.Content) {
				end = node.Content[i+2].Line - 1
			}
			child, has := defMap[key.Value]
			d.mark(value, child, has, key.Line, end)
		}
	case yaml.SequenceNode:
		defList, ok := def.([]any)
		if !ok {
			d.markRange(from, to)
			return
		}
		if equalValues(node, def) {
			return
		}
		for i, item := range node.Content {
			// Items of a flow list share a line: markRange widens the
			// resulting empty range to that line.
			start, end := item.Line, to
			if i+1 < len(node.Content) {
				end = node.Content[i+1].Line - 1
			}
			if i < len(defList) {
				d.mark(item, defList[i], true, start, end)
			} else {
				d.markRange(start, end)
			}
		}
		if len(node.Content) < len(defList) {
			// Items removed (or the list emptied): nothing left to point at
			// but the list's own line.
			d.markRange(from, from)
		}
	default:
		if !equalValues(node, def) {
			d.markRange(from, to)
		}
	}
}

// equalValues compares a YAML node with a decoded default, as JSON values.
func equalValues(node *yaml.Node, def any) bool {
	var v any
	if err := node.Decode(&v); err != nil {
		return false
	}
	return reflect.DeepEqual(normalizeJSON(v), normalizeJSON(def))
}

// normalizeJSON round-trips a value through JSON: numbers become float64 and
// maps map[string]any, on both sides of a comparison.
func normalizeJSON(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return v
	}
	return out
}
