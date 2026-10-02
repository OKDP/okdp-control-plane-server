package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	okdpcontracts "github.com/okdp/okdp-lib-chart/contracts"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// contractsFS holds the contract schemas: the canonical JSON Schemas of
// okdp-lib (github.com/okdp/okdp-lib-chart/contracts), which the charts and
// scripts/check.sh validate against too. The same document drives the form
// rendered by the console (converted to a ContractDescriptor), the
// server-side validation of submitted values, and the address shown for a
// connection of that contract.
var contractsFS = okdpcontracts.FS

// ContractCatalog exposes the known contracts.
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
	// ValidatePublic checks the non-secret values, as they are written to the
	// connection file, against the contract's JSON Schema itself.
	ValidatePublic(typeName string, values map[string]any) error
}

type embeddedCatalog struct {
	types   []models.ContractDescriptor
	byName  map[string]*models.ContractDescriptor
	schemas map[string]*jsonschema.Schema
}

// NewEmbeddedContractCatalog loads the built-in contracts. It fails at startup
// rather than at request time: a malformed schema is a build mistake, not a
// runtime condition.
func NewEmbeddedContractCatalog() (ContractCatalog, error) {
	entries, err := contractsFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("failed to read embedded contracts: %w", err)
	}

	c := &embeddedCatalog{byName: map[string]*models.ContractDescriptor{}, schemas: map[string]*jsonschema.Schema{}}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".schema.json") {
			continue
		}
		raw, err := contractsFS.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("failed to read contract %s: %w", entry.Name(), err)
		}
		ct, err := descriptorFromSchema(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid contract %s: %w", entry.Name(), err)
		}
		if err := validateTypeDescriptor(ct); err != nil {
			return nil, fmt.Errorf("invalid contract %s: %w", entry.Name(), err)
		}
		compiled, err := compileSchema(entry.Name(), raw)
		if err != nil {
			return nil, fmt.Errorf("invalid contract %s: %w", entry.Name(), err)
		}
		c.types = append(c.types, *ct)
		c.schemas[ct.Name] = compiled
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

func compileSchema(name string, raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(name, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(name)
}

// contractSchema is the part of a contract schema the console form needs.
type contractSchema struct {
	Title        string                    `json:"title"`
	Description  string                    `json:"description"`
	Contract     string                    `json:"x-okdp-contract"`
	Icon         string                    `json:"x-okdp-icon"`
	Category     string                    `json:"x-okdp-category"`
	External     bool                      `json:"x-okdp-external"`
	EndpointFrom []string                  `json:"x-okdp-endpoint-from"`
	Internal     map[string]any            `json:"x-okdp-internal"`
	Required     []string                  `json:"required"`
	Properties   map[string]propertySchema `json:"properties"`
}

type propertySchema struct {
	Type           string                  `json:"type"`
	Title          string                  `json:"title"`
	Description    string                  `json:"description"`
	Enum           []any                   `json:"enum"`
	Default        any                     `json:"default"`
	Minimum        *float64                `json:"minimum"`
	Maximum        *float64                `json:"maximum"`
	Secret         bool                    `json:"x-okdp-secret"`
	SecretRequired bool                    `json:"x-okdp-required"`
	Derived        *models.FieldDerivation `json:"x-okdp-derived"`
	Condition      *models.FieldCondition  `json:"x-ui-condition"`
	Placeholder    string                  `json:"x-ui-placeholder"`
	Widget         string                  `json:"x-ui-widget"`
}

// propertyOrder returns the keys of the "properties" object in document order:
// Go maps lose it, and it is the order of the form.
func propertyOrder(raw []byte) ([]string, error) {
	var doc struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(doc.Properties))
	if _, err := dec.Token(); err != nil { // {
		return nil, err
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// descriptorFromSchema converts a contract schema into the descriptor the
// console renders its form from (GET /api/contracts).
func descriptorFromSchema(raw []byte) (*models.ContractDescriptor, error) {
	var cs contractSchema
	if err := json.Unmarshal(raw, &cs); err != nil {
		return nil, err
	}
	order, err := propertyOrder(raw)
	if err != nil {
		return nil, err
	}
	required := map[string]bool{}
	for _, r := range cs.Required {
		required[r] = true
	}
	ct := &models.ContractDescriptor{
		Name:         cs.Contract,
		DisplayName:  cs.Title,
		Description:  cs.Description,
		Icon:         cs.Icon,
		Category:     cs.Category,
		External:     cs.External,
		Internal:     len(cs.Internal) > 0,
		EndpointFrom: cs.EndpointFrom,
	}
	for _, name := range order {
		p := cs.Properties[name]
		field := models.ConnectionField{
			Name:        name,
			Label:       p.Title,
			Required:    required[name] || (p.Secret && p.SecretRequired),
			Secret:      p.Secret,
			Masked:      p.Widget == "password",
			Default:     p.Default,
			Placeholder: p.Placeholder,
			Help:        p.Description,
			Min:         p.Minimum,
			Max:         p.Maximum,
			ShowWhen:    p.Condition,
			Derived:     p.Derived,
		}
		switch {
		case len(p.Enum) > 0:
			field.Type = models.FieldTypeEnum
			for _, option := range p.Enum {
				field.Options = append(field.Options, fmt.Sprint(option))
			}
		case p.Type == "integer" || p.Type == "number":
			field.Type = models.FieldTypeNumber
		case p.Type == "boolean":
			field.Type = models.FieldTypeBoolean
		case p.Type == "string":
			field.Type = models.FieldTypeString
		default:
			// Lists (trino catalogs) are published by charts, never typed into
			// a form: they are validated by the schema, not offered.
			continue
		}
		ct.Fields = append(ct.Fields, field)
	}
	return ct, nil
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

func (c *embeddedCatalog) ValidatePublic(typeName string, values map[string]any) error {
	sch, ok := c.schemas[typeName]
	if !ok {
		return fmt.Errorf("unknown contract %q", typeName)
	}
	// Through JSON, so numbers and nested values have the validator's types.
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if err := sch.Validate(doc); err != nil {
		return fmt.Errorf("the values do not satisfy the %s contract: %v", typeName, err)
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
