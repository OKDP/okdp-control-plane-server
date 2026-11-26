package kubocd

import (
	"context"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// The platform settings are read from the platform Context.

func (a *Adapter) IngressSuffix(ctx context.Context) (string, error) {
	return a.contextRepo.GetIngressSuffix(ctx)
}

func (a *Adapter) ProfileImages(ctx context.Context) (map[string][]models.ProfileImage, error) {
	return a.contextRepo.GetProfileImages(ctx)
}

func (a *Adapter) SparkConfig(ctx context.Context) (*models.SparkConfig, error) {
	return a.contextRepo.GetSparkConfig(ctx)
}
