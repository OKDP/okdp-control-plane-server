package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/okdp/okdp-control-plane-server/internal/auth"
	"github.com/okdp/okdp-control-plane-server/internal/gitops"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/sirupsen/logrus"
)

// credentialsSecretSuffix names the Secret the console writes for a
// connection: <connection>-credentials, in the project namespace.
const credentialsSecretSuffix = "-credentials"

// valueSecretRef is the key under which a connection publishes the NAME of
// the Secret holding its credentials (never a value): secretRef.name in the
// connection file, the field the contract schemas declare.
const valueSecretRef = "secretRef"

// connectionsResource names the resource in not-found and conflict errors.
var connectionsResource = schema.GroupResource{Group: "okdp.io", Resource: "connections"}

// ConnectionService manages the connections of a project.
//
// External connections are files of the deployments Git repository
// (projects/<p>/connections/<name>.yaml) plus a credentials Secret in the
// project namespace. Internal connections are what the deployed instances
// publish in their descriptor (outputs.yaml).
type ConnectionService interface {
	// Catalog returns the known contracts and whether connections can currently
	// be persisted.
	Catalog(ctx context.Context) models.ConnectionCatalogResponse

	List(ctx context.Context, namespace string) ([]models.ConnectionResponse, error)
	Create(ctx context.Context, namespace string, req models.ConnectionRequest) (*models.ConnectionResponse, error)
	Update(ctx context.Context, namespace, name string, req models.ConnectionRequest) (*models.ConnectionResponse, error)
	Delete(ctx context.Context, namespace, name string) error

	// Test probes a live endpoint with the submitted values. It never touches
	// the cluster, so a connection can be validated before it is created.
	Test(ctx context.Context, req models.ConnectionTestRequest) models.ConnectionTestResult

	// ListInternal returns the connections provided by the services already
	// deployed in a project, consumable by the project's other services.
	ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error)

	// ListSelectable returns the connections a deployment form can offer for an
	// input of the given contract: the project's external ones and the ones
	// its deployed instances provide.
	ListSelectable(ctx context.Context, project, contract string) ([]models.SelectableConnection, error)

	// ListConsumers returns the services of a project bound to a connection:
	// the instances whose declaration layers it in or whose parameters name it.
	ListConsumers(ctx context.Context, project, name string) ([]models.ConnectionConsumer, error)
}

// InstanceLister lists the instances of a project, with their status and
// bound connections (ServiceService.ListServices).
type InstanceLister func(ctx context.Context, project string) ([]models.ServiceInstance, error)

type DefaultConnectionService struct {
	deployments *gitops.Deployments
	secrets     repository.ConnectionSecretRepository
	descriptors repository.DescriptorRepository
	instances   InstanceLister
	catalog     ContractCatalog
}

func NewDefaultConnectionService(
	deployments *gitops.Deployments,
	secrets repository.ConnectionSecretRepository,
	descriptors repository.DescriptorRepository,
	instances InstanceLister,
	catalog ContractCatalog,
) *DefaultConnectionService {
	return &DefaultConnectionService{
		deployments: deployments,
		secrets:     secrets,
		descriptors: descriptors,
		instances:   instances,
		catalog:     catalog,
	}
}

// ErrConnectionsUnavailable is returned by the write paths while no
// deployments repository is configured. The handler turns it into a 501.
var ErrConnectionsUnavailable = fmt.Errorf("no deployments repository is configured, connections cannot be stored")

// ValidationError is a problem with what was submitted, as opposed to a failure
// of the platform. It lets the handler answer 400 rather than 500 without
// having to guess from the message.
type ValidationError struct{ message string }

func (e *ValidationError) Error() string { return e.message }

func invalid(format string, args ...any) error {
	return &ValidationError{message: fmt.Sprintf(format, args...)}
}

// storeCredentialsError turns a refused adoption into a rejected input: naming
// a Secret the control plane does not own is a bad request, not a platform
// failure, and the console can say which name to change.
func storeCredentialsError(err error) error {
	if errors.Is(err, repository.ErrForeignSecret) {
		return invalid("%v", err)
	}
	return fmt.Errorf("failed to store the credentials: %w", err)
}

// IsValidationError reports whether err is a rejected user input.
func IsValidationError(err error) bool {
	var validationErr *ValidationError
	return errors.As(err, &validationErr)
}

