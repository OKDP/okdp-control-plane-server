package handlers

import (
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// serverErrorStatus is the status of an error the handler has no specific
// answer for: 403 when Kubernetes refused the caller, 500 otherwise.
func serverErrorStatus(err error) int {
	if apierrors.IsForbidden(err) {
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}
