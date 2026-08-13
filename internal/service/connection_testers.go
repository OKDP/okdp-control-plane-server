package service

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// testTimeout bounds a connectivity test. The console waits for the answer, so
// a hung TCP connect must fail fast rather than tie up the request.
const testTimeout = 10 * time.Second

// testFailure is a connectivity failure carrying a classification, so that the
// console can tell "wrong password" from "host unreachable" instead of showing
// a raw driver error.
type testFailure struct {
	reason  string
	message string
}

func (e *testFailure) Error() string { return e.message }

func failure(reason, format string, args ...any) *testFailure {
	return &testFailure{reason: reason, message: fmt.Sprintf(format, args...)}
}

// connectionTester probes the live endpoints of a connection. It returns one
// check per path it tried: a contract may publish several, and a store
// reachable from outside the cluster and unreachable from inside it is a real
// case worth naming. A check succeeds only when the endpoint answered *and*
// accepted the credentials, since reachability alone would pass a wrong
// password.
type connectionTester func(ctx context.Context, values connectionValues) []models.ConnectionTestCheck

// check turns the error of a single probe into a reportable verdict.
func check(label, target string, decisive bool, err error) models.ConnectionTestCheck {
	result := models.ConnectionTestCheck{Label: label, Target: target, Decisive: decisive}
	if err == nil {
		result.Success = true
		result.Message = "Connection successful."
		return result
	}
	var failed *testFailure
	if errors.As(err, &failed) {
		result.Message = failed.message
		result.Reason = failed.reason
	} else {
		result.Message = err.Error()
		result.Reason = models.TestReasonUnknown
	}
	return result
}

// connectionTesters holds the probe of each type that has one. A type absent
// from the map is reported as not testable rather than silently passing.
var connectionTesters = map[string]connectionTester{
	"database-server": testDatabaseServer,
	"s3":              testS3,
}

// testDatabaseServer picks the driver from the engine field. One type means one
// contract, so the entry form is shared and only the probe differs. A database
// publishes a single address, so there is a single check.
func testDatabaseServer(ctx context.Context, values connectionValues) []models.ConnectionTestCheck {
	target := net.JoinHostPort(values.String("host"), fmt.Sprint(values.Int("port", 0)))
	var err error
	switch engine := values.String("engine"); engine {
	case "postgresql":
		err = testPostgreSQLAt(ctx, values)
	case "mysql":
		err = testMySQLAt(ctx, values)
	default:
		err = failure(models.TestReasonInvalidConfig, "Unknown database engine %q", engine)
	}
	return []models.ConnectionTestCheck{check("Database", target, true, err)}
}

// connectionValues reads submitted values, which arrive as decoded JSON.
type connectionValues map[string]any

func (v connectionValues) String(name string) string {
	if s, ok := v[name].(string); ok {
		return s
	}
	return ""
}

func (v connectionValues) Int(name string, fallback int) int {
	if n, ok := toFloat(v[name]); ok {
		return int(n)
	}
	return fallback
}

func (v connectionValues) Bool(name string, fallback bool) bool {
	if b, ok := v[name].(bool); ok {
		return b
	}
	return fallback
}

// --- PostgreSQL ---

// postgresConfig builds the driver settings from a connection's values. Kept
// apart from the probe so the field names it reads can be checked against the
// descriptor without a live server: the two went out of step once, and a probe
// reading a field nobody writes reports a wrong password instead of saying so.
func postgresConfig(values connectionValues) (*pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig("")
	if err != nil {
		return nil, failure(models.TestReasonInvalidConfig, "Could not build the connection settings: %v", err)
	}
	config.Host = values.String("host")
	config.Port = uint16(values.Int("port", 5432))
	config.Database = values.String("dbName")
	config.User = values.String("username")
	config.Password = values.String("password")

	switch values.String("sslMode") {
	case "disable":
		config.TLSConfig = nil
	case "verify-ca", "verify-full":
		config.TLSConfig = &tls.Config{ServerName: config.Host, MinVersion: tls.VersionTLS12}
	case "prefer":
		// libpq's own default: try TLS, and connect in clear text if the server
		// refuses it. Plenty of servers reachable from here (public datasets,
		// older corporate instances) offer no TLS at all, and without this the
		// only working choice would be `disable`, which never even tries.
		config.TLSConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // #nosec G402 -- same contract as 'require': encryption without certificate validation
		config.Fallbacks = []*pgconn.FallbackConfig{{
			Host:      config.Host,
			Port:      config.Port,
			TLSConfig: nil,
		}}
	default: // "require": encrypt without validating the server certificate
		config.TLSConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // #nosec G402 -- 'require' is defined by PostgreSQL as encryption without certificate validation
	}

	return config, nil
}

func testPostgreSQLAt(ctx context.Context, values connectionValues) error {
	config, err := postgresConfig(values)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return classifyPostgresError(ctx, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if err := conn.Ping(ctx); err != nil {
		return classifyPostgresError(ctx, err)
	}
	return nil
}

func classifyPostgresError(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return failure(models.TestReasonAuthFailed, "The server refused these credentials.")
		case "3D000":
			return failure(models.TestReasonNotFound, "The database does not exist on this server.")
		}
		return failure(models.TestReasonUnknown, "The server rejected the connection: %s", pgErr.Message)
	}
	return classifyNetworkError(ctx, err)
}

// --- MySQL / MariaDB ---

