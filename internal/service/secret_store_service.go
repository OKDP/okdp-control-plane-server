package service

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

type SecretStoreService interface {
	// Available reports whether the external-secrets CRDs are installed.
	Available(ctx context.Context) bool

	ListSecretStores(ctx context.Context, namespace string) ([]models.SecretStoreResponse, error)
	CreateSecretStore(ctx context.Context, namespace string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error)
	UpdateSecretStore(ctx context.Context, namespace, name string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error)
	DeleteSecretStore(ctx context.Context, namespace, name string) error
	TestConnection(ctx context.Context, req models.SecretStoreRequest) error
	GetSecretStoreStatus(ctx context.Context, namespace, name string) (*models.SecretStoreStatusResponse, error)
}

type DefaultSecretStoreService struct {
	repo repository.SecretStoreRepository
}

// DeferredSecretStores is implemented by a SecretStoreRepository whose
// writes reach the cluster later (git persistence). Its Create and Update
// move the default label in the same write, CheckUpdate tells whether a
// store can be updated before its credentials are touched, and a
// credentials Secret is handed over (CredentialsReleaser) instead of being
// deleted, once the write that drops it is committed.
type DeferredSecretStores interface {
	CredentialsReleaser
	CheckUpdate(ctx context.Context, namespace, name string) error
}

func (s *DefaultSecretStoreService) deferred() (DeferredSecretStores, bool) {
	d, ok := s.repo.(DeferredSecretStores)
	return d, ok
}

// clearDefault drops the default label from the other stores, unless the
// repository moves it in the write itself.
func (s *DefaultSecretStoreService) clearDefault(ctx context.Context, namespace string) {
	if _, ok := s.deferred(); ok {
		return
	}
	if err := s.repo.RemoveDefaultLabel(ctx, namespace); err != nil {
		logrus.WithError(err).Warn("Failed to clear previous default store label")
	}
}

// releaseCredentials hands a credentials Secret over to a deferred
// repository. Called only once the write that drops it succeeded.
func (s *DefaultSecretStoreService) releaseCredentials(ctx context.Context, namespace, name string) {
	d, _ := s.deferred()
	if err := d.ReleaseCredentials(ctx, namespace, name); err != nil {
		logrus.WithError(err).Warnf("Failed to release credentials secret %s", name)
	}
}

// mayHaveLanded reports whether a failed write may still be committed.
func mayHaveLanded(err error) bool {
	return errors.Is(err, engine.ErrOutcomeUnknown) || errors.Is(err, gitstore.ErrUncertain)
}

const (
	rotatedTokenInfix  = "-credentials-rot-"
	rotatedTokenRandom = 5
)

// rotatedTokenSecret names a fresh Secret for a new token of store,
// <store>-credentials-rot-<random>: no store reads it yet, whatever git or
// the cluster hold, so writing it never changes a live token.
func rotatedTokenSecret(store string) (string, error) {
	random := make([]byte, rotatedTokenRandom)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return rotatedTokenPrefix(store) + hex.EncodeToString(random), nil
}

// rotatedTokenPrefix is the name of the rotated Secrets of store without
// their random part, with store shortened so that the whole name stays a
// valid Secret name (a DNS-1123 subdomain of 253 characters at most).
func rotatedTokenPrefix(store string) string {
	if max := validation.DNS1123SubdomainMaxLength - len(rotatedTokenInfix) - 2*rotatedTokenRandom; len(store) > max {
		store = strings.TrimRight(store[:max], "-.")
	}
	return store + rotatedTokenInfix
}

// isRotatedTokenSecret reports whether ref names a Secret rotatedTokenSecret
// made for store.
func isRotatedTokenSecret(store string, ref *crd.ESOTokenSecretRef) bool {
	if ref == nil {
		return false
	}
	random, found := strings.CutPrefix(ref.Name, rotatedTokenPrefix(store))
	if !found || len(random) != 2*rotatedTokenRandom {
		return false
	}
	_, err := hex.DecodeString(random)
	return err == nil
}

// tokenSecretOf returns the Secret a store reads its Vault token from, empty
// when it does not use token auth.
func tokenSecretOf(store *crd.ESOSecretStore) *crd.ESOTokenSecretRef {
	if v := store.Spec.Provider.Vault; v != nil && v.Auth.TokenSecretRef != nil {
		return v.Auth.TokenSecretRef
	}
	return nil
}

