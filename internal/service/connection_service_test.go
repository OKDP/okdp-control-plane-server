package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/okdp/okdp-control-plane-server/internal/service/mocks"
)

func newServiceUnderTest(t *testing.T, crdAvailable bool) (*DefaultConnectionService, *mocks.ConnectionRepository, *mocks.ServiceRepository) {
	t.Helper()

	connectionRepo := &mocks.ConnectionRepository{}
	releaseRepo := &mocks.ServiceRepository{}
	connectionRepo.On("Available", mock.Anything).Return(crdAvailable).Maybe()

	catalog, err := NewEmbeddedContractCatalog()
	require.NoError(t, err)

	return NewDefaultConnectionService(connectionRepo, releaseRepo, catalog), connectionRepo, releaseRepo
}

func postgresRequest() models.ConnectionRequest {
	return models.ConnectionRequest{
		Name:        "warehouse",
		Type:        "database-server",
		Description: "Corporate warehouse",
		Values: map[string]any{
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "db.example.com",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "require",
		},
	}
}

// --- Degradation while the KuboCD connection CRDs are absent ---

func TestCatalogReportsTheCRDAvailability(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	catalog := svc.Catalog(context.Background())

	assert.False(t, catalog.CRDAvailable)
	assert.NotEmpty(t, catalog.Types, "the form must be describable even without the CRDs")
}

func TestListReturnsEmptyWhenTheCRDsAreAbsent(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, false)

	connections, err := svc.List(context.Background(), "demo")

	// An uninstalled optional CRD is the normal state today, not a failure.
	require.NoError(t, err)
	assert.Empty(t, connections)
	connectionRepo.AssertNotCalled(t, "List", mock.Anything, mock.Anything)
}

func TestWritesAreRefusedWhenTheCRDsAreAbsent(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, false)

	_, createErr := svc.Create(context.Background(), "demo", postgresRequest())
	deleteErr := svc.Delete(context.Background(), "demo", "warehouse")

	assert.ErrorIs(t, createErr, ErrConnectionsUnavailable)
	assert.ErrorIs(t, deleteErr, ErrConnectionsUnavailable)
	// Nothing may be half-written: no orphan credentials Secret either.
	connectionRepo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// --- Credentials never reach the Connection spec ---

func TestCreateStoresCredentialsInASecretAndNotInTheSpec(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	var stored *crd.Connection
	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, "demo", "warehouse-credentials", mock.Anything).Return(nil)
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return((*crd.Connection)(nil), apierrors.NewNotFound(crd.GetConnectionGVR().GroupResource(), "warehouse"))
	connectionRepo.On("Create", mock.Anything, "demo", mock.Anything).Run(func(args mock.Arguments) {
		stored = args.Get(2).(*crd.Connection)
	}).Return(nil)

	response, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.NoError(t, err)

	require.NotNil(t, stored)
	assert.NotContains(t, stored.Spec.Values, "password", "the password must never be written to the CRD")
	assert.Equal(t, "db.example.com", stored.Spec.Values["host"])
	// The type name is the contract: a package asking for database-server finds
	// this connection by that name.
	assert.Equal(t, "database-server", stored.Spec.Contract)
	assert.Equal(t, "warehouse-credentials", stored.Spec.Values["secretRef"],
		"a consumer reads the Secret name from spec.values, it cannot see annotations")
	assert.NotContains(t, stored.Spec.Values, "username", "the user is a credential, it belongs to the Secret")
	assert.Equal(t, "demo/warehouse-credentials", stored.Annotations[AnnotationCredentialsSecret])

	// The response tells the console which fields are credentials without
	// disclosing them.
	assert.NotContains(t, response.Values, "password")
	assert.Contains(t, response.SecretFields, "password")

	secretCall := findCall(connectionRepo, "CreateOrUpdateSecret")
	require.NotNil(t, secretCall)
	assert.Equal(t, []byte("s3cret"), secretCall.Arguments.Get(3).(map[string][]byte)["password"])
}

func TestCreateRemovesTheSecretWhenTheConnectionIsRejected(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, "demo", "warehouse-credentials", mock.Anything).Return(nil)
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return((*crd.Connection)(nil), apierrors.NewNotFound(crd.GetConnectionGVR().GroupResource(), "warehouse"))
	connectionRepo.On("Create", mock.Anything, "demo", mock.Anything).Return(assert.AnError)
	connectionRepo.On("DeleteSecret", mock.Anything, "demo", "warehouse-credentials").Return(nil)

	_, err := svc.Create(context.Background(), "demo", postgresRequest())

	require.Error(t, err)
	connectionRepo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "warehouse-credentials")
}

