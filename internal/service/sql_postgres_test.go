package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// sqlOnConnections builds the SQL service over the connection service's test
// environment: connections in a memory Git store, credentials in fake Secrets.
func sqlOnConnections(t *testing.T) (*DefaultSqlService, *DefaultConnectionService) {
	t.Helper()
	connections, env, _ := newServiceUnderTest(t)
	sql := NewDefaultSqlService(nil, SqlConnectionSource{Get: env.deployments.GetConnection, Secrets: env.secrets}, nil)
	return sql, connections
}

func TestSqlOnConnectionResolution(t *testing.T) {
	ctx := context.Background()
	sql, connections := sqlOnConnections(t)
	req := models.SqlQueryRequest{Query: "SELECT 1"}

	_, err := sql.ExecuteOnConnection(ctx, "demo", "nope", req)
	assert.True(t, apierrors.IsNotFound(err), "unknown connection: %v", err)

	mysql := postgresRequest()
	mysql.Name = "shop"
	mysql.Values["engine"] = "mysql"
	mysql.Values["driver"] = "com.mysql.cj.jdbc.Driver"
	delete(mysql.Values, "sslMode")
	_, err = connections.Create(ctx, "demo", mysql)
	require.NoError(t, err)
	_, err = sql.ExecuteOnConnection(ctx, "demo", "shop", req)
	assert.ErrorIs(t, err, ErrSqlUnsupported)
	assert.Contains(t, err.Error(), "(mysql)")

	_, err = connections.Create(ctx, "demo", models.ConnectionRequest{Name: "hms", Type: "hive", Values: map[string]any{"thriftUri": "thrift://hms:9083"}})
	require.NoError(t, err)
	_, err = sql.ExecuteOnConnection(ctx, "demo", "hms", req)
	assert.ErrorIs(t, err, ErrSqlUnsupported)
}

func TestSqlOnConnectionMissingSecret(t *testing.T) {
	ctx := context.Background()
	connections, env, _ := newServiceUnderTest(t)
	sql := NewDefaultSqlService(nil, SqlConnectionSource{Get: env.deployments.GetConnection, Secrets: env.secrets}, nil)
	_, err := connections.Create(ctx, "demo", postgresRequest())
	require.NoError(t, err)
	delete(env.secrets.secrets, "demo/warehouse-credentials")

	_, err = sql.ExecuteOnConnection(ctx, "demo", "warehouse", models.SqlQueryRequest{Query: "SELECT 1"})
	assert.ErrorIs(t, err, ErrSqlNoCredentials)
}

func TestSqlOnConnectionWithoutRepository(t *testing.T) {
	_, err := NewDefaultSqlService(nil, SqlConnectionSource{}, nil).ExecuteOnConnection(context.Background(), "demo", "w", models.SqlQueryRequest{Query: "SELECT 1"})
	assert.True(t, errors.Is(err, ErrConnectionsUnavailable))
}

func TestPostgresValue(t *testing.T) {
	assert.Nil(t, postgresValue(pgtype.TextOID, nil))
	assert.Equal(t, true, postgresValue(pgtype.BoolOID, []byte("t")))
	assert.Equal(t, false, postgresValue(pgtype.BoolOID, []byte("f")))
	assert.Equal(t, int64(42), postgresValue(pgtype.Int4OID, []byte("42")))
	assert.Equal(t, int64(-9007199254740991), postgresValue(pgtype.Int8OID, []byte("-9007199254740991")))
	assert.Equal(t, "9007199254740993", postgresValue(pgtype.Int8OID, []byte("9007199254740993")), "past 2^53 a browser would round it")
	assert.Equal(t, 1.5, postgresValue(pgtype.Float8OID, []byte("1.5")))
	assert.Equal(t, "NaN", postgresValue(pgtype.Float8OID, []byte("NaN")))
	assert.Equal(t, "Infinity", postgresValue(pgtype.Float4OID, []byte("Infinity")))
	assert.Equal(t, "12345678901234567890.01", postgresValue(pgtype.NumericOID, []byte("12345678901234567890.01")))
	assert.Equal(t, json.RawMessage(`{"a": [1]}`), postgresValue(pgtype.JSONBOID, []byte(`{"a": [1]}`)))
	assert.Equal(t, "2026-09-26 10:00:00+02", postgresValue(pgtype.TimestamptzOID, []byte("2026-09-26 10:00:00+02")))
	assert.Equal(t, `\x00ff`, postgresValue(pgtype.ByteaOID, []byte(`\x00ff`)))

	encoded, err := json.Marshal([]any{postgresValue(pgtype.JSONOID, []byte(`[1,"x"]`)), postgresValue(pgtype.Float8OID, []byte("NaN"))})
	require.NoError(t, err)
	assert.Equal(t, `[[1,"x"],"NaN"]`, string(encoded))
}