func (s *DefaultSecretStoreService) Available(ctx context.Context) bool {
	return s.repo.Available(ctx)
}

func NewDefaultSecretStoreService(repo repository.SecretStoreRepository) *DefaultSecretStoreService {
	return &DefaultSecretStoreService{repo: repo}
}

func (s *DefaultSecretStoreService) ListSecretStores(ctx context.Context, namespace string) ([]models.SecretStoreResponse, error) {
	stores, err := s.repo.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var result []models.SecretStoreResponse
	for i := range stores {
		result = append(result, s.toResponse(&stores[i], namespace))
	}
	return result, nil
}

func (s *DefaultSecretStoreService) CreateSecretStore(ctx context.Context, namespace string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}

	credSecretName := req.Name + "-credentials"

	if req.Auth.Type == "token" {
		// validateRequest lets the token be empty because an update reads that
		// as "keep the stored one". On a create there is nothing to keep: an
		// empty token writes an empty Secret and the store can never
		// authenticate, while the API answers 201.
		if req.Auth.Config.Token == "" {
			return nil, invalid("auth.config.token is required for token auth")
		}
	}

	// Checked before the credentials are written: a duplicate must not
	// overwrite the token of the store already there.
	if _, err := s.repo.Get(ctx, namespace, req.Name); err == nil {
		return nil, apierrors.NewAlreadyExists(crd.GetSecretStoreGVR().GroupResource(), req.Name)
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}

	if req.Auth.Type == "token" {
		secretData := map[string][]byte{"token": []byte(req.Auth.Config.Token)}
		if err := s.repo.CreateOrUpdateSecret(ctx, namespace, credSecretName, secretData); err != nil {
			return nil, fmt.Errorf("failed to create credentials secret: %w", err)
		}
	}

	if req.IsDefault {
		s.clearDefault(ctx, namespace)
	}

	store := buildSecretStoreCRD(namespace, req, credSecretName)

	if err := s.repo.Create(ctx, namespace, store); err != nil {
		// No store references the token if the commit surely failed (and did
		// not collide with a store created concurrently under the same name).
		if _, ok := s.deferred(); ok && req.Auth.Type == "token" && !apierrors.IsAlreadyExists(err) && !mayHaveLanded(err) {
			s.releaseCredentials(ctx, namespace, credSecretName)
		}
		return nil, err
	}

	created, err := s.repo.Get(ctx, namespace, req.Name)
	if err != nil {
		return nil, err
	}

	resp := s.toResponse(created, namespace)
	return &resp, nil
}