func TestUpdateOnlyWritesTheResubmittedCredentials(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	existing := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Contract: "database-server"},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(existing, nil)
	connectionRepo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)
	connectionRepo.On("CreateOrUpdateSecret", mock.Anything, "demo", "warehouse-credentials", mock.Anything).Return(nil)

	req := postgresRequest()
	delete(req.Values, "password") // the console does not resend an unchanged credential

	_, err := svc.Update(context.Background(), "demo", "warehouse", req)
	require.NoError(t, err)

	// Only what was resubmitted is written. The stored password survives because
	// CreateOrUpdateSecret merges into the existing Secret instead of replacing it.
	call := findCall(connectionRepo, "CreateOrUpdateSecret")
	require.NotNil(t, call, "the resubmitted username must be written")
	written := call.Arguments.Get(3).(map[string][]byte)
	assert.Contains(t, written, "username")
	assert.NotContains(t, written, "password")
}

// TestUpdateWithNoCredentialAtAllLeavesTheSecretAlone covers the case the
// merge cannot: nothing to write means nothing is written.
func TestUpdateWithNoCredentialAtAllLeavesTheSecretAlone(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	existing := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: "demo"},
		Spec: crd.ConnectionSpec{
			Contract: "database-server",
			Values:   map[string]any{"secretRef": "warehouse-credentials"},
		},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(existing, nil)
	connectionRepo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)

	req := postgresRequest()
	delete(req.Values, "password")
	delete(req.Values, "username")

	_, err := svc.Update(context.Background(), "demo", "warehouse", req)

	require.NoError(t, err)
	connectionRepo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// --- Validation ---

func TestCreateRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*models.ConnectionRequest)
		message string
	}{
		{
			name:    "name that is not a valid Kubernetes object name",
			mutate:  func(r *models.ConnectionRequest) { r.Name = "Ware House" },
			message: "invalid connection name",
		},
		{
			name:    "unknown type",
			mutate:  func(r *models.ConnectionRequest) { r.Type = "oracle" },
			message: `unknown contract "oracle"`,
		},
		{
			// Every field is checked against the descriptor of the submitted
			// type, so a payload built for another contract is refused rather
			// than half-stored.
			name:    "values belonging to another contract",
			mutate:  func(r *models.ConnectionRequest) { r.Type = "trino" },
			message: `for contract "trino"`,
		},
		{
			name:    "value rejected by the schema",
			mutate:  func(r *models.ConnectionRequest) { delete(r.Values, "host") },
			message: "Host is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, connectionRepo, _ := newServiceUnderTest(t, true)
			req := postgresRequest()
			tt.mutate(&req)

			_, err := svc.Create(context.Background(), "demo", req)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
			assert.True(t, IsValidationError(err), "must be reported to the caller as a 400, not a 500")
			connectionRepo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestManagedConnectionsAreNotEditable(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	managed := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "trino", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Contract: "trino", OutputName: "main"},
		Status:     crd.ConnectionStatus{Parent: "demo-trino"},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "trino").Return(managed, nil)

	err := svc.Delete(context.Background(), "demo", "trino")

	require.Error(t, err)
	// The message names the service, so the reader knows what to remove if they
	// really want the connection gone.
	assert.Contains(t, err.Error(), "demo-trino")
	assert.Contains(t, err.Error(), "cannot be deleted")
	connectionRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything, mock.Anything)
}

// The controller fills status.parent only once it has reconciled, so a managed
// connection can legitimately have none, so the refusal must still read
// sensibly.
func TestManagedConnectionWithoutAParentIsStillRefused(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	managed := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "trino", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Contract: "trino", OutputName: "main"},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "trino").Return(managed, nil)

	err := svc.Delete(context.Background(), "demo", "trino")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "provided by a deployed service")
	assert.NotContains(t, err.Error(), `""`)
	connectionRepo.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything, mock.Anything)
}

func TestListExcludesManagedConnections(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "warehouse"},
			Spec:       crd.ConnectionSpec{Contract: "database-server"},
		},
		{
			// Owned by a release: it belongs to the internal view.
			ObjectMeta: metav1.ObjectMeta{Name: "trino"},
			Spec:       crd.ConnectionSpec{Contract: "trino", OutputName: "main"},
		},
	}, nil)

	connections, err := svc.List(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, "warehouse", connections[0].Name)
}