// mysqlConfig builds the driver settings from a connection's values, split out
// for the same reason as postgresConfig: the field names it reads are part of
// its contract with the descriptor, and must be testable without a server.
func mysqlConfig(values connectionValues) (*mysqldriver.Config, error) {
	config := mysqldriver.NewConfig()
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(values.String("host"), fmt.Sprint(values.Int("port", 3306)))
	config.DBName = values.String("dbName")
	config.User = values.String("username")
	config.Passwd = values.String("password")
	config.Timeout = testTimeout
	config.ReadTimeout = testTimeout
	config.WriteTimeout = testTimeout
	config.AllowNativePasswords = true

	switch tlsMode := values.String("tls"); tlsMode {
	case "", "false":
		config.TLSConfig = "false"
	case "preferred", "true", "skip-verify":
		config.TLSConfig = tlsMode
	default:
		return nil, failure(models.TestReasonInvalidConfig, "Unsupported TLS mode %q.", tlsMode)
	}

	return config, nil
}

func testMySQLAt(ctx context.Context, values connectionValues) error {
	config, err := mysqlConfig(values)
	if err != nil {
		return err
	}

	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return failure(models.TestReasonInvalidConfig, "Could not build the connection settings: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return classifyMySQLError(ctx, err)
	}
	return nil
}

func classifyMySQLError(ctx context.Context, err error) error {
	var mysqlErr *mysqldriver.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1045, 1044, 1698:
			return failure(models.TestReasonAuthFailed, "The server refused these credentials.")
		case 1049:
			return failure(models.TestReasonNotFound, "The database does not exist on this server.")
		}
		return failure(models.TestReasonUnknown, "The server rejected the connection: %s", mysqlErr.Message)
	}
	return classifyNetworkError(ctx, err)
}

// --- S3-compatible object storage ---

// testS3 probes every address the contract publishes. The in-cluster URL is the
// one a workload will use, so it is the decisive verdict. Testing only the
// public one, as this did, reported success for a store the deployed service
// could not reach.
func testS3(ctx context.Context, values connectionValues) []models.ConnectionTestCheck {
	internal := values.String("internalUrl")
	public := values.String("apiUrl")

	var checks []models.ConnectionTestCheck
	if internal != "" {
		checks = append(checks, check("In-cluster URL", internal, true, testS3At(ctx, values, internal)))
	}
	if public != "" {
		// Decisive only when no in-cluster address exists, in which case this is
		// the path the workload takes.
		checks = append(checks, check("Public URL", public, internal == "", testS3At(ctx, values, public)))
	}
	if len(checks) == 0 {
		checks = append(checks, check("S3 endpoint", "", true,
			failure(models.TestReasonInvalidConfig, "Endpoint is required.")))
	}
	return checks
}

func testS3At(ctx context.Context, values connectionValues, endpoint string) error {
	host, secure, err := parseS3Endpoint(endpoint)
	if err != nil {
		return err
	}

	bucket := values.String("bucket")
	lookup := minio.BucketLookupDNS
	if values.Bool("pathStyle", true) {
		lookup = minio.BucketLookupPath
	}

	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(values.String("accessKey"), values.String("secretKey"), ""),
		Secure:       secure,
		Region:       values.String("region"),
		BucketLookup: lookup,
	})
	if err != nil {
		return failure(models.TestReasonInvalidConfig, "Could not build the S3 client: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()

	// BucketExists is a HEAD: it validates the endpoint, the signature and the
	// bucket in one round trip, without reading any object.
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return classifyS3Error(ctx, err)
	}
	if !exists {
		return failure(models.TestReasonNotFound, "The bucket %q does not exist, or these credentials cannot see it.", bucket)
	}
	return nil
}

func parseS3Endpoint(endpoint string) (host string, secure bool, err error) {
	if endpoint == "" {
		return "", false, failure(models.TestReasonInvalidConfig, "Endpoint is required.")
	}
	// Accept a bare host:port as well as a full URL.
	if !strings.Contains(endpoint, "://") {
		return endpoint, true, nil
	}
	parsed, parseErr := url.Parse(endpoint)
	if parseErr != nil || parsed.Host == "" {
		return "", false, failure(models.TestReasonInvalidConfig, "Endpoint %q is not a valid URL.", endpoint)
	}
	switch parsed.Scheme {
	case "https":
		return parsed.Host, true, nil
	case "http":
		return parsed.Host, false, nil
	default:
		return "", false, failure(models.TestReasonInvalidConfig, "Endpoint scheme %q is not supported.", parsed.Scheme)
	}
}

func classifyS3Error(ctx context.Context, err error) error {
	var errResp minio.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.Code {
		case "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied":
			return failure(models.TestReasonAuthFailed, "The server refused these credentials.")
		case "NoSuchBucket":
			return failure(models.TestReasonNotFound, "The bucket does not exist on this server.")
		}
		if errResp.Message != "" {
			return failure(models.TestReasonUnknown, "The server rejected the request: %s", errResp.Message)
		}
	}
	return classifyNetworkError(ctx, err)
}

// --- shared classification ---

// classifyNetworkError turns a transport-level error into something actionable.
// The deadline is checked first: once it fires, the driver's own error is just
// whatever it happened to be doing when it was cut off.
func classifyNetworkError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return failure(models.TestReasonTimeout, "The server did not answer within %s.", testTimeout)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return failure(models.TestReasonUnreachable, "The host %q could not be resolved.", dnsErr.Name)
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return failure(models.TestReasonUnreachable, "The server could not be reached: %v", opErr.Err)
	}

	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return failure(models.TestReasonInvalidConfig, "The server certificate could not be verified. Relax the TLS setting, or make the issuing CA trusted by the cluster.")
	}

	return failure(models.TestReasonUnknown, "The connection failed: %v", err)
}
