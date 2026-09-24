package middleware

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

// Path parameters naming a file or directory of the deployments repository.
// Their value is joined into a path: "..", "." or "a/b" would address another
// directory (DELETE …/services/.. used to remove the whole project).
var gitNameParams = map[string]string{
	"serviceName": "service",
	"connName":    "connection",
}

// Path parameters naming a Kubernetes object of the project namespace. The API
// server accepts DNS subdomains for these kinds, so an object created by hand
// with a dot in its name stays reachable; "." and ".." are still refused.
var objectNameParams = map[string]string{
	"storeName": "secret store",
	"esName":    "external secret",
	"appName":   "Spark application",
	"podName":   "pod",
}

// ValidatePathNames answers 400 when a path parameter naming a Git or
// Kubernetes object is not a valid name, before any handler or store sees it.
// The project (:name) is left to RequireProject, which answers 404 for a name
// that cannot be a project; :name under /v1/identity is a Keycloak user or
// group, whose names are not DNS names.
func ValidatePathNames() gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, p := range c.Params {
			if kind, ok := gitNameParams[p.Key]; ok {
				if err := gitops.ValidateName(kind, p.Value); err != nil {
					c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
			}
			if kind, ok := objectNameParams[p.Key]; ok {
				if msgs := validation.IsDNS1123Subdomain(p.Value); len(msgs) > 0 {
					c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
						"error": fmt.Sprintf("invalid %s name %q: it must be a lowercase DNS subdomain", kind, p.Value),
					})
					return
				}
			}
		}
		c.Next()
	}
}