// --- Internal connections ---

// The tab lists what the project's services actually publish, and only that:
// the picker offers Connections, so an entry without one behind it could not be
// bound while looking exactly like the ones that can.
func TestListInternalOnlyListsPublishedConnections(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "kcd-demo-trino-main"},
			Spec: crd.ConnectionSpec{
				Contract:   "trino",
				OutputName: "main",
				Values:     map[string]any{"internalUri": "jdbc:trino://trino.demo.svc.cluster.local:8080"},
			},
			Status: crd.ConnectionStatus{Parent: "demo-trino", Phase: "READY"},
		},
		{
			// Declared by a user on the Connections page, not published by a
			// release: it belongs to the external tab.
			ObjectMeta: metav1.ObjectMeta{Name: "corporate-warehouse"},
			Spec:       crd.ConnectionSpec{Contract: "database-server"},
		},
	}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, "kcd-demo-trino-main", connections[0].Name)
	assert.True(t, connections[0].Managed)
	assert.Equal(t, "demo-trino", connections[0].ReleaseName)
}

// A trino connection carries URIs, never a host and a port. Reading host+port
// only left the address column on "not available yet" for a Ready connection
// whose address was in its own values.
func TestListInternalShowsTheAddressOfAURIContract(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("List", mock.Anything, "demo").Return([]crd.Connection{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "kcd-demo-hms-metastore"},
			Spec: crd.ConnectionSpec{
				Contract:   "hive",
				OutputName: "metastore",
				Values:     map[string]any{"thriftUri": "thrift://demo-hms.demo.svc.cluster.local:9083"},
			},
			Status: crd.ConnectionStatus{Parent: "demo-hms", Phase: "READY"},
		},
	}, nil)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	require.Len(t, connections, 1)
	assert.Equal(t, "thrift://demo-hms.demo.svc.cluster.local:9083", connections[0].Endpoint)
	assert.Equal(t, "Hive metastore", connections[0].TypeDisplay)
}

// Without the CRDs there is nothing to publish a connection, so the honest
// answer is an empty list rather than a set of guesses.
func TestListInternalIsEmptyWithoutTheCRDs(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	connections, err := svc.ListInternal(context.Background(), "demo")

	require.NoError(t, err)
	assert.Empty(t, connections)
}

// --- Connectivity test ---

func TestTestRejectsBadInputBeforeDialing(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	tests := []struct {
		name    string
		request models.ConnectionTestRequest
		reason  string
	}{
		{
			name:    "unknown type",
			request: models.ConnectionTestRequest{Type: "oracle", Values: map[string]any{}},
			reason:  models.TestReasonInvalidConfig,
		},
		{
			name:    "missing required value",
			request: models.ConnectionTestRequest{Type: "database-server", Values: map[string]any{"host": "db"}},
			reason:  models.TestReasonInvalidConfig,
		},
		{
			name: "type that has no tester",
			request: models.ConnectionTestRequest{Type: "trino", Values: map[string]any{
				"host": "trino.demo.svc.cluster.local", "port": float64(8080),
			}},
			reason: models.TestReasonInvalidConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := svc.Test(context.Background(), tt.request)

			assert.False(t, result.Success)
			assert.Equal(t, tt.reason, result.Reason)
			assert.NotEmpty(t, result.Message)
		})
	}
}

func TestTestReportsAnUnreachableHost(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	result := svc.Test(context.Background(), models.ConnectionTestRequest{
		Type: "database-server",
		Values: map[string]any{
			// Reserved by RFC 6761 to never resolve.
			"engine":   "postgresql",
			"driver":   "org.postgresql.Driver",
			"host":     "nothing.invalid",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "disable",
		},
	})

	assert.False(t, result.Success)
	assert.Contains(t, []string{models.TestReasonUnreachable, models.TestReasonTimeout}, result.Reason)
}

// One type, one form, two drivers: the probe reads the engine field. An engine
// the server has no driver for must say so rather than fall back to a default,
// which would report a MySQL server unreachable on the PostgreSQL port. The
// catalog's enum stops this at the door, so the guard is tested directly.
func TestDatabaseProbeRejectsAnUnknownEngine(t *testing.T) {
	checks := testDatabaseServer(context.Background(), connectionValues{
		"engine": "oracle",
		"host":   "db.example.com",
		"port":   float64(1521),
	})

	require.Len(t, checks, 1)
	assert.False(t, checks[0].Success)
	assert.Equal(t, models.TestReasonInvalidConfig, checks[0].Reason)
	assert.Contains(t, checks[0].Message, "oracle")
}

