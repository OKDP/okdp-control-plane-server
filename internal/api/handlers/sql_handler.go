package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/okdp/okdp-control-plane-server/internal/models"
	"github.com/okdp/okdp-control-plane-server/internal/service"
)

type SqlHandler struct {
	service service.SqlService
}

func NewSqlHandler(svc service.SqlService) *SqlHandler {
	return &SqlHandler{service: svc}
}

// ExecuteQuery godoc
// @Summary      Execute a SQL query on a deployed SQL engine
// @Description  Proxies the statement to the service instance (Trino only), forwarding the caller's bearer token. Engine-side SQL errors answer 200 with `error` set.
// @Tags         sql
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        serviceName path string true "Service instance name"
// @Param        request body models.SqlQueryRequest true "SQL Query Request"
// @Success      200  {object}  models.SqlQueryResult
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/projects/{name}/services/{serviceName}/sql [post]
func (h *SqlHandler) ExecuteQuery(c *gin.Context) {
	req, ok := bindSqlRequest(c)
	if !ok {
		return
	}
	result, err := h.service.ExecuteQuery(c.Request.Context(), c.Param("name"), c.Param("serviceName"), c.GetHeader("Authorization"), req)
	respondSql(c, result, err)
}

// ExecuteOnConnection godoc
// @Summary      Execute SQL on an external database connection
// @Description  Runs a statement batch on a database-server connection of the project (engine postgresql) with the connection's credentials, writes included: every project member acts as the connection's database user. Several statements separated by ';' run in one implicit transaction; the result is the last statement that returned columns, with the command tag of the last statement. Rows past maxRows are dropped (not cancelled). SQL errors (errorName = SQLSTATE) and the 2-minute timeout answer 200 with `error` set.
// @Tags         sql
// @Accept       json
// @Produce      json
// @Param        name path string true "Project name"
// @Param        connName path string true "Connection name"
// @Param        request body models.SqlQueryRequest true "SQL Query Request"
// @Success      200  {object}  models.SqlQueryResult
// @Failure      400  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      502  {object}  map[string]string
// @Router       /api/projects/{name}/connections/{connName}/sql [post]
func (h *SqlHandler) ExecuteOnConnection(c *gin.Context) {
	req, ok := bindSqlRequest(c)
	if !ok {
		return
	}
	result, err := h.service.ExecuteOnConnection(c.Request.Context(), c.Param("name"), c.Param("connName"), req)
	respondSql(c, result, err)
}

func bindSqlRequest(c *gin.Context) (models.SqlQueryRequest, bool) {
	var req models.SqlQueryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return req, false
	}
	if strings.TrimSpace(req.Query) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query must not be empty"})
		return req, false
	}
	return req, true
}

func respondSql(c *gin.Context, result *models.SqlQueryResult, err error) {
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		case errors.Is(err, service.ErrSqlUnsupported), errors.Is(err, service.ErrSqlNoURL),
			errors.Is(err, service.ErrSqlNoCredentials), errors.Is(err, service.ErrConnectionsUnavailable):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			logrus.WithError(err).Error("Failed to execute SQL query")
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, result)
}
