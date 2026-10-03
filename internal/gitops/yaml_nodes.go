package gitops

import (
	"bytes"

	"gopkg.in/yaml.v3"
)

// yaml.Node builders: the output order is the declaration order.

func str(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// mapping takes alternating keys (string) and values (*yaml.Node).
func mapping(pairs ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(pairs); i += 2 {
		n.Content = append(n.Content, str(pairs[i].(string)), pairs[i+1].(*yaml.Node))
	}
	return n
}

// encodeDocuments renders nodes as YAML documents, two-space indented.
func encodeDocuments(docs ...*yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, d := range docs {
		if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{d}}); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
