package models

// SqlQueryRequest is a SQL statement to execute on a deployed SQL engine.
type SqlQueryRequest struct {
	Query string `json:"query" binding:"required"`
	// MaxRows caps the number of result rows returned (default 1000).
	MaxRows int `json:"maxRows,omitempty"`
}

// SqlColumn describes one column of a SQL result set.
type SqlColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SqlQueryError is a query failure reported by the engine (syntax error,
// missing table…, or a PostgreSQL statement past the proxy timeout) — the
// proxy call itself succeeded. ErrorName is Trino's error name or
// PostgreSQL's SQLSTATE.
type SqlQueryError struct {
	Message      string `json:"message"`
	ErrorName    string `json:"errorName,omitempty"`
	LineNumber   int    `json:"lineNumber,omitempty"`
	ColumnNumber int    `json:"columnNumber,omitempty"`
}

// SqlQueryResult is the outcome of a SQL execution: either a result set or
// an engine-reported error.
type SqlQueryResult struct {
	QueryID   string      `json:"queryId,omitempty"`
	Columns   []SqlColumn `json:"columns"`
	Rows      [][]any     `json:"rows"`
	RowCount  int         `json:"rowCount"`
	Truncated bool        `json:"truncated"`
	ElapsedMs int64       `json:"elapsedMs"`
	// CommandTag is the PostgreSQL command tag of the last statement
	// (e.g. "INSERT 0 5"). Empty for Trino.
	CommandTag string         `json:"commandTag,omitempty"`
	Error      *SqlQueryError `json:"error,omitempty"`
}
