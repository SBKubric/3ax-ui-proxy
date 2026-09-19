package controller

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/gin-gonic/gin"
)

// MonitoringController serves the mon-server contract under
// <webBasePath>mon/v1 (docs/spec/monitoring-contract.md; panel side in
// docs/spec/monitoring-panel.md §4.1–4.2). It is a machine API: bearer token
// instead of a session, plain JSON bodies instead of the panel's
// {success,msg,obj} envelope, and a bare 404 for anyone without the token so
// the panel does not reveal it is there.
type MonitoringController struct {
	settingService    service.SettingService
	monitoringService service.MonitoringService
}

const (
	monMaxBodyBytes  = 1 << 20 // 1 MiB
	monMaxEvents     = 1000
	monMaxStats      = 2000
	monMaxMonClients = 200
)

// NewMonitoringController registers the contract routes on g.
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

// checkMonAuth admits only a request carrying the panel's monToken while
// monitoring is enabled, and answers everything else with a bare 404, as
// checkAPIAuth does for the panel API. A passing request stamps
// monLastContact.
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
	presented, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(token)) != 1 {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	a.monitoringService.TouchMonLastContact(time.Now())
	c.Header("X-Mon-Contract", "1")
	c.Next()
}

// monError is the contract's error body (§3).
type monError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (a *MonitoringController) fail(c *gin.Context, err error) {
	var me *service.MonError
	if errors.As(err, &me) {
		c.AbortWithStatusJSON(me.Status, monError{me.Code, me.Message})
		return
	}
	logger.Warning("monitoring:", c.Request.Method, c.Request.URL.Path, err)
	c.AbortWithStatusJSON(http.StatusInternalServerError, monError{"internal", err.Error()})
}

// readBody decodes a JSON body of at most 1 MiB into dst. A body over the
// limit is 413 batch_too_large, anything undecodable 400 invalid_body.
func (a *MonitoringController) readBody(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, monMaxBodyBytes)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, monError{"batch_too_large", "body larger than 1 MiB"})
			return false
		}
		c.AbortWithStatusJSON(http.StatusBadRequest, monError{"invalid_body", err.Error()})
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		c.AbortWithStatusJSON(http.StatusBadRequest, monError{"invalid_body", "trailing data after the JSON body"})
		return false
	}
	return true
}

func (a *MonitoringController) tooLarge(c *gin.Context, what string, n, limit int) bool {
	if n <= limit {
		return false
	}
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, monError{"batch_too_large",
		what + " has " + itoa(n) + " elements, limit " + itoa(limit)})
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// GET /state
func (a *MonitoringController) state(c *gin.Context) {
	st, err := a.monitoringService.State()
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// POST /probe/ensure
func (a *MonitoringController) probeEnsure(c *gin.Context) {
	var body struct {
		MonClients []service.MonClient `json:"monClients"`
	}
	if !a.readBody(c, &body) {
		return
	}
	if a.tooLarge(c, "monClients", len(body.MonClients), monMaxMonClients) {
		return
	}
	for i, mc := range body.MonClients {
		if strings.TrimSpace(mc.Id) == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, monError{"invalid_body", "monClients[" + itoa(i) + "].id: required"})
			return
		}
	}
	res, err := a.monitoringService.EnsureProbeSet(body.MonClients)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// GET /probe/configs[?host=]
func (a *MonitoringController) probeConfigs(c *gin.Context) {
	res, err := a.monitoringService.ProbeConfigs(c.Query("host"))
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// DELETE /probe
func (a *MonitoringController) probeDelete(c *gin.Context) {
	if err := a.monitoringService.DeleteProbeSet(); err != nil {
		a.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// POST /events
func (a *MonitoringController) events(c *gin.Context) {
	var body struct {
		Events []service.MonEventIn `json:"events"`
	}
	if !a.readBody(c, &body) {
		return
	}
	if a.tooLarge(c, "events", len(body.Events), monMaxEvents) {
		return
	}
	res, err := a.monitoringService.ApplyEvents(body.Events)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// POST /stats
func (a *MonitoringController) stats(c *gin.Context) {
	var body struct {
		Stats []service.MonStatIn `json:"stats"`
	}
	if !a.readBody(c, &body) {
		return
	}
	if a.tooLarge(c, "stats", len(body.Stats), monMaxStats) {
		return
	}
	res, err := a.monitoringService.UpsertStats(body.Stats)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}