// findCall returns the first recorded call to method, or nil.
func findCall(m *mocks.ConnectionRepository, method string) *mock.Call {
	for i := range m.Calls {
		if m.Calls[i].Method == method {
			return &m.Calls[i]
		}
	}
	return nil
}

// The form does not send a derived field, so the button must test the values as
// they will be stored, not the raw payload. Validating before normalizing asked
// for a JDBC driver nobody was offered, and every test came back "JDBC driver is
// required" without a packet ever leaving the server.
func TestTestNormalizesBeforeValidating(t *testing.T) {
	svc, _, _ := newServiceUnderTest(t, false)

	result := svc.Test(context.Background(), models.ConnectionTestRequest{
		Type: "database-server",
		Values: map[string]any{
			"engine": "postgresql",
			// Reserved by RFC 6761 to never resolve, so the probe fails on the
			// network rather than on the payload.
			"host":     "nothing.invalid",
			"port":     float64(5432),
			"dbName":   "analytics",
			"username": "reader",
			"password": "s3cret",
			"sslMode":  "disable",
		},
	})

	assert.False(t, result.Success)
	assert.NotEqual(t, models.TestReasonInvalidConfig, result.Reason,
		"the payload is complete once normalized, the failure must come from the network")
}

// --- Pointing at a Secret the console does not own ---

// Credentials increasingly arrive in the namespace on their own, projected from
// a vault by External Secrets. A connection must be able to name one instead of
// handing its password to a form.
func TestCreateCanPointAtAnExistingSecret(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("SecretKeys", mock.Anything, "demo", "warehouse-from-vault").
		Return([]string{"password", "username"}, true, nil)
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return((*crd.Connection)(nil), apierrors.NewNotFound(crd.GetConnectionGVR().GroupResource(), "warehouse"))
	connectionRepo.On("Create", mock.Anything, "demo", mock.Anything).Return(nil)

	req := postgresRequest()
	req.ExistingSecret = "warehouse-from-vault"
	delete(req.Values, "username")
	delete(req.Values, "password")

	_, err := svc.Create(context.Background(), "demo", req)

	require.NoError(t, err)
	// The Secret belongs to somebody else: writing it would overwrite what the
	// vault projected.
	connectionRepo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)

	call := findCall(connectionRepo, "Create")
	require.NotNil(t, call)
	stored := call.Arguments.Get(2).(*crd.Connection)
	assert.Equal(t, "warehouse-from-vault", stored.Spec.Values["secretRef"],
		"consumers bind the Secret through this name")
}

