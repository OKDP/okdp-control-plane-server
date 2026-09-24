package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
)

const (
	sqlDefaultMaxRows = 1000
	sqlHardMaxRows    = 10000
	sqlQueryTimeout   = 2 * time.Minute
	// sqlMaxPageBytes caps one engine response page.
	sqlMaxPageBytes = 32 << 20
)

var (
	// ErrSqlUnsupported: the instance's service has no SQL engine driver.
	ErrSqlUnsupported = errors.New("does not support SQL execution")
	// ErrSqlNoURL: the instance descriptor publishes no URL (yet).
	ErrSqlNoURL = errors.New("does not expose a URL")
	// ErrSqlNoCredentials: the connection's credentials Secret is missing.
	ErrSqlNoCredentials = errors.New("does not exist")
)

// SqlService executes SQL statements on deployed SQL engines.
type SqlService interface {
	// ExecuteQuery runs a statement on the given service instance. The
	// caller's Authorization header is forwarded: the engine enforces
	// platform SSO itself.
	ExecuteQuery(ctx context.Context, project, serviceName, authorization string, req models.SqlQueryRequest) (*models.SqlQueryResult, error)
	// ExecuteOnConnection runs a statement batch on an external connection of
	// the project (database-server, engine postgresql), logged in with the
	// connection's credentials Secret.
	ExecuteOnConnection(ctx context.Context, project, connection string, req models.SqlQueryRequest) (*models.SqlQueryResult, error)
}

// SqlConnectionSource reads what the SQL service needs of an external
// connection: its declaration in Git and its credentials Secret.
type SqlConnectionSource struct {
	Get     func(ctx context.Context, project, name string) (*gitops.Connection, error)
	Secrets repository.ConnectionSecretRepository
}

// sqlEngine is the driver of one HTTP wire protocol of a deployed instance.
// Engines are keyed by the catalog service name in DefaultSqlService.engines;
// a service without an entry answers ErrSqlUnsupported. PostgreSQL is not an
// instance but an external connection: see ExecuteOnConnection.
//
// Only Trino has a driver. DuckDB (community package duckdb, DuckDB 2.0
// served over Quack) cannot get one here: Quack encodes messages with DuckDB's
// internal serialization (application/duckdb), so its only client is DuckDB
// itself, which this CGO-free server does not embed. Its authentication is
// also a shared token Secret, not the caller's bearer token.
type sqlEngine interface {
	execute(ctx context.Context, client *http.Client, baseURL, authorization, query string, maxRows int) (*models.SqlQueryResult, error)
}

type DefaultSqlService struct {
	getService func(ctx context.Context, project, name string) (*models.ServiceInstance, error)
	// insecureTLS reports whether engine certificates are taken on trust.
	insecureTLS func(ctx context.Context) bool
	engines     map[string]sqlEngine
	connections SqlConnectionSource
	client      *http.Client
	insecure    *http.Client
}

// NewDefaultSqlService resolves instances through getService (the descriptor
// URL is the engine endpoint). insecureTLS may be nil (certificates checked).
func NewDefaultSqlService(getService func(ctx context.Context, project, name string) (*models.ServiceInstance, error), connections SqlConnectionSource, insecureTLS func(ctx context.Context) bool) *DefaultSqlService {
	insecureTransport := http.DefaultTransport.(*http.Transport).Clone()
	insecureTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // #nosec G402 -- opt-in, sandboxes
	return &DefaultSqlService{
		getService:  getService,
		insecureTLS: insecureTLS,
		engines:     map[string]sqlEngine{"trino": trinoEngine{}},
		connections: connections,
		client:      &http.Client{Timeout: 30 * time.Second},
		insecure:    &http.Client{Timeout: 30 * time.Second, Transport: insecureTransport},
	}
}

func (s *DefaultSqlService) ExecuteQuery(ctx context.Context, project, serviceName, authorization string, req models.SqlQueryRequest) (*models.SqlQueryResult, error) {
	instance, err := s.getService(ctx, project, serviceName)
	if err != nil {
		return nil, err
	}
	engine, ok := s.engines[instance.Service]
	if !ok {
		return nil, fmt.Errorf("service '%s' (%s) %w", serviceName, instance.Service, ErrSqlUnsupported)
	}
	if instance.URL == "" {
		return nil, fmt.Errorf("service '%s' %w", serviceName, ErrSqlNoURL)
	}

	client := s.client
	if s.insecureTLS != nil && s.insecureTLS(ctx) {
		client = s.insecure
	}

	return timed(ctx, req, func(ctx context.Context, maxRows int) (*models.SqlQueryResult, error) {
		return engine.execute(ctx, client, instance.URL, authorization, req.Query, maxRows)
	})
}

func (s *DefaultSqlService) ExecuteOnConnection(ctx context.Context, project, name string, req models.SqlQueryRequest) (*models.SqlQueryResult, error) {
	if s.connections.Get == nil {
		return nil, ErrConnectionsUnavailable
	}
	connection, err := s.connections.Get(ctx, project, name)
	if err != nil {
		if errors.Is(err, gitops.ErrNotFound) {
			return nil, apierrors.NewNotFound(connectionsResource, name)
		}
		return nil, err
	}
	values := connectionValues{}
	for k, v := range connection.Values {
		values[k] = v
	}
	if connection.Contract != "database-server" || values.String("engine") != "postgresql" {
		engine := values.String("engine")
		if engine == "" {
			engine = connection.Contract
		}
		return nil, fmt.Errorf("connection '%s' (%s) %w", name, engine, ErrSqlUnsupported)
	}
	if connection.SecretRef != "" {
		data, found, err := s.connections.Secrets.ReadSecret(ctx, project, connection.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("reading the credentials of connection '%s': %w", name, err)
		}
		if !found {
			return nil, fmt.Errorf("connection '%s': credentials secret '%s' %w", name, connection.SecretRef, ErrSqlNoCredentials)
		}
		for k, v := range data {
			values[k] = string(v)
		}
	}
	return timed(ctx, req, func(ctx context.Context, maxRows int) (*models.SqlQueryResult, error) {
		return runPostgres(ctx, values, req.Query, maxRows)
	})
}