func TestLineColumn(t *testing.T) {
	query := "SELECT 1;\nSELEC 2"
	line, column := lineColumn(query, strings.Index(query, "SELEC 2")+1)
	assert.Equal(t, [2]int{2, 1}, [2]int{line, column})
	line, column = lineColumn("SELECT é FROM", 10)
	assert.Equal(t, [2]int{1, 10}, [2]int{line, column}, "positions count characters, not bytes")
}

// TestSqlOnPostgres runs against a live server, e.g.
//
//	docker run -d --rm -p 55432:5432 -e POSTGRES_PASSWORD=pw postgres:17
//	OKDP_TEST_POSTGRES_URL=postgres://postgres:pw@localhost:55432/postgres go test ./internal/service -run SqlOnPostgres
func TestSqlOnPostgres(t *testing.T) {
	raw := os.Getenv("OKDP_TEST_POSTGRES_URL")
	if raw == "" {
		t.Skip("OKDP_TEST_POSTGRES_URL is not set")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	password, _ := u.User.Password()

	ctx := context.Background()
	sql, connections := sqlOnConnections(t)
	req := postgresRequest()
	req.Values["host"] = u.Hostname()
	req.Values["port"] = float64(port)
	req.Values["dbName"] = strings.TrimPrefix(u.Path, "/")
	req.Values["username"] = u.User.Username()
	req.Values["password"] = password
	req.Values["sslMode"] = "prefer"
	_, err = connections.Create(ctx, "demo", req)
	require.NoError(t, err)

	run := func(query string, maxRows int) *models.SqlQueryResult {
		t.Helper()
		res, err := sql.ExecuteOnConnection(ctx, "demo", "warehouse", models.SqlQueryRequest{Query: query, MaxRows: maxRows})
		require.NoError(t, err)
		return res
	}

	res := run(`DROP TABLE IF EXISTS okdp_sql_test;
CREATE TABLE okdp_sql_test (id int8, name text, amount numeric, doc jsonb, at timestamptz);
INSERT INTO okdp_sql_test SELECT g, 'n' || g, g * 1.5, jsonb_build_object('g', g), '2026-09-26 10:00:00+00' FROM generate_series(1, 5) g`, 0)
	require.Nil(t, res.Error)
	assert.Equal(t, "INSERT 0 5", res.CommandTag, "writes are allowed")
	assert.Empty(t, res.Columns)

	res = run("SELECT id, name, amount, doc FROM okdp_sql_test ORDER BY id", 3)
	require.Nil(t, res.Error)
	assert.Equal(t, []models.SqlColumn{{Name: "id", Type: "int8"}, {Name: "name", Type: "text"}, {Name: "amount", Type: "numeric"}, {Name: "doc", Type: "jsonb"}}, res.Columns)
	assert.Equal(t, 3, res.RowCount)
	assert.True(t, res.Truncated)
	assert.Equal(t, "SELECT 5", res.CommandTag, "the rows past the cap are still read")
	assert.Equal(t, []any{int64(1), "n1", "1.5", json.RawMessage(`{"g": 1}`)}, res.Rows[0])

	res = run("SELECT count(*) AS n FROM okdp_sql_test; UPDATE okdp_sql_test SET name = 'x' WHERE id = 1", 0)
	require.Nil(t, res.Error)
	assert.Equal(t, [][]any{{int64(5)}}, res.Rows, "the last statement returning columns is the result")
	assert.Equal(t, "UPDATE 1", res.CommandTag)

	res = run("SELECT 1;\nSELEC 2", 0)
	require.NotNil(t, res.Error)
	assert.Equal(t, "42601", res.Error.ErrorName)
	assert.Equal(t, 2, res.Error.LineNumber)
	assert.Equal(t, 1, res.Error.ColumnNumber)

	res = run("INSERT INTO okdp_sql_test (id) VALUES (100); SELECT 1/0", 0)
	require.NotNil(t, res.Error)
	assert.Equal(t, "22012", res.Error.ErrorName)
	res = run("SELECT count(*) FROM okdp_sql_test WHERE id = 100", 0)
	assert.Equal(t, [][]any{{int64(0)}}, res.Rows, "a failing batch rolls back its implicit transaction")

	res = run("DROP TABLE okdp_sql_test", 0)
	require.Nil(t, res.Error)
	assert.Equal(t, "DROP TABLE", res.CommandTag)
}