// IsNotFound reports whether an error from this service is a missing resource,
// so the handler can answer 404 without importing the Kubernetes error package.
func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// connectionGitError maps the deployments repository errors to the API ones.
func connectionGitError(err error, name string) error {
	var inUse *gitops.ErrInUse
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gitops.ErrNotFound):
		return apierrors.NewNotFound(connectionsResource, name)
	case errors.Is(err, gitops.ErrExists):
		return apierrors.NewAlreadyExists(connectionsResource, name)
	case errors.As(err, &inUse):
		return invalid("connection %q is still layered into %s; change them first", name, strings.Join(inUse.Users, ", "))
	default:
		return err
	}
}

func (s *DefaultConnectionService) available() bool {
	return s.deployments != nil
}

func (s *DefaultConnectionService) Catalog(ctx context.Context) models.ConnectionCatalogResponse {
	return models.ConnectionCatalogResponse{
		Types:        s.catalog.List(),
		CRDAvailable: s.available(),
	}
}

// --- External connections ---

func (s *DefaultConnectionService) List(ctx context.Context, namespace string) ([]models.ConnectionResponse, error) {
	if !s.available() {
		return []models.ConnectionResponse{}, nil
	}
	connections, err := s.deployments.ListConnections(ctx, namespace)
	if err != nil {
		return nil, err
	}
	result := make([]models.ConnectionResponse, 0, len(connections))
	for i := range connections {
		result = append(result, s.toResponse(&connections[i]))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// withoutSecretIfNone drops a named Secret for a contract without secret
// fields: such a connection carries no secretRef at all.
func (s *DefaultConnectionService) withoutSecretIfNone(req models.ConnectionRequest) models.ConnectionRequest {
	if descriptor, known := s.catalog.Get(req.Type); known && len(descriptor.SecretFields()) == 0 {
		req.ExistingSecret = ""
	}
	return req
}

func (s *DefaultConnectionService) Create(ctx context.Context, namespace string, req models.ConnectionRequest) (*models.ConnectionResponse, error) {
	req = s.withoutSecretIfNone(req)
	descriptor, values, err := s.validateRequest(ctx, namespace, req, false)
	if err != nil {
		return nil, err
	}

	// The name has to be free before anything is written: the credentials Secret
	// of a live connection follows the same naming convention, so writing it
	// first would let a repeated create merge into it and the rollback below
	// delete it, taking the running consumers' credentials with it.
	if _, err := s.deployments.GetConnection(ctx, namespace, req.Name); err == nil {
		return nil, apierrors.NewAlreadyExists(connectionsResource, req.Name)
	} else if !errors.Is(err, gitops.ErrNotFound) {
		return nil, err
	}

	public, secrets := splitValues(descriptor, values)
	if err := s.catalog.ValidatePublic(descriptor.Name, public); err != nil {
		return nil, invalid("%v", err)
	}
	secretName := ""
	ownSecret := req.ExistingSecret == ""

	if !ownSecret {
		// Pointing at a Secret somebody else owns: nothing is written, and the
		// credential fields of the payload are ignored on purpose.
		secretName = req.ExistingSecret
	} else if len(secrets) > 0 {
		secretName = req.Name + credentialsSecretSuffix
		if err := s.secrets.CreateOrUpdateSecret(ctx, namespace, secretName, secrets); err != nil {
			return nil, storeCredentialsError(err)
		}
	}

	connection := gitops.Connection{
		Name:        req.Name,
		Project:     namespace,
		Contract:    descriptor.Name,
		Description: req.Description,
		Values:      public,
		SecretRef:   secretName,
	}
	if _, err := s.deployments.PutConnection(ctx, auth.ActorFrom(ctx), connection, true); err != nil {
		// Leave no orphan Secret behind when the connection itself is refused. A
		// Secret we do not own is never touched, and neither is the one a
		// concurrent create under the same name has just bound to its own
		// connection.
		if ownSecret && len(secrets) > 0 && !errors.Is(err, gitops.ErrExists) {
			if cleanupErr := s.secrets.DeleteSecret(ctx, namespace, secretName); cleanupErr != nil {
				logrus.WithError(cleanupErr).Warn("Failed to clean up the credentials secret of a rejected connection")
			}
		}
		return nil, connectionGitError(err, req.Name)
	}

	response := s.toResponse(&connection)
	return &response, nil
}

func (s *DefaultConnectionService) Update(ctx context.Context, namespace, name string, req models.ConnectionRequest) (*models.ConnectionResponse, error) {
	req.Name = name
	req = s.withoutSecretIfNone(req)
	descriptor, values, err := s.validateRequest(ctx, namespace, req, true)
	if err != nil {
		return nil, err
	}

	existing, err := s.deployments.GetConnection(ctx, namespace, name)
	if err != nil {
		return nil, connectionGitError(err, name)
	}

	public, secrets := splitValues(descriptor, values)
	if err := s.catalog.ValidatePublic(descriptor.Name, public); err != nil {
		return nil, invalid("%v", err)
	}
	ownedName := name + credentialsSecretSuffix
	secretName := existing.SecretRef

	// Set when the connection moves off a Secret we owned. The old Secret is
	// dropped only once the new reference is persisted, otherwise a failed
	// update leaves the stored connection pointing at a Secret that no longer
	// exists.
	orphanedSecret := ""

	if len(descriptor.SecretFields()) == 0 {
		// No secret field, no secretRef.
		req.ExistingSecret, secretName = "", ""
		if existing.SecretRef == ownedName {
			orphanedSecret = ownedName
		}
	} else if req.ExistingSecret != "" {
		if existing.SecretRef == ownedName && req.ExistingSecret != ownedName {
			orphanedSecret = ownedName
		}
		secretName = req.ExistingSecret
	} else if len(secrets) > 0 {
		// An unchanged credential is not resubmitted by the console, so only
		// write the Secret when new values actually arrived, otherwise an edit
		// of, say, the port would blank out the password.
		if err := s.secrets.CreateOrUpdateSecret(ctx, namespace, ownedName, secrets); err != nil {
			return nil, storeCredentialsError(err)
		}
		secretName = ownedName
	}

	updated := gitops.Connection{
		Name:        name,
		Project:     namespace,
		Contract:    descriptor.Name,
		Description: req.Description,
		Values:      public,
		SecretRef:   secretName,
	}
	if _, err := s.deployments.PutConnection(ctx, auth.ActorFrom(ctx), updated, false); err != nil {
		return nil, connectionGitError(err, name)
	}

	if orphanedSecret != "" {
		s.deleteOwnedSecret(ctx, namespace, orphanedSecret)
	}

	response := s.toResponse(&updated)
	return &response, nil
}

func (s *DefaultConnectionService) Delete(ctx context.Context, namespace, name string) error {
	if !s.available() {
		return ErrConnectionsUnavailable
	}
	existing, err := s.deployments.GetConnection(ctx, namespace, name)
	if err != nil {
		return connectionGitError(err, name)
	}
	if _, err := s.deployments.DeleteConnection(ctx, auth.ActorFrom(ctx), namespace, name); err != nil {
		return connectionGitError(err, name)
	}

	// Only a Secret this server wrote is removed with the connection. One that
	// was already there, projected from a vault, belongs to whoever put it
	// there: deleting it would take the credentials of everything else reading
	// it.
	if existing.SecretRef == "" {
		return nil
	}
	if existing.SecretRef != name+credentialsSecretSuffix {
		logrus.WithField("secret", existing.SecretRef).Info("Leaving the credentials secret in place, the connection did not own it")
		return nil
	}
	s.deleteOwnedSecret(ctx, namespace, existing.SecretRef)
	return nil
}

// deleteOwnedSecret removes a credentials Secret, only when it carries the
// console's managed-by label: the name alone is no proof, an external Secret
// being free to follow the same convention.
func (s *DefaultConnectionService) deleteOwnedSecret(ctx context.Context, namespace, name string) {
	content, found, err := s.secrets.InspectSecret(ctx, namespace, name)
	if err != nil {
		logrus.WithError(err).WithField("secret", name).Warn("Could not inspect the credentials secret, leaving it in place")
		return
	}
	if !found {
		return
	}
	if !content.Managed {
		logrus.WithField("secret", name).Info("Leaving the credentials secret in place, the console did not write it")
		return
	}
	// The Secret outlives the connection only if this fails. Report nothing to
	// the user for it, the connection itself is gone.
	if err := s.secrets.DeleteSecret(ctx, namespace, name); err != nil {
		logrus.WithError(err).WithField("secret", name).Warn("Failed to delete the credentials secret of a connection")
	}
}

func (s *DefaultConnectionService) Test(ctx context.Context, req models.ConnectionTestRequest) models.ConnectionTestResult {
	started := time.Now()

	result := func(success bool, reason, message string) models.ConnectionTestResult {
		return models.ConnectionTestResult{
			Success:    success,
			Reason:     reason,
			Message:    message,
			DurationMs: time.Since(started).Milliseconds(),
		}
	}

	if _, known := s.catalog.Get(req.Type); !known {
		return result(false, models.TestReasonInvalidConfig, fmt.Sprintf("Unknown contract %q.", req.Type))
	}
	// Normalize first, exactly as Create and Update do: the form does not send a
	// derived field, so validating the raw payload would demand a JDBC driver
	// nobody was asked for. Testing the normalized values is also the point of
	// the button, which is to try what will actually be stored.
	values := s.catalog.Normalize(req.Type, req.Values)
	if err := s.catalog.Validate(req.Type, values); err != nil {
		return result(false, models.TestReasonInvalidConfig, err.Error())
	}

	tester, ok := connectionTesters[req.Type]
	if !ok {
		return result(false, models.TestReasonInvalidConfig, "This contract cannot be tested.")
	}

	// A contract may publish several addresses. The verdict is the one a
	// workload will actually take, and the other checks travel along for
	// diagnosis: a store reachable publicly and unreachable in-cluster passes
	// and fails at once.
	checks := tester(ctx, values)
	outcome := result(true, "", "Connection successful.")
	outcome.Checks = checks
	for _, c := range checks {
		if !c.Decisive {
			continue
		}
		outcome.Success = c.Success
		outcome.Message = c.Message
		outcome.Reason = c.Reason
	}
	return outcome
}

// --- Internal connections ---

// outputsOf lists the connections the deployed instances of a project
// publish in their descriptor.
func (s *DefaultConnectionService) outputsOf(ctx context.Context, project string) ([]repository.Descriptor, error) {
	if s.descriptors == nil {
		return nil, nil
	}
	return s.descriptors.List(ctx, project)
}

// internalCapable reports whether an instance output of that contract can be
// referenced by name: okdp.connection resolves an internal reference only for
// contracts declaring a naming convention (x-okdp-internal). The others (s3,
// database-server) are external only.
func (s *DefaultConnectionService) internalCapable(contract string) bool {
	descriptor, known := s.catalog.Get(contract)
	return known && descriptor.Internal
}

// ListInternal returns the connections the project's own instances publish
// (their descriptor's outputs.yaml) that other instances can reference by
// name, and only those.
func (s *DefaultConnectionService) ListInternal(ctx context.Context, project string) ([]models.InternalConnection, error) {
	descriptors, err := s.outputsOf(ctx, project)
	if err != nil {
		return nil, err
	}
	result := make([]models.InternalConnection, 0)
	for _, d := range descriptors {
		for _, output := range d.Outputs {
			if !s.internalCapable(output.Contract) {
				continue
			}
			result = append(result, s.outputToInternal(d, output, project))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (s *DefaultConnectionService) outputToInternal(d repository.Descriptor, output repository.DescriptorOutput, project string) models.InternalConnection {
	values := output.Values
	if values == nil {
		values = map[string]any{}
	}
	entry := models.InternalConnection{
		Name:        output.Name,
		Type:        output.Contract,
		TypeDisplay: output.Contract,
		Service:     d.Service,
		ReleaseName: d.Release,
		Namespace:   project,
		// The descriptor exists: the chart rendered and the engine applied it.
		Status:  "Ready",
		Values:  values,
		Managed: true,
	}
	if descriptor, known := s.catalog.Get(output.Contract); known {
		entry.Icon = descriptor.Icon
		entry.Category = descriptor.Category
		entry.TypeDisplay = descriptor.DisplayName
	}
	entry.Endpoint = endpointFrom(values, s.catalog, output.Contract)
	// host and port stay filled when the contract has them, for the columns that
	// show them separately. Most contracts publish a URI instead.
	if host, ok := values["host"].(string); ok {
		entry.Host = host
		if port, ok := toFloat(values["port"]); ok {
			entry.Port = int32(port)
		}
	}
	if !d.CreatedAt.IsZero() {
		entry.CreatedAt = d.CreatedAt.UTC().Format(time.RFC3339)
	}
	return entry
}

// --- helpers ---

// validateRequest checks a submitted connection and returns the values as they
// should be stored: derived fields filled, fields put out of play by the
// submitted engine dropped. On an edit, credentials that were not resubmitted
// are kept as they are rather than reported as missing.
func (s *DefaultConnectionService) validateRequest(ctx context.Context, namespace string, req models.ConnectionRequest, isUpdate bool) (*models.ContractDescriptor, map[string]any, error) {
	if !s.available() {
		return nil, nil, ErrConnectionsUnavailable
	}
	if errs := validation.IsDNS1123Label(req.Name); len(errs) > 0 {
		return nil, nil, invalid("invalid connection name %q: %s", req.Name, errs[0])
	}

	descriptor, known := s.catalog.Get(req.Type)
	if !known {
		return nil, nil, invalid("unknown contract %q", req.Type)
	}
	if !descriptor.External {
		return nil, nil, invalid("connections of type %q come from a deployed service and cannot be declared by hand", req.Type)
	}
	values := s.catalog.Normalize(req.Type, req.Values)
	// The Secret name is published with the values for the consumers, so a
	// client editing what it just read sends it back. No contract field
	// declares it, and the write paths set it again from the request.
	delete(values, valueSecretRef)
	validateValues := s.catalog.Validate
	if isUpdate {
		validateValues = s.catalog.ValidateUpdate
	}
	if req.ExistingSecret != "" {
		// The credentials live in a Secret we do not own, so the payload is not
		// expected to carry them: validate as an edit, which tolerates that.
		validateValues = s.catalog.ValidateUpdate
		if err := s.checkExistingSecret(ctx, namespace, req, descriptor); err != nil {
			return nil, nil, err
		}
	}
	if err := validateValues(req.Type, values); err != nil {
		return nil, nil, invalid("%s", err.Error())
	}
	return descriptor, values, nil
}

// checkExistingSecret refuses a connection pointed at a Secret that is absent or
// does not carry what the contract needs. Storing it anyway would produce a
// connection that looks healthy and whose consumers fail at pod start with
// CreateContainerConfigError, far from the form that caused it.
func (s *DefaultConnectionService) checkExistingSecret(
	ctx context.Context,
	namespace string,
	req models.ConnectionRequest,
	descriptor *models.ContractDescriptor,
) error {
	secretNamespace := namespace
	content, found, err := s.secrets.InspectSecret(ctx, secretNamespace, req.ExistingSecret)
	if err != nil {
		return fmt.Errorf("failed to read the secret %q: %w", req.ExistingSecret, err)
	}
	if !found {
		return invalid("no secret named %q in namespace %q", req.ExistingSecret, secretNamespace)
	}
	// A Secret this server wrote belongs to the connection it was created for,
	// and goes away with it. Pointing a second connection at it would leave that
	// one credentialless the day the first is deleted.
	if content.Managed {
		return invalid("secret %q belongs to another connection, name one this server does not manage", req.ExistingSecret)
	}

	present := make(map[string]bool, len(content.Keys))
	for _, key := range content.Keys {
		present[key] = true
	}
	var missing []string
	for _, field := range descriptor.SecretFields() {
		if !present[field] {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return invalid("secret %q does not carry the keys the %s contract needs: %s",
			req.ExistingSecret, descriptor.Name, strings.Join(missing, ", "))
	}
	return nil
}

// splitValues separates the values that go into the Connection spec from the
// credentials, which are stored in a Secret instead.
func splitValues(descriptor *models.ContractDescriptor, values map[string]any) (map[string]any, map[string][]byte) {
	public := map[string]any{}
	secrets := map[string][]byte{}

	for name, value := range values {
		field, known := descriptor.Field(name)
		if !known {
			continue
		}
		if field.Secret {
			if str, ok := value.(string); ok && str != "" {
				secrets[name] = []byte(str)
			}
			continue
		}
		public[name] = value
	}
	return public, secrets
}

func (s *DefaultConnectionService) toResponse(connection *gitops.Connection) models.ConnectionResponse {
	values := map[string]any{}
	for k, v := range connection.Values {
		values[k] = v
	}
	if connection.SecretRef != "" {
		values[valueSecretRef] = connection.SecretRef
	}

	response := models.ConnectionResponse{
		Name:        connection.Name,
		Type:        connection.Contract,
		Scope:       models.ConnectionScopeProject,
		Namespace:   connection.Project,
		Description: connection.Description,
		// Declared in Git: what it points at is only checked by a test.
		Status: "Ready",
		Values: values,
	}
	if descriptor, known := s.catalog.Get(connection.Contract); known {
		response.SecretFields = descriptor.SecretFields()
		if connection.SecretRef != "" && len(response.SecretFields) > 0 {
			response.CredentialsSecret = &models.CredentialsSecretRef{
				Name:      connection.SecretRef,
				Namespace: connection.Project,
				Keys:      response.SecretFields,
				Owned:     connection.SecretRef == connection.Name+credentialsSecretSuffix,
			}
		}
	}
	return response
}

func (s *DefaultConnectionService) ListSelectable(ctx context.Context, project, contract string) ([]models.SelectableConnection, error) {
	result := []models.SelectableConnection{}
	if s.available() {
		connections, err := s.deployments.ListConnections(ctx, project)
		if err != nil {
			return nil, err
		}
		for _, c := range connections {
			if contract != "" && c.Contract != contract {
				continue
			}
			result = append(result, models.SelectableConnection{
				Name:        c.Name,
				Scope:       models.ConnectionScopeProject,
				Type:        c.Contract,
				Status:      "Ready",
				Description: c.Description,
			})
		}
	}

	descriptors, err := s.outputsOf(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, d := range descriptors {
		for _, output := range d.Outputs {
			if contract != "" && output.Contract != contract {
				continue
			}
			if !s.internalCapable(output.Contract) {
				continue
			}
			result = append(result, models.SelectableConnection{
				Name:       output.Name,
				Scope:      models.ConnectionScopeProject,
				Type:       output.Contract,
				Status:     "Ready",
				Managed:    true,
				ProvidedBy: d.Release,
			})
		}
	}

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// endpointFrom returns the address a reader can use to reach a connection.
//
// Which value carries it depends on the contract: database-server publishes a
// host and a port, trino a URI, hive a thrift URI, s3 a URL. Reading host+port
// and nothing else, as this did, meant a Ready trino, hive or s3 connection
// showed "not available yet" while its address sat in its own values, two
// clicks away in the details panel. The descriptor now names the keys that
// carry it, in order of preference.
func endpointFrom(values map[string]any, catalog ContractCatalog, contractName string) string {
	if descriptor, known := catalog.Get(contractName); known {
		for _, key := range descriptor.EndpointFrom {
			if value, ok := values[key].(string); ok && value != "" {
				return value
			}
		}
	}
	// The host+port convention, kept as a fallback so a contract shaped like
	// database-server needs no declaration to be addressable.
	host, ok := values["host"].(string)
	if !ok || host == "" {
		return ""
	}
	if port, ok := toFloat(values["port"]); ok {
		return fmt.Sprintf("%s:%d", host, int32(port))
	}
	return host
}

// ListConsumers returns the services bound to a connection: the instances
// whose declaration layers it in (external) or whose parameters name it
// (internal).
func (s *DefaultConnectionService) ListConsumers(ctx context.Context, project, name string) ([]models.ConnectionConsumer, error) {
	consumers := make([]models.ConnectionConsumer, 0)
	if s.instances == nil {
		return consumers, nil
	}
	instances, err := s.instances(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, instance := range instances {
		for _, bound := range instance.Connections {
			if bound.Name != name {
				continue
			}
			consumers = append(consumers, models.ConnectionConsumer{
				Service:     instance.Name,
				ReleaseName: instance.ReleaseName,
				Status:      instance.Status,
				Effective:   bound.Resolved,
			})
			break
		}
	}
	sort.Slice(consumers, func(i, j int) bool { return consumers[i].ReleaseName < consumers[j].ReleaseName })
	return consumers, nil
}
