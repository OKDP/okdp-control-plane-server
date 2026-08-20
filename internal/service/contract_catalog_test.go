package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCatalog(t *testing.T) ContractCatalog {
	t.Helper()
	catalog, err := NewEmbeddedContractCatalog()
	require.NoError(t, err, "the embedded contracts must all parse")
	return catalog
}

func TestEmbeddedCatalogLoadsBuiltInTypes(t *testing.T) {
	catalog := newTestCatalog(t)

	names := map[string]bool{}
	for _, ct := range catalog.List() {
		names[ct.Name] = true
	}

	// One descriptor per ClusterContract the platform publishes.
	assert.True(t, names["database-server"])
	assert.True(t, names["s3"])
	assert.True(t, names["trino"])
	assert.True(t, names["hive"])
	assert.True(t, names["iceberg-catalog"])
}

func TestEveryTypeWithCredentialsMarksThemSecret(t *testing.T) {
	catalog := newTestCatalog(t)

	// A credential that is not marked secret would be written to the Connection
	// spec in clear, so assert the marking rather than trusting the JSON.
	for _, name := range []string{"database-server"} {
		ct, ok := catalog.Get(name)
		require.True(t, ok, name)
		assert.Contains(t, ct.SecretFields(), "password", name)
	}

	s3, ok := catalog.Get("s3")
	require.True(t, ok)
	assert.Contains(t, s3.SecretFields(), "secretKey")
	// Both halves of an S3 identity go to the Secret. Not because a key ID is a
	// secret on its own, it is not, but because the s3 Contract declares a Secret
	// "with keys: accessKey, secretKey" and consuming packages read both from it.
	assert.Contains(t, s3.SecretFields(), "accessKey", "the s3 contract expects the key ID in the Secret")
}

// A database outside the cluster is often reachable only in clear text
// public datasets, older corporate instances. Without "prefer" the only
// working choice is "disable", which never even attempts TLS, so a server
// that does support it would be talked to unencrypted.
func TestPostgreSQLOffersThePreferSSLMode(t *testing.T) {
	catalog := newTestCatalog(t)

	database, ok := catalog.Get("database-server")
	if !ok {
		t.Fatal("database-server type missing")
	}
	sslMode, ok := database.Field("sslMode")
	if !ok {
		t.Fatal("sslMode field missing")
	}

	var found bool
	for _, option := range sslMode.Options {
		if option == "prefer" {
			found = true
		}
	}
	if !found {
		t.Errorf("sslMode options = %v, want one of them to be \"prefer\"", sslMode.Options)
	}
	if sslMode.Default != "prefer" {
		t.Errorf("sslMode default = %v, want prefer", sslMode.Default)
	}
}

func TestValidateAcceptsAWellFormedPayload(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.Validate("database-server", map[string]any{
		"engine":   "postgresql",
		"driver":   "org.postgresql.Driver",
		"host":     "db.example.com",
		"port":     float64(5432),
		"dbName":   "analytics",
		"username": "reader",
		"password": "s3cret",
		"sslMode":  "require",
	})

	assert.NoError(t, err)
}

