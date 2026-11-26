package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/engine/fake"
)

// schemaCatalog serves one fixed parameter schema.
type schemaCatalog struct {
	engine.Catalog
	schema map[string]any
}

func (c schemaCatalog) ParameterSchema(context.Context, string, string) (map[string]any, error) {
	return c.schema, nil
}

func coreWithSchema(schema map[string]any) *DefaultServiceService {
	eng := fake.New().Engine()
	eng.Catalog = schemaCatalog{Catalog: eng.Catalog, schema: schema}
	return &DefaultServiceService{engine: eng}
}

func TestASchemaErrorNamesNoLocalPath(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	core := coreWithSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{"replicas": map[string]any{"type": "integer"}},
	})

	err = core.validateParameters(context.Background(), "jupyterhub", "1.0.0", map[string]any{"replicas": "two"})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), cwd)
	assert.NotContains(t, err.Error(), "file://")
	assert.Contains(t, err.Error(), "/replicas")
}

// A $ref to a local file is not read: the schema fails closed at compile time
// (not via the file's schema rules), and nothing about cwd reaches the logs.
func TestASchemaCannotReadLocalFiles(t *testing.T) {
	refused := filepath.Join(t.TempDir(), "refuse-all.json")
	require.NoError(t, os.WriteFile(refused, []byte(`{"not": {}}`), 0o600))
	core := coreWithSchema(map[string]any{"$ref": "file://" + refused})
	var logs bytes.Buffer
	logrus.SetOutput(&logs)
	t.Cleanup(func() { logrus.SetOutput(os.Stderr) })

	err := core.validateParameters(context.Background(), "jupyterhub", "1.0.0", map[string]any{})

	require.Error(t, err, "uncompilable schema must fail closed")
	var violation *schemaViolation
	assert.False(t, errors.As(err, &violation), "local file must not have been read and evaluated as a schema")
	assert.Contains(t, err.Error(), "failed to compile schema")
	cwd, err := os.Getwd()
	require.NoError(t, err)
	assert.NotContains(t, logs.String(), cwd)
}

func TestInvalidSchemaFailsClosed(t *testing.T) {
	core := coreWithSchema(map[string]any{"type": 123})
	err := core.validateParameters(context.Background(), "jupyterhub", "1.0.0", map[string]any{"anything": "value"})
	require.Error(t, err, "malformed schema must not silently bypass validation")
	assert.Contains(t, err.Error(), "invalid parameter schema")
}
