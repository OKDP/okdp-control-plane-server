package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// runPostgres executes a statement batch on a PostgreSQL connection with the
// credentials of the connection, not the caller's: every project member using
// the editor acts as that database user, writes included.
//
// The batch goes through the simple query protocol, so several statements
// separated by ';' run in one call, in one implicit transaction unless the
// batch opens its own. The result is the last statement that returned
// columns (else the last statement), with its command tag.
//
// Rows past maxRows are read and dropped, never cancelled: a cancel aborts the
// implicit transaction, which would roll back the writes of an earlier
// statement of the batch without saying so. sqlQueryTimeout bounds the cost.
func runPostgres(ctx context.Context, values connectionValues, query string, maxRows int) (*models.SqlQueryResult, error) {
	config, err := postgresConfig(values)
	if err != nil {
		return nil, err
	}
	config.ConnectTimeout = testTimeout
	config.RuntimeParams["application_name"] = "okdp-sql-editor"

	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("engine unreachable: %w", classifyPostgresError(ctx, err))
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	result := &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}
	typeMap := conn.TypeMap()
	haveColumns := false

	mrr := conn.PgConn().Exec(ctx, query)
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		fields := rr.FieldDescriptions()
		columns := make([]models.SqlColumn, len(fields))
		for i, f := range fields {
			columns[i] = models.SqlColumn{Name: f.Name, Type: postgresTypeName(typeMap, f.DataTypeOID)}
		}
		rows := [][]any{}
		truncated := false
		for rr.NextRow() {
			if len(rows) >= maxRows {
				truncated = true
				continue
			}
			raw := rr.Values()
			row := make([]any, len(raw))
			for i, v := range raw {
				row[i] = postgresValue(fields[i].DataTypeOID, v)
			}
			rows = append(rows, row)
		}
		tag, err := rr.Close()
		if err != nil {
			_ = mrr.Close()
			return postgresFailure(ctx, query, err)
		}
		result.CommandTag = tag.String()
		if len(fields) > 0 || !haveColumns {
			haveColumns = haveColumns || len(fields) > 0
			result.Columns, result.Rows, result.Truncated = columns, rows, truncated
		}
	}
	if err := mrr.Close(); err != nil {
		return postgresFailure(ctx, query, err)
	}
	return result, nil
}

// postgresFailure turns an execution error into the result the editor shows
// (SQL errors, timeout) or a proxy error (the connection broke).
func postgresFailure(ctx context.Context, query string, err error) (*models.SqlQueryResult, error) {
	empty := &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		empty.Error = &models.SqlQueryError{Message: pgErr.Message, ErrorName: pgErr.Code}
		if pgErr.Detail != "" {
			empty.Error.Message += ": " + pgErr.Detail
		}
		if pgErr.Position > 0 {
			empty.Error.LineNumber, empty.Error.ColumnNumber = lineColumn(query, int(pgErr.Position))
		}
		return empty, nil
	}
	if ctx.Err() != nil {
		empty.Error = &models.SqlQueryError{
			Message:   fmt.Sprintf("The query did not finish within %s and was cancelled.", sqlQueryTimeout),
			ErrorName: "TIMEOUT",
		}
		return empty, nil
	}
	return nil, fmt.Errorf("engine connection lost: %w", err)
}

// lineColumn converts PostgreSQL's 1-based character position into a 1-based
// line and column.
func lineColumn(query string, position int) (int, int) {
	line, column := 1, 1
	for i, r := range []rune(query) {
		if i >= position-1 {
			break
		}
		if r == '\n' {
			line, column = line+1, 1
		} else {
			column++
		}
	}
	return line, column
}

func postgresTypeName(m *pgtype.Map, oid uint32) string {
	if t, ok := m.TypeForOID(oid); ok {
		return t.Name
	}
	return "oid:" + strconv.FormatUint(uint64(oid), 10)
}

// maxSafeInteger is the largest integer a browser's float64 holds exactly.
const maxSafeInteger = 1<<53 - 1

// postgresValue converts one text-format value to JSON. Numbers stay numbers
// only when a browser reads them back exactly; numeric, large int8, NaN and
// infinities come back as their text. json/jsonb are passed through as JSON.
// Every other type (dates, intervals, arrays, bytea as \x…) is its PostgreSQL
// text form.
func postgresValue(oid uint32, raw []byte) any {
	if raw == nil {
		return nil
	}
	text := string(raw)
	switch oid {
	case pgtype.BoolOID:
		return text == "t"
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID, pgtype.OIDOID:
		if n, err := strconv.ParseInt(text, 10, 64); err == nil && n <= maxSafeInteger && n >= -maxSafeInteger {
			return n
		}
	case pgtype.Float4OID, pgtype.Float8OID:
		if f, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	case pgtype.JSONOID, pgtype.JSONBOID:
		if json.Valid(raw) {
			return json.RawMessage(text)
		}
	}
	return text
}
