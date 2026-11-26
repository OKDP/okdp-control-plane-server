package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/engine"
	"github.com/okdp/okdp-control-plane-server/internal/gitstore"
	"github.com/okdp/okdp-control-plane-server/internal/models"
)

const (
	codeStorageConflict    = "conflict"
	codeStorageUnavailable = "service-unavailable"
	codeStorageRejected    = "upstream-rejected"
	codeStorageUnknown     = "outcome-unknown"
)

// storageStatus classifies a storage error into an HTTP status and machine
// code:
//   - 504 when a write's outcome is unknown (it may still land)
//   - 503 when the backing store is unreachable, busy, or waiting for the
//     project namespace to be provisioned in the cluster
//   - 502 when the remote Git server refused a push (hook/policy/authz)
//   - 409 on optimistic concurrency conflict or duplicate resource
func storageStatus(err error) (status int, code string, ok bool) {
	switch {
	case errors.Is(err, engine.ErrOutcomeUnknown), errors.Is(err, gitstore.ErrUncertain):
		return http.StatusGatewayTimeout, codeStorageUnknown, true
	case errors.Is(err, engine.ErrNamespaceProvisioning), errors.Is(err, gitstore.ErrUnavailable), errors.Is(err, gitstore.ErrBusy):
		return http.StatusServiceUnavailable, codeStorageUnavailable, true
	case errors.Is(err, gitstore.ErrRejected):
		return http.StatusBadGateway, codeStorageRejected, true
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return http.StatusConflict, codeStorageConflict, true
	}
	return 0, "", false
}

// respondStorageError answers err with its storageStatus using
// models.ErrorResponse (compatible with {"error": ...}) and reports whether it
// handled the error.
func respondStorageError(c *gin.Context, err error) bool {
	status, code, ok := storageStatus(err)
	if !ok {
		return false
	}
	c.JSON(status, models.ErrorResponse{Error: err.Error(), Code: code})
	return true
}