// timed clamps the row cap, bounds the execution by sqlQueryTimeout and
// fills the counters every engine shares.
func timed(ctx context.Context, req models.SqlQueryRequest, run func(ctx context.Context, maxRows int) (*models.SqlQueryResult, error)) (*models.SqlQueryResult, error) {
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = sqlDefaultMaxRows
	}
	if maxRows > sqlHardMaxRows {
		maxRows = sqlHardMaxRows
	}
	ctx, cancel := context.WithTimeout(ctx, sqlQueryTimeout)
	defer cancel()
	start := time.Now()
	result, err := run(ctx, maxRows)
	if err != nil {
		return nil, err
	}
	result.RowCount = len(result.Rows)
	result.ElapsedMs = time.Since(start).Milliseconds()
	return result, nil
}

// trinoEngine speaks the Trino client protocol: POST /v1/statement, then GET
// each nextUri until there is none.
type trinoEngine struct{}

// trinoResponse is the subset of the Trino client protocol the proxy reads.
type trinoResponse struct {
	ID      string `json:"id"`
	NextURI string `json:"nextUri"`
	Columns []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"columns"`
	Data  [][]any `json:"data"`
	Error *struct {
		Message       string `json:"message"`
		ErrorName     string `json:"errorName"`
		ErrorLocation *struct {
			LineNumber   int `json:"lineNumber"`
			ColumnNumber int `json:"columnNumber"`
		} `json:"errorLocation"`
	} `json:"error"`
}

func (e trinoEngine) execute(ctx context.Context, client *http.Client, baseURL, authorization, query string, maxRows int) (*models.SqlQueryResult, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("invalid engine URL %q", baseURL)
	}
	result := &models.SqlQueryResult{Columns: []models.SqlColumn{}, Rows: [][]any{}}

	page, err := e.call(ctx, client, http.MethodPost, base.String()+"/v1/statement", authorization, query)
	if err != nil {
		return nil, err
	}
	for {
		if page.ID != "" {
			result.QueryID = page.ID
		}
		if len(page.Columns) > 0 && len(result.Columns) == 0 {
			for _, col := range page.Columns {
				result.Columns = append(result.Columns, models.SqlColumn{Name: col.Name, Type: col.Type})
			}
		}
		for _, row := range page.Data {
			if len(result.Rows) >= maxRows {
				result.Truncated = true
				break
			}
			result.Rows = append(result.Rows, row)
		}
		if page.Error != nil {
			result.Error = &models.SqlQueryError{
				Message:   page.Error.Message,
				ErrorName: page.Error.ErrorName,
			}
			if page.Error.ErrorLocation != nil {
				result.Error.LineNumber = page.Error.ErrorLocation.LineNumber
				result.Error.ColumnNumber = page.Error.ErrorLocation.ColumnNumber
			}
			break
		}
		if page.NextURI == "" {
			break
		}
		next, err := rebase(base, page.NextURI)
		if err != nil {
			return nil, err
		}
		if result.Truncated {
			// Enough rows: tell Trino to stop computing the rest.
			e.cancel(client, next, authorization)
			break
		}
		if page, err = e.call(ctx, client, http.MethodGet, next, authorization, ""); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// rebase keeps the path and query of a nextUri but sends it to the engine
// URL the instance publishes. Trino builds nextUri from its own view of the
// request (an in-cluster http host unless it honors forwarded headers), and
// the caller's token must never follow a URI to another host.
func rebase(base *url.URL, nextURI string) (string, error) {
	next, err := url.Parse(nextURI)
	if err != nil {
		return "", fmt.Errorf("unexpected engine nextUri %q: %w", nextURI, err)
	}
	u := *base
	u.Path = strings.TrimRight(base.Path, "/") + next.Path
	u.RawPath = ""
	u.RawQuery = next.RawQuery
	u.Fragment = ""
	return u.String(), nil
}

func (trinoEngine) call(ctx context.Context, client *http.Client, method, target, authorization, body string) (*trinoResponse, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		httpReq.Header.Set("Authorization", authorization)
	}
	if body != "" {
		httpReq.Header.Set("Content-Type", "text/plain")
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("engine unreachable: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, sqlMaxPageBytes))
	if err != nil {
		return nil, fmt.Errorf("reading engine response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("engine rejected the platform credentials (%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var page trinoResponse
	if err := json.Unmarshal(payload, &page); err != nil {
		return nil, fmt.Errorf("unexpected engine response: %w", err)
	}
	return &page, nil
}

// cancel is best-effort: a DELETE on the nextUri stops the query.
func (trinoEngine) cancel(client *http.Client, nextURI, authorization string) {
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, nextURI, nil)
	if err != nil {
		return
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	} else {
		logrus.WithError(err).Debug("Failed to cancel SQL query")
	}
}