func (s *DefaultSecretStoreService) UpdateSecretStore(ctx context.Context, namespace, name string, req models.SecretStoreRequest) (*models.SecretStoreResponse, error) {
	existing, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	req.Name = name
	if err := validateRequest(req); err != nil {
		return nil, err
	}

	deferred, isDeferred := s.deferred()
	if isDeferred {
		if err := deferred.CheckUpdate(ctx, namespace, name); err != nil {
			return nil, err
		}
	}

	credSecretName := name + "-credentials"

	// What the store authenticates with today. An omitted token can only mean
	// "keep the stored one" when there is a stored one.
	existingVault := existing.Spec.Provider.Vault
	usedTokenAuth := existingVault != nil && existingVault.Auth.TokenSecretRef != nil
	previousRef := tokenSecretOf(existing)
	// A deferred store reads its token from the cluster before the commit
	// lands: a new token goes to another Secret, so that a commit that fails
	// leaves the live one untouched.
	if isDeferred && req.Auth.Type == "token" && req.Auth.Config.Token != "" {
		if credSecretName, err = rotatedTokenSecret(name); err != nil {
			return nil, err
		}
	}
	wroteToken := false

	switch req.Auth.Type {
	case "token":
		if req.Auth.Config.Token != "" {
			secretData := map[string][]byte{"token": []byte(req.Auth.Config.Token)}
			if err := s.repo.CreateOrUpdateSecret(ctx, namespace, credSecretName, secretData); err != nil {
				return nil, fmt.Errorf("failed to update credentials secret: %w", err)
			}
			wroteToken = true
		} else if !usedTokenAuth {
			// The CR would reference a Secret that was never written, so the
			// store could never sync while the API reported success.
			return nil, invalid("auth.config.token is required when switching to token auth")
		}
	case "kubernetes":
		// The CR is rebuilt from the request, so a caller that omits the
		// account would silently repoint the store at default and break the
		// Vault role binding. Absent means "leave it", like the token above,
		// while an empty string is an explicit ask for the default account.
		if req.Auth.Config.ServiceAccount == nil && existingVault != nil && existingVault.Auth.Kubernetes != nil {
			if ref := existingVault.Auth.Kubernetes.ServiceAccountRef; ref != nil {
				current := ref.Name
				req.Auth.Config.ServiceAccount = &current
			}
		}
	}

	if req.IsDefault {
		s.clearDefault(ctx, namespace)
	}

	store := buildSecretStoreCRD(namespace, req, credSecretName)
	store.ResourceVersion = existing.ResourceVersion
	// A token kept as it is stays where the store reads it today, which a
	// manifest written by hand may name differently.
	if req.Auth.Type == "token" && req.Auth.Config.Token == "" && previousRef != nil {
		kept := *previousRef
		store.Spec.Provider.Vault.Auth.TokenSecretRef = &kept
	}

	if err := s.repo.Update(ctx, namespace, store); err != nil {
		if isDeferred && wroteToken && !mayHaveLanded(err) && (previousRef == nil || previousRef.Name != credSecretName) {
			s.releaseCredentials(ctx, namespace, credSecretName)
		}
		return nil, err
	}

	// Dropped only once the stored CR no longer points at it, so a failed
	// update never destroys a credential the store still uses. Left behind, the
	// Vault token would outlive the store's use of it: still valid in Vault,
	// no longer shown anywhere in the console.
	if isDeferred {
		if newRef := tokenSecretOf(store); previousRef != nil && (newRef == nil || newRef.Name != previousRef.Name) {
			s.releaseCredentials(ctx, namespace, previousRef.Name)
		}
	} else {
		if usedTokenAuth && req.Auth.Type != "token" {
			if err := s.repo.DeleteSecret(ctx, namespace, credSecretName); err != nil && !apierrors.IsNotFound(err) {
				logrus.WithError(err).Warnf("Failed to delete unused credentials secret %s", credSecretName)
			}
		}
		// A token rotated while the store was in git: the store no longer
		// reads it.
		if newRef := tokenSecretOf(store); isRotatedTokenSecret(name, previousRef) && (newRef == nil || newRef.Name != previousRef.Name) {
			s.deleteToken(ctx, namespace, previousRef.Name)
		}
	}

	updated, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	resp := s.toResponse(updated, namespace)
	return &resp, nil
}

func (s *DefaultSecretStoreService) DeleteSecretStore(ctx context.Context, namespace, name string) error {
	existing, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return err
	}

	// The deletion reaches the cluster later: the Secret is handed over once
	// the deletion is committed, never on a failed or uncertain one.
	if _, ok := s.deferred(); ok {
		if err := s.repo.Delete(ctx, namespace, name); err != nil {
			return err
		}
		if ref := tokenSecretOf(existing); ref != nil {
			s.releaseCredentials(ctx, namespace, ref.Name)
		}
		return nil
	}

	credSecretName := name + "-credentials"
	s.deleteToken(ctx, namespace, credSecretName)
	if err := s.repo.Delete(ctx, namespace, name); err != nil {
		return err
	}
	// A token rotated while the store was in git, dropped once the store is
	// gone.
	if ref := tokenSecretOf(existing); isRotatedTokenSecret(name, ref) {
		s.deleteToken(ctx, namespace, ref.Name)
	}
	return nil
}

func (s *DefaultSecretStoreService) deleteToken(ctx context.Context, namespace, name string) {
	if err := s.repo.DeleteSecret(ctx, namespace, name); err != nil && !apierrors.IsNotFound(err) {
		logrus.WithError(err).Warnf("Failed to delete credentials secret %s", name)
	}
}

func (s *DefaultSecretStoreService) TestConnection(ctx context.Context, req models.SecretStoreRequest) error {
	if req.Vault == nil {
		return fmt.Errorf("vault configuration is required")
	}
	if req.Vault.Server == "" {
		return fmt.Errorf("vault.server is required")
	}
	if err := validateVaultCABundle(req.Vault.CABundle); err != nil {
		return err
	}
	if req.Auth == nil {
		return fmt.Errorf("auth configuration is required")
	}

	if req.Auth.Type == "kubernetes" {
		return nil
	}

	if req.Auth.Config.Token == "" {
		return fmt.Errorf("auth.config.token is required for token auth")
	}

	return validateVaultToken(ctx, req.Vault.Server, req.Auth.Config.Token, req.Vault.CABundle)
}

