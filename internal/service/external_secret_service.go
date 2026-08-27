package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/repository"
	"github.com/okdp/okdp-control-plane-server/internal/repository/crd"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ExternalSecretService interface {
	// Available reports whether the external-secrets CRDs are installed.
	Available(ctx context.Context) bool

	ListExternalSecrets(ctx context.Context, namespace string) ([]models.ExternalSecretResponse, error)
	CreateExternalSecret(ctx context.Context, namespace string, req models.ExternalSecretRequest) (*models.ExternalSecretResponse, error)
	UpdateExternalSecret(ctx context.Context, namespace, name string, req models.ExternalSecretRequest) (*models.ExternalSecretResponse, error)
	DeleteExternalSecret(ctx context.Context, namespace, name string) error
	GetExternalSecretStatus(ctx context.Context, namespace, name string) (*models.ExternalSecretStatusResponse, error)
	CheckRemoteRef(ctx context.Context, namespace string, req models.ExternalSecretCheckRequest) (*models.ExternalSecretCheckResponse, error)
}

type DefaultExternalSecretService struct {
	repo            repository.ExternalSecretRepository
	secretStoreRepo repository.SecretStoreRepository
}

func (s *DefaultExternalSecretService) Available(ctx context.Context) bool {
	return s.repo.Available(ctx)
}

func NewDefaultExternalSecretService(repo repository.ExternalSecretRepository, secretStoreRepo repository.SecretStoreRepository) *DefaultExternalSecretService {
	return &DefaultExternalSecretService{repo: repo, secretStoreRepo: secretStoreRepo}
}

func (s *DefaultExternalSecretService) ListExternalSecrets(ctx context.Context, namespace string) ([]models.ExternalSecretResponse, error) {
	items, err := s.repo.List(ctx, namespace)
	if err != nil {
		return nil, err
	}

	var result []models.ExternalSecretResponse
	for i := range items {
		result = append(result, s.toResponse(&items[i], namespace))
	}
	return result, nil
}

func (s *DefaultExternalSecretService) CreateExternalSecret(ctx context.Context, namespace string, req models.ExternalSecretRequest) (*models.ExternalSecretResponse, error) {
	if err := validateExternalSecretRequest(req); err != nil {
		return nil, err
	}

	// Validate that the referenced SecretStore exists in the namespace
	_, err := s.secretStoreRepo.Get(ctx, namespace, req.SecretStoreRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("SecretStore '%s' not found in project '%s'", req.SecretStoreRef, namespace)
		}
		return nil, fmt.Errorf("failed to verify SecretStore: %w", err)
	}

	esCRD := buildExternalSecretCRD(namespace, req)

	if err := s.repo.Create(ctx, namespace, esCRD); err != nil {
		return nil, err
	}

	created, err := s.repo.Get(ctx, namespace, req.Name)
	if err != nil {
		return nil, err
	}

	resp := s.toResponse(created, namespace)
	return &resp, nil
}

func (s *DefaultExternalSecretService) UpdateExternalSecret(ctx context.Context, namespace, name string, req models.ExternalSecretRequest) (*models.ExternalSecretResponse, error) {
	existing, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	req.Name = name
	if err := validateExternalSecretRequest(req); err != nil {
		return nil, err
	}

	// Validate that the referenced SecretStore exists in the namespace
	_, err = s.secretStoreRepo.Get(ctx, namespace, req.SecretStoreRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("SecretStore '%s' not found in project '%s'", req.SecretStoreRef, namespace)
		}
		return nil, fmt.Errorf("failed to verify SecretStore: %w", err)
	}

	esCRD := buildExternalSecretCRD(namespace, req)
	esCRD.ResourceVersion = existing.ResourceVersion

	if err := s.repo.Update(ctx, namespace, esCRD); err != nil {
		return nil, err
	}

	updated, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	resp := s.toResponse(updated, namespace)
	return &resp, nil
}

func (s *DefaultExternalSecretService) DeleteExternalSecret(ctx context.Context, namespace, name string) error {
	_, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return err
	}

	return s.repo.Delete(ctx, namespace, name)
}