func TestValidateRejectsBadPayloads(t *testing.T) {
	catalog := newTestCatalog(t)

	base := func() map[string]any {
		return map[string]any{
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "db.example.com",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "require",
		}
	}

	tests := []struct {
		name    string
		mutate  func(map[string]any)
		message string
	}{
		{
			name:    "missing required field",
			mutate:  func(v map[string]any) { delete(v, "host") },
			message: "Host is required",
		},
		{
			name:    "empty required field",
			mutate:  func(v map[string]any) { v["host"] = "" },
			message: "Host is required",
		},
		{
			name:    "port out of range",
			mutate:  func(v map[string]any) { v["port"] = float64(70000) },
			message: "Port must be at most 65535",
		},
		{
			name:    "port not a number",
			mutate:  func(v map[string]any) { v["port"] = "5432" },
			message: "Port must be a number",
		},
		{
			name:    "value outside the enum",
			mutate:  func(v map[string]any) { v["sslMode"] = "maybe" },
			message: "TLS mode must be one of",
		},
		{
			name:    "unknown field",
			mutate:  func(v map[string]any) { v["hostname"] = "db.example.com" },
			message: `unknown field "hostname"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := base()
			tt.mutate(values)

			err := catalog.Validate("database-server", values)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.Validate("oracle", map[string]any{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown contract "oracle"`)
}

func TestValidateAllowsOmittingOptionalFields(t *testing.T) {
	catalog := newTestCatalog(t)

	// region and pathStyle carry defaults and must not be demanded.
	err := catalog.Validate("s3", map[string]any{
		"apiUrl":    "https://s3.example.com",
		"bucket":    "warehouse",
		"accessKey": "AKIA",
		"secretKey": "secret",
	})

	assert.NoError(t, err)
}

// The driver class and the engine cannot be allowed to disagree: a connection
// declaring MySQL with the PostgreSQL driver opens for nobody, and the user has
// no way of telling which of the two fields was the wrong one.
func TestNormalizeDerivesTheDriverFromTheEngine(t *testing.T) {
	catalog := newTestCatalog(t)

	values := catalog.Normalize("database-server", map[string]any{
		"engine": "mysql",
		"driver": "org.postgresql.Driver",
		"host":   "db.example.com",
	})

	assert.Equal(t, "com.mysql.cj.jdbc.Driver", values["driver"])
}

// PostgreSQL and MySQL name their TLS modes differently, and the form offers
// whichever belongs to the chosen engine. A value for the other one is a
// leftover from switching engines, not a setting: it must not be stored, and
// must not be demanded either.
func TestNormalizeDropsTheFieldsTheEngineRulesOut(t *testing.T) {
	catalog := newTestCatalog(t)

	values := catalog.Normalize("database-server", map[string]any{
		"engine":  "mysql",
		"host":    "db.example.com",
		"sslMode": "prefer",
		"tls":     "preferred",
	})

	_, hasSSLMode := values["sslMode"]
	assert.False(t, hasSSLMode, "the PostgreSQL wording has no place on a MySQL connection")
	assert.Equal(t, "preferred", values["tls"])
}

func TestValidateIgnoresAFieldTheEngineRulesOut(t *testing.T) {
	catalog := newTestCatalog(t)

	err := catalog.Validate("database-server", map[string]any{
		"engine":   "mysql",
		"driver":   "com.mysql.cj.jdbc.Driver",
		"host":     "db.example.com",
		"port":     float64(3306),
		"dbName":   "analytics",
		"username": "reader",
		"password": "s3cret",
		"tls":      "preferred",
	})

	assert.NoError(t, err)
}

// The address of a connection depends on its contract: database-server carries
// a host and a port, trino a URI, hive a thrift URI, s3 a URL. Reading host and
// port only, as the code did, showed "not available yet" on a Ready trino, hive
// or s3 connection whose address sat in its own values.
func TestEndpointComesFromTheContract(t *testing.T) {
	catalog := newTestCatalog(t)

	// A URI contract: the in-cluster address wins over the public one.
	assert.Equal(t,
		"thrift://demo-hms.demo.svc.cluster.local:9083",
		endpointFrom(map[string]any{"thriftUri": "thrift://demo-hms.demo.svc.cluster.local:9083"}, catalog, "hive"))
	assert.Equal(t,
		"jdbc:trino://trino.demo.svc.cluster.local:8080",
		endpointFrom(map[string]any{
			"url":         "https://trino.example.com",
			"uri":         "jdbc:trino://trino.example.com:443",
			"internalUri": "jdbc:trino://trino.demo.svc.cluster.local:8080",
		}, catalog, "trino"))

	// The host+port convention still works with no declaration.
	assert.Equal(t, "db.example.com:5432",
		endpointFrom(map[string]any{"host": "db.example.com", "port": float64(5432)}, catalog, "database-server"))

	// Nothing to show is an empty string, never a guess.
	assert.Equal(t, "", endpointFrom(map[string]any{}, catalog, "hive"))
	assert.Equal(t, "", endpointFrom(map[string]any{"realm": "main"}, catalog, "unknown-contract"))
}

// Every contract the platform publishes must be declarable, or the wizard
// leaves the user stuck with "No compatible connection available" and no way to
// create one. These are the five ClusterContracts of platform-packages: the
// OIDC provider is deliberately not among them, it is platform configuration
// read from the Context, not something a project connects to.
func TestEveryPlatformContractIsInTheCatalog(t *testing.T) {
	catalog := newTestCatalog(t)

	for _, name := range []string{"database-server", "s3", "trino", "hive", "iceberg-catalog"} {
		ct, ok := catalog.Get(name)
		require.True(t, ok, "%s is a ClusterContract of the platform and must be declarable", name)
		assert.True(t, ct.External, "%s must offer a creation form", name)
	}
}

// The consumers and the connectivity test read the values, not the descriptor,
// so an omitted optional field must carry its declared default.
func TestNormalizeWritesTheDeclaredDefaults(t *testing.T) {
	catalog, err := NewEmbeddedContractCatalog()
	if err != nil {
		t.Fatalf("loading the catalog: %v", err)
	}

	values := catalog.Normalize("database-server", map[string]any{
		"engine": "postgresql",
		"host":   "db.example.com",
		"port":   float64(5432),
		"dbName": "analytics",
	})

	if values["sslMode"] != "prefer" {
		t.Fatalf("sslMode: got %v, want the declared default \"prefer\"", values["sslMode"])
	}
}