// A connection pointing at a Secret that is absent, or that does not carry what
// the contract needs, looks healthy and fails at pod start with
// CreateContainerConfigError, far from the form that caused it.
func TestCreateRefusesASecretThatCannotWork(t *testing.T) {
	tests := []struct {
		name    string
		keys    []string
		found   bool
		message string
	}{
		{
			name:    "secret does not exist",
			found:   false,
			message: `no secret named "warehouse-from-vault"`,
		},
		{
			name:    "secret is missing a key the contract needs",
			keys:    []string{"username"},
			found:   true,
			message: "does not carry the keys the database-server contract needs: password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, connectionRepo, _ := newServiceUnderTest(t, true)
			connectionRepo.On("SecretKeys", mock.Anything, "demo", "warehouse-from-vault").
				Return(tt.keys, tt.found, nil)

			req := postgresRequest()
			req.ExistingSecret = "warehouse-from-vault"

			_, err := svc.Create(context.Background(), "demo", req)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
			assert.True(t, IsValidationError(err), "must reach the caller as a 400")
			connectionRepo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

// --- Consumers ---

// Deleting a connection takes its Secret with it, and the pods mounting that
// Secret fail at their next restart with CreateContainerConfigError, far from
// the dialog that caused it. The controller publishes who consumes what, so the
// console can say it beforehand instead of guessing from parameters.
func TestListConsumersReadsWhatTheControllerPublished(t *testing.T) {
	svc, _, releaseRepo := newServiceUnderTest(t, true)

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-hms"},
			Status: crd.ReleaseStatus{
				Phase: "READY",
				EffectiveInputConnections: []crd.InputConnectionReference{
					{Kind: "Connection", Name: "demo-db", Namespace: "demo"},
				},
			},
		},
		{
			// Waiting for the same connection: it counts as a consumer, but it
			// never resolved it.
			ObjectMeta: metav1.ObjectMeta{Name: "demo-superset"},
			Status: crd.ReleaseStatus{
				Phase: "WAIT_ICNX",
				WatchedInputConnections: []crd.InputConnectionReference{
					{Kind: "Connection", Name: "demo-db", Namespace: "demo"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-trino"},
			Status:     crd.ReleaseStatus{Phase: "READY"},
		},
	}, nil)

	consumers, err := svc.ListConsumers(context.Background(), "demo", "demo-db")

	require.NoError(t, err)
	require.Len(t, consumers, 2)
	assert.Equal(t, "hms", consumers[0].Service, "the instance name, as the console shows it")
	assert.True(t, consumers[0].Effective)
	assert.Equal(t, "superset", consumers[1].Service)
	assert.False(t, consumers[1].Effective, "waiting for a connection is not running on it")
}

// A ClusterConnection has no namespace of its own, so a reference to one comes
// back with an empty namespace and must still match.
func TestListConsumersMatchesAPlatformConnection(t *testing.T) {
	svc, _, releaseRepo := newServiceUnderTest(t, true)

	releaseRepo.On("List", mock.Anything, "demo", "demo").Return([]crd.Release{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-superset"},
			Status: crd.ReleaseStatus{
				Phase: "READY",
				EffectiveInputConnections: []crd.InputConnectionReference{
					{Kind: "ClusterConnection", Name: "shared-warehouse"},
				},
			},
		},
	}, nil)

	consumers, err := svc.ListConsumers(context.Background(), "demo", "shared-warehouse")

	require.NoError(t, err)
	require.Len(t, consumers, 1)
	assert.Equal(t, "superset", consumers[0].Service)
}

// The panel says where the credentials come from, because the two cases behave
// differently: deleting a connection removes a Secret the console wrote, and
// leaves an external one alone. Reading the Secret back to find out would cost
// one API call per connection in a list, so the answer is written down when the
// connection is created.
func TestResponseSaysWhetherTheConsoleOwnsTheSecret(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		connection  string
		wantOwned   bool
	}{
		{
			name: "written by the console",
			annotations: map[string]string{
				AnnotationCredentialsSecret: "demo/warehouse-credentials",
				AnnotationCredentialsOwned:  "true",
			},
			connection: "warehouse",
			wantOwned:  true,
		},
		{
			name: "pointing at a secret from elsewhere",
			annotations: map[string]string{
				AnnotationCredentialsSecret: "demo/from-vault",
				AnnotationCredentialsOwned:  "false",
			},
			connection: "warehouse",
			wantOwned:  false,
		},
		{
			// Created before the annotation existed: the naming convention is
			// the only clue, and it is what the console itself assumed.
			name:        "older connection, no annotation",
			annotations: map[string]string{AnnotationCredentialsSecret: "demo/warehouse-credentials"},
			connection:  "warehouse",
			wantOwned:   true,
		},
		{
			name:        "older connection pointing elsewhere",
			annotations: map[string]string{AnnotationCredentialsSecret: "demo/shared-db-secret"},
			connection:  "warehouse",
			wantOwned:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newServiceUnderTest(t, true)
			connection := &crd.Connection{
				ObjectMeta: metav1.ObjectMeta{Name: tt.connection, Annotations: tt.annotations},
				Spec:       crd.ConnectionSpec{Contract: "database-server"},
			}

			response := svc.toResponse(connection, "demo")

			require.NotNil(t, response.CredentialsSecret)
			assert.Equal(t, tt.wantOwned, response.CredentialsSecret.Owned)
		})
	}
}

// The panel tells the user an external Secret stays when the connection goes.
// It only did so because the name did not match the convention: Delete removed
// `<connection>-credentials` whatever it was. A vault-projected Secret named
// that way would have been destroyed, taking the credentials of everything else
// reading it.
func TestDeleteLeavesASecretItDoesNotOwn(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(&crd.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warehouse",
			Annotations: map[string]string{
				// Named exactly as the console would have named its own.
				AnnotationCredentialsSecret: "demo/warehouse-credentials",
				AnnotationCredentialsOwned:  "false",
			},
		},
	}, nil)
	connectionRepo.On("Delete", mock.Anything, "demo", "warehouse").Return(nil)

	require.NoError(t, svc.Delete(context.Background(), "demo", "warehouse"))

	connectionRepo.AssertNotCalled(t, "DeleteSecret", mock.Anything, mock.Anything, mock.Anything)
}

