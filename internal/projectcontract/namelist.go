// Package projectcontract owns the shared shapes of the source-in-place
// project contract (infra-os style `project.yaml`). Compound has two readers of
// that file — internal/product, which derives product identity and deployment
// claims, and internal/runtimebinding, which derives runtime bindings. Anything
// both must agree about lives here, so one contract field cannot mean two
// things depending on which reader opened the file.
package projectcontract

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// NameList is a contract list whose entries may be written as a plain name or
// as a record about that name. Real contracts use all three shapes for the same
// field — `data.databases` appears as a bare string, as
// `{authority, env_var, database, host}`, and as `{aws_rds: "<prose>"}` — so a
// plain []string rejects the file outright and takes the whole product's
// deployment down with it.
//
// Normalization keeps the identifying name and discards the description:
//   - a scalar is the name
//   - a record's `database`, `name`, `id`, or `slug` field is the name
//   - a single-key record with none of those uses its key as the name
//
// A record that names nothing is an error rather than an empty entry: silently
// dropping it would under-report what a product depends on.
type NameList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (n *NameList) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Tag == "!!null" {
		*n = nil
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		name := strings.TrimSpace(value.Value)
		if name == "" {
			*n = nil
			return nil
		}
		*n = NameList{name}
		return nil
	}
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("expected a scalar or a list, got %s", nodeKind(value))
	}
	out := make(NameList, 0, len(value.Content))
	for i, item := range value.Content {
		name, err := entryName(item)
		if err != nil {
			return fmt.Errorf("entry %d: %w", i+1, err)
		}
		if name != "" {
			out = append(out, name)
		}
	}
	*n = out
	return nil
}

// Strings returns the normalized names as a plain slice for callers that hold
// the field as []string.
func (n NameList) Strings() []string { return []string(n) }

var nameKeys = []string{"database", "name", "id", "slug"}

func entryName(item *yaml.Node) (string, error) {
	switch item.Kind {
	case yaml.ScalarNode:
		return strings.TrimSpace(item.Value), nil
	case yaml.MappingNode:
		if len(item.Content)%2 != 0 {
			return "", fmt.Errorf("record must contain key-value pairs")
		}
		for _, key := range nameKeys {
			if value := mappingValue(item, key); value != "" {
				return value, nil
			}
		}
		if len(item.Content) == 2 {
			// `{aws_rds: "<prose>"}` — the key is the name, the value describes it.
			return strings.TrimSpace(item.Content[0].Value), nil
		}
		return "", fmt.Errorf("record names nothing: expected one of %s, or a single key", strings.Join(nameKeys, "/"))
	default:
		return "", fmt.Errorf("expected a name or a record, got %s", nodeKind(item))
	}
}

func mappingValue(mapping *yaml.Node, key string) string {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if !strings.EqualFold(strings.TrimSpace(mapping.Content[i].Value), key) {
			continue
		}
		if mapping.Content[i+1].Kind == yaml.ScalarNode {
			return strings.TrimSpace(mapping.Content[i+1].Value)
		}
	}
	return ""
}

func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a record"
	default:
		return "an unsupported node"
	}
}