func (s *DefaultExternalSecretService) GetExternalSecretStatus(ctx context.Context, namespace, name string) (*models.ExternalSecretStatusResponse, error) {
	es, err := s.repo.Get(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	conditions := mapConditions(es.Status.Conditions)
	status := deriveExternalSecretStatus(es.Status.Conditions)

	var lastSyncedAt *string
	var lastError *string

	if es.Status.RefreshTime != "" {
		lastSyncedAt = &es.Status.RefreshTime
	}

	if len(es.Status.Conditions) > 0 {
		last := es.Status.Conditions[len(es.Status.Conditions)-1]
		if last.Status != "True" && last.Message != "" {
			lastError = &last.Message
		}
	}

	return &models.ExternalSecretStatusResponse{
		Status:       status,
		Conditions:   conditions,
		LastSyncedAt: lastSyncedAt,
		LastError:    lastError,
	}, nil
}

// --- helpers ---

func validateExternalSecretRequest(req models.ExternalSecretRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if req.SecretStoreRef == "" {
		return fmt.Errorf("secretStoreRef is required")
	}
	if req.Target.Name == "" {
		return fmt.Errorf("target.name is required")
	}
	if req.RefreshInterval == "" {
		return fmt.Errorf("refreshInterval is required")
	}
	if len(req.Data) == 0 {
		return fmt.Errorf("at least one data mapping is required")
	}
	for i, d := range req.Data {
		if d.SecretKey == "" {
			return fmt.Errorf("data[%d].secretKey is required", i)
		}
		if d.RemoteRef.Key == "" {
			return fmt.Errorf("data[%d].remoteRef.key is required", i)
		}
		// The same shape the key check enforces. Without it the two paths
		// disagree on one input: the check refuses "a?b=1" as an invalid path
		// while the create accepts it and stores an import that can never
		// resolve.
		// The same rule the key check applies: only a segment that walks out of
		// the mount is refused. Vault accepts the rest, and a key literally
		// named "avec espace" was measured syncing on the running cluster.
		if hasTraversal(strings.Trim(d.RemoteRef.Key, "/")) {
			return invalid("data[%d].remoteRef.key %q walks out of the store's mount", i, d.RemoteRef.Key)
		}
	}
	return nil
}

func buildExternalSecretCRD(namespace string, req models.ExternalSecretRequest) *crd.ESOExternalSecret {
	var data []crd.ESOExternalSecretData
	for _, d := range req.Data {
		data = append(data, crd.ESOExternalSecretData{
			SecretKey: d.SecretKey,
			RemoteRef: crd.ESORemoteRef{
				Key:      d.RemoteRef.Key,
				Property: d.RemoteRef.Property,
			},
		})
	}

	return &crd.ESOExternalSecret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: crd.ExternalSecretAPIVersion,
			Kind:       crd.ExternalSecretKind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: crd.ESOExternalSecretSpec{
			RefreshInterval: req.RefreshInterval,
			SecretStoreRef: crd.ESOSecretStoreRef{
				Name: req.SecretStoreRef,
				Kind: "SecretStore",
			},
			Target: crd.ESOExternalSecretTarget{
				Name:           req.Target.Name,
				CreationPolicy: "Owner",
			},
			Data: data,
		},
	}
}

func (s *DefaultExternalSecretService) toResponse(es *crd.ESOExternalSecret, namespace string) models.ExternalSecretResponse {
	var data []models.ExternalSecretDataEntry
	for _, d := range es.Spec.Data {
		data = append(data, models.ExternalSecretDataEntry{
			SecretKey: d.SecretKey,
			RemoteRef: models.ExternalSecretRemote{
				Key:      d.RemoteRef.Key,
				Property: d.RemoteRef.Property,
			},
		})
	}

	resp := models.ExternalSecretResponse{
		Name:           es.Name,
		Namespace:      namespace,
		SecretStoreRef: es.Spec.SecretStoreRef.Name,
		Target: models.ExternalSecretTargetResponse{
			Name:           es.Spec.Target.Name,
			CreationPolicy: es.Spec.Target.CreationPolicy,
		},
		RefreshInterval: es.Spec.RefreshInterval,
		Data:            data,
		Status:          deriveExternalSecretStatus(es.Status.Conditions),
		CreatedAt:       es.CreationTimestamp.Format(time.RFC3339),
	}

	if es.Status.RefreshTime != "" {
		resp.LastSyncedAt = &es.Status.RefreshTime
	}

	if len(es.Status.Conditions) > 0 {
		last := es.Status.Conditions[len(es.Status.Conditions)-1]
		if last.Status != "True" && last.Message != "" {
			resp.LastError = &last.Message
		}
	}

	return resp
}

func deriveExternalSecretStatus(conditions []crd.ESOCondition) string {
	if len(conditions) == 0 {
		return "Pending"
	}
	for _, c := range conditions {
		if c.Type == "Ready" {
			switch c.Status {
			case "True":
				return "Synced"
			case "False":
				return "Error"
			default:
				return "Pending"
			}
		}
	}
	return "Unknown"
}