// validateVaultToken calls GET /v1/auth/token/lookup-self to verify that
// the token is valid. sys/health only checks network connectivity -- a bad
// token still gets "Ready" from ESO.
//
// The method matters: Vault's default policy grants "read" on this path, which
// maps to GET. A POST needs the "update" capability, which no least-privilege
// token carries, so only a root token would have passed.
func validateVaultToken(ctx context.Context, server, token, caBundle string) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if caBundle != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caBundle)) {
			return fmt.Errorf("invalid CA bundle")
		}
		tlsConfig.RootCAs = pool
	}

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}

	url := server + "/v1/auth/token/lookup-self"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to build request: %w", err)
	}
	httpReq.Header.Set("X-Vault-Token", token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return fmt.Errorf("invalid token: permission denied")
	default:
		return fmt.Errorf("vault returned status %d", resp.StatusCode)
	}
}

func (s *DefaultSecretStoreService) GetSecretStoreStatus(ctx context.Context, namespace, name string) (*models.SecretStoreStatusResponse, error) {
	store, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	conditions := mapConditions(store.Status.Conditions)
	status := deriveOverallStatus(store.Status.Conditions)

	var lastCheckedAt *string
	var lastError *string

	if len(store.Status.Conditions) > 0 {
		last := store.Status.Conditions[len(store.Status.Conditions)-1]
		if last.LastTransitionTime != "" {
			lastCheckedAt = &last.LastTransitionTime
		}
		if last.Status != "True" && last.Message != "" {
			lastError = &last.Message
		}
	}

	return &models.SecretStoreStatusResponse{
		Status:        status,
		Conditions:    conditions,
		LastCheckedAt: lastCheckedAt,
		LastError:     lastError,
	}, nil
}

// --- helpers ---

func validateRequest(req models.SecretStoreRequest) error {
	if req.Name == "" {
		return invalid("name is required")
	}
	if req.Provider != "vault" {
		return invalid("unsupported provider %q, only 'vault' is supported", req.Provider)
	}
	if req.Vault == nil {
		return invalid("vault configuration is required")
	}
	if req.Vault.Server == "" {
		return invalid("vault.server is required")
	}
	if hasEmbeddedURLCredentials(req.Vault.Server) {
		return invalid("vault.server must not contain embedded credentials")
	}
	if err := validateVaultCABundle(req.Vault.CABundle); err != nil {
		return err
	}
	if req.Vault.Path == "" {
		return invalid("vault.path is required")
	}
	if req.Vault.Version != "v1" && req.Vault.Version != "v2" {
		return invalid("vault.version must be 'v1' or 'v2'")
	}
	if req.Auth == nil {
		return invalid("auth configuration is required")
	}
	switch req.Auth.Type {
	case "token":
		// Token can be empty on update (preserves existing)
	case "kubernetes":
		if req.Auth.Config.Role == "" {
			return invalid("auth.config.role is required for kubernetes auth")
		}
		// Left to the API server this came back as a schema rejection the
		// handler could not tell from a platform failure, so a mistyped
		// account answered 500.
		if sa := req.Auth.Config.ServiceAccount; sa != nil && *sa != "" {
			if msgs := validation.IsDNS1123Subdomain(*sa); len(msgs) > 0 {
				return invalid("auth.config.serviceAccount %q is not a valid ServiceAccount name: %s", *sa, msgs[0])
			}
		}
	default:
		return invalid("unsupported auth type %q, must be 'token' or 'kubernetes'", req.Auth.Type)
	}
	return nil
}

// validateVaultCABundle refuses private key material inside a public CA bundle
// that will be persisted in Kubernetes CRs or Git.
func validateVaultCABundle(caBundle string) error {
	if strings.TrimSpace(caBundle) == "" {
		return nil
	}
	rest := []byte(caBundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if strings.Contains(strings.ToUpper(block.Type), "PRIVATE KEY") {
			return invalid("vault.caBundle must not contain private keys")
		}
	}
	if strings.Contains(strings.ToUpper(caBundle), "PRIVATE KEY") {
		return invalid("vault.caBundle must not contain private keys")
	}
	return nil
}

