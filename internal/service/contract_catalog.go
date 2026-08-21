package service

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// contractsFS holds the built-in contracts. Adding one is a matter of dropping
// a JSON file here: the same descriptor drives the form rendered by the
// console, the server-side validation of submitted values, and the address
// shown for a connection of that contract.
//
//go:embed contracts/*.json
var contractsFS embed.FS

// ContractCatalog exposes the known contracts.
//
// It is deliberately an interface: once the KuboCD `Contract` CRD is available
// on the cluster, an implementation backed by it can be layered on so that
// cluster-provided schemas take precedence over the built-in ones, without any
// change to the callers or to the API contract.
type ContractCatalog interface {
	List() []models.ContractDescriptor
	Get(name string) (*models.ContractDescriptor, bool)
	// Validate checks submitted values against the contract descriptor.
	Validate(typeName string, values map[string]any) error
	// ValidateUpdate is Validate for an edit. The console does not resend a
	// credential that has not changed, so a missing secret field is a value
	// left as it is, not an omission.
	ValidateUpdate(typeName string, values map[string]any) error
	// Normalize fills the derived fields and drops the ones the submitted
	// values put out of play, so that validation and storage see a coherent
	// set. Called before Validate, on both create and update.
	Normalize(typeName string, values map[string]any) map[string]any
}

type embeddedCatalog struct {
	types  []models.ContractDescriptor
	byName map[string]*models.ContractDescriptor
}

// NewEmbeddedContractCatalog loads the built-in contracts. It fails at startup
// rather than at request time: a malformed descriptor is a build mistake, not a
// runtime condition.
func NewEmbeddedContractCatalog() (ContractCatalog, error) {
	entries, err := contractsFS.ReadDir("contracts")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded contracts: %w", err)
	}

	c := &embeddedCatalog{byName: map[string]*models.ContractDescriptor{}}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := contractsFS.ReadFile("contracts/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("failed to read contract %s: %w", entry.Name(), err)
		}
		var ct models.ContractDescriptor
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&ct); err != nil {
			return nil, fmt.Errorf("failed to parse contract %s: %w", entry.Name(), err)
		}
		if err := validateTypeDescriptor(&ct); err != nil {
			return nil, fmt.Errorf("invalid contract %s: %w", entry.Name(), err)
		}
		c.types = append(c.types, ct)
	}

	sort.Slice(c.types, func(i, j int) bool { return c.types[i].Name < c.types[j].Name })

	for i := range c.types {
		ct := &c.types[i]
		if _, dup := c.byName[ct.Name]; dup {
			return nil, fmt.Errorf("duplicate contract %q", ct.Name)
		}
		c.byName[ct.Name] = ct
	}

	return c, nil
}

func validateTypeDescriptor(ct *models.ContractDescriptor) error {
	if ct.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(ct.Fields) == 0 {
		return fmt.Errorf("at least one field is required")
	}
	seen := map[string]bool{}
	for _, f := range ct.Fields {
		if f.Name == "" {
			return fmt.Errorf("a field has no name")
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field %q", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case models.FieldTypeString, models.FieldTypeNumber, models.FieldTypeBoolean:
		case models.FieldTypeEnum:
			if len(f.Options) == 0 {
				return fmt.Errorf("field %q is an enum with no options", f.Name)
			}
		default:
			return fmt.Errorf("field %q has unsupported type %q", f.Name, f.Type)
		}
	}
	return nil
}

func (c *embeddedCatalog) List() []models.ContractDescriptor {
	out := make([]models.ContractDescriptor, len(c.types))
	copy(out, c.types)
	return out
}

func (c *embeddedCatalog) Get(name string) (*models.ContractDescriptor, bool) {
	ct, ok := c.byName[name]
	return ct, ok
}

// Validate reports the first problem found in values, phrased for the end user.
// Unknown keys are rejected so that a renamed field surfaces as an error rather
// than being silently persisted and ignored.
func (c *embeddedCatalog) Validate(typeName string, values map[string]any) error {
	return c.validate(typeName, values, true)
}

func (c *embeddedCatalog) ValidateUpdate(typeName string, values map[string]any) error {
	return c.validate(typeName, values, false)
}

// Normalize returns a copy of values with the derived fields computed and the
// fields whose condition is not met removed. A MySQL connection carrying a
// PostgreSQL sslMode, or a driver contradicting its engine, cannot be opened by
// anything. Neither should be storable.
func (c *embeddedCatalog) Normalize(typeName string, values map[string]any) map[string]any {
	ct, ok := c.Get(typeName)
	if !ok {
		return values
	}

	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}

	for i := range ct.Fields {
		f := &ct.Fields[i]
		if !f.Applies(out) {
			delete(out, f.Name)
			continue
		}
		if f.Derived != nil {
			source, _ := out[f.Derived.From].(string)
			if derived, known := f.Derived.Map[source]; known {
				out[f.Name] = derived
			}
			continue
		}
		// An omitted field means the contract's default, and it belongs in the
		// values: the connectivity test and the consumers read those, not the
		// descriptor, so an absent sslMode would be probed as the strictest mode
		// rather than the declared one.
		if _, present := out[f.Name]; !present && f.Default != nil && !f.Secret {
			out[f.Name] = f.Default
		}
	}
	return out
}

func (c *embeddedCatalog) validate(typeName string, values map[string]any, requireSecrets bool) error {
	ct, ok := c.Get(typeName)
	if !ok {
		return fmt.Errorf("unknown contract %q", typeName)
	}

	for key := range values {
		if _, known := ct.Field(key); !known {
			return fmt.Errorf("unknown field %q for contract %q", key, typeName)
		}
	}

	for i := range ct.Fields {
		f := &ct.Fields[i]
		// A field the submitted values put out of play is neither required nor
		// checked: a MySQL connection is not missing a PostgreSQL TLS mode.
		if !f.Applies(values) {
			continue
		}
		value, present := values[f.Name]
		if !present || value == nil || value == "" {
			if f.Required && (requireSecrets || !f.Secret) {
				return fmt.Errorf("%s is required", fieldLabel(f))
			}
			continue
		}
		if err := validateFieldValue(f, value); err != nil {
			return err
		}
	}
	return nil
}

func validateFieldValue(f *models.ConnectionField, value any) error {
	switch f.Type {
	case models.FieldTypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", fieldLabel(f))
		}
	case models.FieldTypeBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", fieldLabel(f))
		}
	case models.FieldTypeNumber:
		// JSON decoding yields float64. An int may reach us from a Go caller.
		number, ok := toFloat(value)
		if !ok {
			return fmt.Errorf("%s must be a number", fieldLabel(f))
		}
		if f.Min != nil && number < *f.Min {
			return fmt.Errorf("%s must be at least %g", fieldLabel(f), *f.Min)
		}
		if f.Max != nil && number > *f.Max {
			return fmt.Errorf("%s must be at most %g", fieldLabel(f), *f.Max)
		}
	case models.FieldTypeEnum:
		str, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", fieldLabel(f))
		}
		for _, option := range f.Options {
			if option == str {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of: %s", fieldLabel(f), strings.Join(f.Options, ", "))
	}
	return nil
}

func toFloat(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

func fieldLabel(f *models.ConnectionField) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}