func TestDeleteRemovesTheSecretItWrote(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(&crd.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: "warehouse",
			Annotations: map[string]string{
				AnnotationCredentialsSecret: "demo/warehouse-credentials",
				AnnotationCredentialsOwned:  "true",
			},
		},
	}, nil)
	connectionRepo.On("Delete", mock.Anything, "demo", "warehouse").Return(nil)
	connectionRepo.On("DeleteSecret", mock.Anything, "demo", "warehouse-credentials").Return(nil)

	require.NoError(t, svc.Delete(context.Background(), "demo", "warehouse"))

	connectionRepo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "warehouse-credentials")
}

// A repeated create must not touch the credentials Secret of the connection
// that already holds the name: merging into it and then rolling it back would
// take the running consumers' credentials with it.
func TestCreateOnATakenNameLeavesTheCredentialsAlone(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(&crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: "demo"},
		Spec:       crd.ConnectionSpec{Contract: "database-server"},
	}, nil)

	_, err := svc.Create(context.Background(), "demo", postgresRequest())
	require.Error(t, err)
	require.True(t, apierrors.IsAlreadyExists(err))

	connectionRepo.AssertNotCalled(t, "CreateOrUpdateSecret", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	connectionRepo.AssertNotCalled(t, "DeleteSecret", mock.Anything, mock.Anything, mock.Anything)
	connectionRepo.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything)
}

// The Secret name is published in spec.values for the consumers, so a client
// that edits what it read sends it back. It must not be rejected as an unknown
// field, and it must survive the edit.
func TestUpdateAcceptsTheValuesItReturned(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	existing := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "warehouse", Namespace: "demo"},
		Spec: crd.ConnectionSpec{
			Contract: "database-server",
			Values:   map[string]any{"secretRef": "warehouse-credentials"},
		},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(existing, nil)
	connectionRepo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)

	req := postgresRequest()
	delete(req.Values, "password")
	delete(req.Values, "username")
	req.Values["secretRef"] = "warehouse-credentials" // as read from the API
	req.Values["port"] = float64(5433)

	response, err := svc.Update(context.Background(), "demo", "warehouse", req)
	require.NoError(t, err)
	assert.Equal(t, "warehouse-credentials", existing.Spec.Values["secretRef"])
	assert.Equal(t, float64(5433), response.Values["port"])
}

// Pointing a connection at a Secret somebody else owns leaves the one the
// console wrote unreferenced: nothing would ever remove it, since deleting the
// connection now reads it as not ours.
func TestUpdateToAnExistingSecretRemovesTheOneItOwned(t *testing.T) {
	svc, connectionRepo, _ := newServiceUnderTest(t, true)

	existing := &crd.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "warehouse",
			Namespace: "demo",
			Annotations: map[string]string{
				AnnotationCredentialsSecret: "demo/warehouse-credentials",
				AnnotationCredentialsOwned:  "true",
			},
		},
		Spec: crd.ConnectionSpec{
			Contract: "database-server",
			Values:   map[string]any{"secretRef": "warehouse-credentials"},
		},
	}
	connectionRepo.On("Get", mock.Anything, "demo", "warehouse").Return(existing, nil)
	connectionRepo.On("Update", mock.Anything, "demo", mock.Anything).Return(nil)
	connectionRepo.On("SecretKeys", mock.Anything, "demo", "warehouse-from-vault").
		Return([]string{"password", "username"}, true, nil)
	connectionRepo.On("DeleteSecret", mock.Anything, "demo", "warehouse-credentials").Return(nil)

	req := postgresRequest()
	req.ExistingSecret = "warehouse-from-vault"
	delete(req.Values, "password")
	delete(req.Values, "username")

	_, err := svc.Update(context.Background(), "demo", "warehouse", req)
	require.NoError(t, err)

	connectionRepo.AssertCalled(t, "DeleteSecret", mock.Anything, "demo", "warehouse-credentials")
	assert.Equal(t, "warehouse-from-vault", existing.Spec.Values["secretRef"])
}
