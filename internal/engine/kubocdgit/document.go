package kubocdgit

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// document is a YAML manifest edited in place: setting a field keeps the
// other fields, their order and their comments.
type document struct {
	node yaml.Node
}

func parseDocument(data []byte) (*document, error) {
	d := &document{}
	if err := yaml.Unmarshal(data, &d.node); err != nil {
		return nil, err
	}
	if d.node.Kind != yaml.DocumentNode || len(d.node.Content) != 1 || d.node.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	return d, nil
}

func (d *document) root() *yaml.Node { return d.node.Content[0] }

// set writes value at path, creating the missing mappings on the way. A
// replaced value keeps the comments of the one it replaces.
func (d *document) set(value any, path ...string) error {
	var v yaml.Node
	if err := v.Encode(value); err != nil {
		return fmt.Errorf("encode %v: %w", path, err)
	}
	m := d.root()
	for i, key := range path {
		idx := keyIndex(m, key)
		if i == len(path)-1 {
			if idx < 0 {
				m.Content = append(m.Content, keyNode(key), &v)
				return nil
			}
			old := m.Content[idx+1]
			v.HeadComment, v.LineComment, v.FootComment = old.HeadComment, old.LineComment, old.FootComment
			m.Content[idx+1] = &v
			return nil
		}
		if idx >= 0 && m.Content[idx+1].Kind == yaml.MappingNode {
			m = m.Content[idx+1]
			continue
		}
		child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		if idx < 0 {
			m.Content = append(m.Content, keyNode(key), child)
		} else {
			m.Content[idx+1] = child
		}
		m = child
	}
	return nil
}

// unset removes the field at path, when it exists.
func (d *document) unset(path ...string) {
	m := d.root()
	for i, key := range path {
		idx := keyIndex(m, key)
		if idx < 0 {
			return
		}
		if i == len(path)-1 {
			m.Content = append(m.Content[:idx], m.Content[idx+2:]...)
			return
		}
		m = m.Content[idx+1]
		if m.Kind != yaml.MappingNode {
			return
		}
	}
}

// unsetKeys removes the keys match accepts from the mapping at path, when it
// exists.
func (d *document) unsetKeys(match func(string) bool, path ...string) {
	m := d.root()
	for _, key := range path {
		idx := keyIndex(m, key)
		if idx < 0 || m.Content[idx+1].Kind != yaml.MappingNode {
			return
		}
		m = m.Content[idx+1]
	}
	kept := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if !match(m.Content[i].Value) {
			kept = append(kept, m.Content[i], m.Content[i+1])
		}
	}
	m.Content = kept
}

func (d *document) bytes() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&d.node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func keyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

// decode reads a manifest as JSON values (float64 numbers, map[string]any),
// the shape Kubernetes objects have once decoded. A file holding more than
// one document is refused: reading the first one only would hide the others
// from every decision taken on the file, while the sync tool applies them all.
func decode(data []byte) (map[string]any, error) {
	if err := singleDocument(data); err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := sigsyaml.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("empty manifest")
	}
	return obj, nil
}

func singleDocument(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	documents := 0
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(node.Content) == 0 {
			continue
		}
		if documents++; documents > 1 {
			return errors.New("the file holds more than one YAML document")
		}
	}
}

// render writes a new manifest with the same 2-space indentation as
// document.bytes, so a later edit only touches the changed lines. Keys are
// sorted, so the same object always gives the same bytes, which a replayed
// idempotent write relies on.
func render(obj map[string]any) ([]byte, error) {
	raw, err := sigsyaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(raw)
	if err != nil {
		return nil, err
	}
	return doc.bytes()
}
