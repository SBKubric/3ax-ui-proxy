package controller

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// MonitoringController serves the mon-server contract at <basePath>mon/v1
// (docs/spec/monitoring-contract.md). It is not a browser API: no session,
// no {success,msg,obj} envelope — a bearer token, JSON bodies, and
// {error,message} on failure. Unauthorized requests get a bare 404, the way
// /panel/api hides itself.
type MonitoringController struct {
	monitoringService service.MonitoringService
	settingService    service.SettingService
}

// monMaxBody caps a contract request body (contract §3).
const monMaxBody = 1 << 20

// NewMonitoringController mounts the contract under g.
func NewMonitoringController(g *gin.RouterGroup) *MonitoringController {
	a := &MonitoringController{}
	a.initRouter(g)
	return a
}

func (a *MonitoringController) initRouter(g *gin.RouterGroup) {
	mon := g.Group("/mon/v1")
	mon.Use(a.checkMonAuth)
	mon.GET("/state", a.state)
	mon.POST("/probe/ensure", a.probeEnsure)
	mon.GET("/probe/configs", a.probeConfigs)
	mon.DELETE("/probe", a.probeDelete)
	mon.POST("/events", a.events)
	mon.POST("/stats", a.stats)
}

// checkMonAuth is the contract's gate (§2): monitoring enabled, a token
// configured, and Authorization: Bearer <token> matching it in constant
// time; anything else is a bare 404 with no body. An authorized request
// counts as contact and carries the contract header.
func (a *MonitoringController) checkMonAuth(c *gin.Context) {
	enabled, err := a.settingService.GetMonEnable()
	if err != nil || !enabled {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	token, err := a.settingService.GetMonToken()
	if err != nil || token == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	const scheme = "Bearer "
	header := c.GetHeader("Authorization")
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	presented := strings.TrimSpace(header[len(scheme):])
	if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	a.monitoringService.RecordContact(time.Now())
	c.Header("X-Mon-Contract", strconv.Itoa(service.MonContract))
	c.Next()
}

// monFail writes the contract's error body for err: a MonError keeps its
// status and code, anything else is a 500.
func monFail(c *gin.Context, err error) {
	var monErr *service.MonError
	if errors.As(err, &monErr) {
		c.JSON(monErr.Status, gin.H{"error": monErr.Code, "message": monErr.Message})
		return
	}
	logger.Warning("monitoring contract:", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": err.Error()})
}

// readBody decodes a JSON body of at most monMaxBody bytes into v. Unknown
// fields are allowed: a newer mon-server may send more than this panel knows.
func readBody(c *gin.Context, v any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, monMaxBody)
	if err := json.NewDecoder(c.Request.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &service.MonError{Status: http.StatusRequestEntityTooLarge, Code: service.MonErrBatchTooLarge,
				Message: fmt.Sprintf("body exceeds %d bytes", monMaxBody)}
		}
		return &service.MonError{Status: http.StatusBadRequest, Code: service.MonErrInvalidBody, Message: err.Error()}
	}
	return nil
}

func (a *MonitoringController) state(c *gin.Context) {
	state, err := a.monitoringService.State()
	if err != nil {
		monFail(c, err)
		return
	}
	c.JSON(http.StatusOK, state)
}

// monEnsureBody is the registry snapshot of POST /probe/ensure.
type monEnsureBody struct {
	MonClients []service.MonClient `json:"monClients"`
}

func (a *MonitoringController) probeEnsure(c *gin.Context) {
	var body monEnsureBody
	if err := readBody(c, &body); err != nil {
		monFail(c, err)
		return
	}
	if len(body.MonClients) > service.MonMaxMonClients {
		monFail(c, &service.MonError{Status: http.StatusRequestEntityTooLarge, Code: service.MonErrBatchTooLarge,
			Message: fmt.Sprintf("monClients: %d elements, the limit is %d", len(body.MonClients), service.MonMaxMonClients)})
		return
	}
	for i, mc := range body.MonClients {
		if !service.ValidMonClientId(mc.Id) {
			monFail(c, &service.MonError{Status: http.StatusBadRequest, Code: service.MonErrInvalidBody,
				Message: fmt.Sprintf("monClients[%d].id: required, at most 64 of [A-Za-z0-9_.-]", i)})
			return
		}
	}
	result, err := a.monitoringService.EnsureProbeSet(body.MonClients)
	if err != nil {
		monFail(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (a *MonitoringController) probeConfigs(c *gin.Context) {
	configs, err := a.monitoringService.ProbeConfigs(strings.TrimSpace(c.Query("host")))
	if err != nil {
		monFail(c, err)
		return
	}
	c.JSON(http.StatusOK, configs)
}

func (a *MonitoringController) probeDelete(c *gin.Context) {
	if err := a.monitoringService.DeleteProbeSet(); err != nil {
		monFail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (a *MonitoringController) events(c *gin.Context) {
	var batch service.MonEventsBatch
	if err := readBody(c, &batch); err != nil {
		monFail(c, err)
		return
	}
	result, err := a.monitoringService.ApplyEvents(&batch)
	if err != nil {
		monFail(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

func (a *MonitoringController) stats(c *gin.Context) {
	var batch service.MonStatsBatch
	if err := readBody(c, &batch); err != nil {
		monFail(c, err)
		return
	}
	result, err := a.monitoringService.UpsertStats(&batch)
	if err != nil {
		monFail(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}
