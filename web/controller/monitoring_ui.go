package controller

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/gin-gonic/gin"
)

// MonitoringUIController serves the Monitoring page of the panel under
// <webBasePath>panel/api/monitoring (docs/spec/monitoring-panel.md §7.4).
//
// Unlike MonitoringController, which speaks the mon-server contract with a
// bearer token, this is ordinary panel API: it hangs off the /panel/api group
// and therefore inherits APIController.checkAPIAuth, so a request without a
// session gets the same bare 404 as every other panel route. No auth code
// lives here.
//
// Every handler parses its query string and calls one service method; the
// answers travel in the panel's {success,msg,obj} envelope.
type MonitoringUIController struct {
	monitoringService service.MonitoringService
}

// NewMonitoringUIController registers the four read-only routes on g.
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
}

// monUIStatsRanges and monUISummaryRanges are the windows §7.4 allows; an
// absent range means 24h, anything else is a parameter error.
var (
	monUIStatsRanges = map[string]time.Duration{
		"1h":  time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	monUISummaryRanges = map[string]time.Duration{
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
	}
)

const monUIDefaultRange = "24h"

// parseMonUIRange resolves the range query parameter against the windows a
// handler accepts.
//
// The messages below are plain English on purpose: the I18nWeb keys of the
// Monitoring page land with the page itself (§7.5, step 10), and a parameter
// error here is a bug in the page's own request rather than something a user
// reads.
func parseMonUIRange(c *gin.Context, allowed map[string]time.Duration) (time.Duration, error) {
	raw := c.Query("range")
	if raw == "" {
		raw = monUIDefaultRange
	}
	window, ok := allowed[raw]
	if !ok {
		names := make([]string, 0, len(allowed))
		for name := range allowed {
			names = append(names, name)
		}
		sort.Strings(names)
		return 0, errors.New("range " + strconv.Quote(raw) + " is not one of " + strings.Join(names, ", "))
	}
	return window, nil
}

// monUIInbound reads the inbound the stats handler works on; both parameters
// are required, and inboundId is 0 for the AmneziaWG server.
func monUIInbound(c *gin.Context) (string, int, error) {
	kind := c.Query("inboundKind")
	if kind == "" {
		return "", 0, errors.New("inboundKind is required")
	}
	raw, ok := c.GetQuery("inboundId")
	if !ok {
		return "", 0, errors.New("inboundId is required")
	}
	id, err := strconv.Atoi(raw)
	if err != nil {
		return "", 0, errors.New("inboundId must be a number")
	}
	return kind, id, nil
}

// GET targets — the page header, the mon-client pills and one card per
// inbound with its targets and Health badge.
func (a *MonitoringUIController) targets(c *gin.Context) {
	obj, err := a.monitoringService.UITargets(time.Now())
	jsonObj(c, obj, err)
}

// GET events?before=<ms>&limit=<n≤200>[&inboundKind&inboundId] — the feed.
func (a *MonitoringUIController) events(c *gin.Context) {
	before, err := monUIInt(c, "before")
	if err != nil {
		jsonMsg(c, "monitoring: bad events request", err)
		return
	}
	limit, err := monUIInt(c, "limit")
	if err != nil {
		jsonMsg(c, "monitoring: bad events request", err)
		return
	}
	// The inbound filter is optional here: without it the feed also carries
	// the mon_client and panel events, which belong to no inbound.
	kind := c.Query("inboundKind")
	inboundId := 0
	if kind != "" {
		if kind, inboundId, err = monUIInbound(c); err != nil {
			jsonMsg(c, "monitoring: bad events request", err)
			return
		}
	}
	obj, err := a.monitoringService.UIEvents(before, int(limit), kind, inboundId)
	jsonObj(c, obj, err)
}

// GET stats?inboundKind&inboundId&range=1h|24h|7d|30d — the series.
func (a *MonitoringUIController) stats(c *gin.Context) {
	kind, inboundId, err := monUIInbound(c)
	if err != nil {
		jsonMsg(c, "monitoring: bad stats request", err)
		return
	}
	window, err := parseMonUIRange(c, monUIStatsRanges)
	if err != nil {
		jsonMsg(c, "monitoring: bad stats request", err)
		return
	}
	obj, err := a.monitoringService.UIStats(time.Now(), kind, inboundId, window)
	jsonObj(c, obj, err)
}

// GET summary?range=24h|7d — uptime, coverage and incidents. This is the
// step-8 calculation (monitoring_summary.go) unchanged: the page and the
// daily Telegram digest print the same numbers.
func (a *MonitoringUIController) summary(c *gin.Context) {
	window, err := parseMonUIRange(c, monUISummaryRanges)
	if err != nil {
		jsonMsg(c, "monitoring: bad summary request", err)
		return
	}
	obj, err := a.monitoringService.Summary(time.Now(), window)
	jsonObj(c, obj, err)
}

// monUIInt reads an optional non-negative integer parameter; absent is 0, and
// the service applies the defaults.
func monUIInt(c *gin.Context, name string) (int64, error) {
	raw, ok := c.GetQuery(name)
	if !ok || raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, errors.New(name + " must be a non-negative number")
	}
	return v, nil
}
