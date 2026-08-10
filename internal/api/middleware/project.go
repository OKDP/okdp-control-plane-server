package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/okdp/okdp-server-new/internal/models"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RequireProject checks that the :name segment of the path really is a project,
// that is a Namespace carrying the okdp.io/project label.
//
// Without it, every route under /api/projects/:name passes the raw path
// segment to the Kubernetes client, with the cluster-wide rights of the
// ServiceAccount. GET /api/projects/kube-system/services answers about
// kube-system, and POST /api/projects/okdp-system/connections writes into the
// platform namespace. No per-project authorization can decide anything as long
// as the value it would inspect is not guaranteed to be a project.
//
// Resolution happens once per request and the result is put in the gin context
// under ProjectKey, so a handler needing the description does not fetch it again.
func RequireProject(get func(c *gin.Context, name string) (*models.Project, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		name := c.Param("name")
		if name == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "a project name is required"})
			return
		}

		project, err := get(c, name)
		switch {
		case err != nil && !apierrors.IsNotFound(err):
			// A cluster that cannot answer is not a project that does not exist:
			// answering 404 on a timeout tells the console the project was
			// deleted, and loses the cause on the way.
			_ = c.Error(err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "could not resolve project '" + name + "'"})
			return
		case err != nil || project == nil:
			// Deliberately the same answer whether the namespace is absent or
			// simply not a project: the caller learns nothing about what exists
			// outside the projects.
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "project '" + name + "' not found"})
			return
		}

		c.Set(ProjectKey, project)
		c.Next()
	}
}

// ProjectKey is where RequireProject stores the resolved project.
const ProjectKey = "okdp.project"
