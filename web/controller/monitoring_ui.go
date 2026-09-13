package controller

import (
	"errors"
	"strconv"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// MonitoringUIController serves the Monitoring page, the Health column and
// the settings tab under /panel/api/monitoring (docs/spec/monitoring-panel.md
// §7.4): behind the panel session, in the {success,msg,obj} envelope, like
// the rest of /panel/api.
type MonitoringUIController struct {
	monitoringService service.MonitoringService
	settingService    service.SettingService
}

// NewMonitoringUIController mounts the UI routes under g.
func NewMonitoringUIController(g *gin.RouterGroup) *MonitoringUIController {
	a := &MonitoringUIController{}
	a.initRouter(g)
	return a
}

func (a *MonitoringUIController) initRouter(g *gin.RouterGroup) {
	g.GET("/targets", a.targets)
	g.GET("/events", a.events)
	g.GET("/stats", a.stats)
	g.GET("/summary", a.summary)
	g.GET("/status", a.status)
	g.POST("/token/regenerate", a.regenerateToken)
	g.POST("/probe/delete", a.deleteProbeSet)
}

func (a *MonitoringUIController) targets(c *gin.Context) {
	res, err := a.monitoringService.Targets(time.Now())
	if err != nil {
		jsonMsg(c, "monitoring targets", err)
		return
	}
	jsonObj(c, res, nil)
}

func (a *MonitoringUIController) events(c *gin.Context) {
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	kind := c.Query("inboundKind")
	inboundId, _ := strconv.Atoi(c.Query("inboundId"))
	res, err := a.monitoringService.Events(before, limit, kind, inboundId)
	if err != nil {
		jsonMsg(c, "monitoring events", err)
		return
	}
	jsonObj(c, res, nil)
}

func (a *MonitoringUIController) stats(c *gin.Context) {
	inboundId, err := strconv.Atoi(c.Query("inboundId"))
	if err != nil {
		jsonMsg(c, "monitoring stats", errors.New("inboundId is required"))
		return
	}
	res, err := a.monitoringService.Series(c.Query("inboundKind"), inboundId, c.DefaultQuery("range", "24h"), time.Now())
	if err != nil {
		jsonMsg(c, "monitoring stats", err)
		return
	}
	jsonObj(c, res, nil)
}

func (a *MonitoringUIController) summary(c *gin.Context) {
	key := c.DefaultQuery("range", "24h")
	window, ok := service.MonRangeWindow(key)
	if !ok || window > 7*24*time.Hour {
		jsonMsg(c, "monitoring summary", errors.New("range must be 24h or 7d"))
		return
	}
	res, err := a.monitoringService.Summary(time.Now(), window)
	if err != nil {
		jsonMsg(c, "monitoring summary", err)
		return
	}
	jsonObj(c, res, nil)
}

// status is what the settings tab shows: the probe set, the last contact,
// and the STALE flag.
func (a *MonitoringUIController) status(c *gin.Context) {
	probe, err := a.monitoringService.ProbeStatus()
	if err != nil {
		jsonMsg(c, "monitoring status", err)
		return
	}
	stale, staleSince := a.monitoringService.Stale()
	jsonObj(c, gin.H{
		"probe":       probe,
		"lastContact": a.monitoringService.LastContact(),
		"stale":       stale,
		"staleSince":  staleSince,
		"serverTime":  time.Now().UnixMilli(),
	}, nil)
}

// regenerateToken stores a fresh token at once — the old one stops working
// immediately, as the settings tab warns — and returns it for the form.
func (a *MonitoringUIController) regenerateToken(c *gin.Context) {
	token, err := a.settingService.RegenerateMonToken()
	if err != nil {
		jsonMsg(c, "monitoring token", err)
		return
	}
	jsonObj(c, token, nil)
}

func (a *MonitoringUIController) deleteProbeSet(c *gin.Context) {
	err := a.monitoringService.DeleteProbeSet()
	jsonMsg(c, I18nWeb(c, "pages.settings.monProbeSetRemoved"), err)
}