func buildSecretStoreCRD(namespace string, req models.SecretStoreRequest, credSecretName string) *crd.ESOSecretStore {
	store := &crd.ESOSecretStore{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crd.SecretStoreAPIVersion,
			Kind:       crd.SecretStoreKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: crd.ESOSecretStoreSpec{
			Provider: crd.ESOProvider{
				Vault: &crd.ESOVaultProvider{
					Server:   req.Vault.Server,
					Path:     req.Vault.Path,
					Version:  req.Vault.Version,
					CABundle: []byte(req.Vault.CABundle),
				},
			},
		},
	}

	switch req.Auth.Type {
	case "token":
		store.Spec.Provider.Vault.Auth.TokenSecretRef = &crd.ESOTokenSecretRef{
			Name: credSecretName,
			Key:  "token",
		}
	case "kubernetes":
		mountPath := req.Auth.Config.MountPath
		if mountPath == "" {
			mountPath = "kubernetes"
		}
		// Vault matches the bound_service_account_names of the role against this
		// account, so pinning it to default forced every role onto the identity
		// the whole namespace already shares.
		serviceAccount := "default"
		if req.Auth.Config.ServiceAccount != nil && *req.Auth.Config.ServiceAccount != "" {
			serviceAccount = *req.Auth.Config.ServiceAccount
		}
		store.Spec.Provider.Vault.Auth.Kubernetes = &crd.ESOKubernetesAuth{
			MountPath: mountPath,
			Role:      req.Auth.Config.Role,
			ServiceAccountRef: &crd.ESOServiceAccountRef{
				Name: serviceAccount,
			},
		}
	}

	if req.IsDefault {
		store.Labels = map[string]string{crd.LabelDefaultStore: "true"}
	}

	return store
}

func (s *DefaultSecretStoreService) toResponse(store *crd.ESOSecretStore, namespace string) models.SecretStoreResponse {
	resp := models.SecretStoreResponse{
		Name:      store.Name,
		Provider:  store.Spec.Provider.Name,
		Namespace: namespace,
		Status:    deriveOverallStatus(store.Status.Conditions),
		IsDefault: store.Labels[crd.LabelDefaultStore] == "true",
	}
	if !store.CreationTimestamp.IsZero() {
		resp.CreatedAt = store.CreationTimestamp.Format(time.RFC3339)
	}

	if len(store.Status.Conditions) > 0 {
		last := store.Status.Conditions[len(store.Status.Conditions)-1]
		if last.LastTransitionTime != "" {
			resp.LastCheckedAt = &last.LastTransitionTime
		}
		if last.Status != "True" && last.Message != "" {
			resp.LastError = &last.Message
		}
	}

	if store.Spec.Provider.Vault != nil {
		v := store.Spec.Provider.Vault
		resp.Vault = &models.VaultConfig{
			Server:   v.Server,
			Path:     v.Path,
			Version:  v.Version,
			CABundle: string(v.CABundle),
		}

		authResp := &models.SecretStoreAuthResponse{
			Config: models.SecretAuthConfigSafe{},
		}
		if v.Auth.TokenSecretRef != nil {
			authResp.Type = "token"
		} else if v.Auth.Kubernetes != nil {
			authResp.Type = "kubernetes"
			mp := v.Auth.Kubernetes.MountPath
			authResp.Config.MountPath = &mp
			role := v.Auth.Kubernetes.Role
			authResp.Config.Role = &role
			// Read back the account the store authenticates as, otherwise a
			// caller cannot tell which identity the Vault role must bind.
			if ref := v.Auth.Kubernetes.ServiceAccountRef; ref != nil {
				sa := ref.Name
				authResp.Config.ServiceAccount = &sa
			}
		}
		resp.Auth = authResp
	}

	return resp
}

func mapConditions(conditions []crd.ESOCondition) []models.SecretStoreCondition {
	var out []models.SecretStoreCondition
	for _, c := range conditions {
		out = append(out, models.SecretStoreCondition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime,
		})
	}
	return out
}

func deriveOverallStatus(conditions []crd.ESOCondition) string {
	if len(conditions) == 0 {
		return "Unknown"
	}
	for _, c := range conditions {
		if c.Type == "Ready" {
			switch c.Status {
			case "True":
				return "Ready"
			case "False":
				return "Error"
			default:
				return "Pending"
			}
		}
	}
	return "Pending"
}
