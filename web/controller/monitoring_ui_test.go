package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/coinman-dev/3ax-ui/v2/web/session"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

// monUIEnvelope is the panel's {success,msg,obj} answer, with obj kept raw so
// each test decodes the shape it cares about.
type monUIEnvelope struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

// newMonUIRouter builds the panel API the way APIController.initRouter does —
// the /panel/api group guarded by checkAPIAuth, with the monitoring group
// hanging off it — but without the rest of the panel: NewAPIController would
// also start the server controller's background task, which a route test has
// no use for.
func newMonUIRouter(t *testing.T) *gin.Engine {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	gin.SetMode(gin.TestMode)

	r := gin.New()
	store := cookie.NewStore([]byte("monitoring-ui-test-secret"))
	r.Use(sessions.Sessions("3ax-ui", store))
	// A test-only login: the panel's own login handler checks credentials,
	// logs and throttles, while checkAPIAuth only ever looks at the session.
	r.GET("/test-login", func(c *gin.Context) {
		if err := session.SetLoginUser(c, &model.User{Id: 1, Username: "admin"}); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusOK)
	})

	a := &APIController{}
	api := r.Group("/panel/api")
	api.Use(a.checkAPIAuth)
	NewMonitoringUIController(api.Group("/monitoring"))
	return r
}

// monUILogin returns the session cookie of a logged-in panel user.
func monUILogin(t *testing.T, r *gin.Engine) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test-login", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("test login: status %d", w.Code)
	}
	c := w.Header().Get("Set-Cookie")
	if c == "" {
		t.Fatal("test login issued no session cookie")
	}
	return c
}

func monUIGet(r *gin.Engine, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// monUIDecode reads the envelope of a 200 answer.
func monUIDecode(t *testing.T, w *httptest.ResponseRecorder) monUIEnvelope {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body.String())
	}
	var env monUIEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v (%s)", err, w.Body.String())
	}
	return env
}

var monUIRoutes = []string{
	"/panel/api/monitoring/targets",
	"/panel/api/monitoring/events",
	"/panel/api/monitoring/stats?inboundKind=xray&inboundId=1",
	"/panel/api/monitoring/summary",
	"/panel/api/monitoring/probe",
}

// TestMonUIRoutesNeedASession: without a panel session every route answers
// the bare 404 checkAPIAuth gives the rest of the panel API, so the page's
// endpoints are as invisible as the inbound ones.
func TestMonUIRoutesNeedASession(t *testing.T) {
	r := newMonUIRouter(t)
	for _, path := range monUIRoutes {
		w := monUIGet(r, path, "")
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Errorf("%s without a session: status %d body %q, want a bare 404", path, w.Code, w.Body.String())
		}
	}
	cookie := monUILogin(t, r)
	for _, path := range monUIRoutes {
		if w := monUIGet(r, path, cookie); w.Code != http.StatusOK {
			t.Errorf("%s with a session: status %d body %s", path, w.Code, w.Body.String())
		}
	}
}

// TestMonUIHandlersAnswerInTheEnvelope walks the four routes with a session:
// the shapes §7.1 asks for, the limit the feed caps at, and a parameter error
// that comes back as success:false rather than as a status code.
func TestMonUIHandlersAnswerInTheEnvelope(t *testing.T) {
	r := newMonUIRouter(t)
	cookie := monUILogin(t, r)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "in-1", Remark: "Reality main", Enable: true,
		Settings:       `{"clients":[],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.MonTarget{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct",
		State: model.MonStateDown, Since: time.Now().UnixMilli(), Reason: "tls_timeout"}).Error; err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).UnixMilli()
	for i := 0; i < 205; i++ {
		ev := model.MonEvent{Id: fmt.Sprintf("%036d", i), Ts: base + int64(i)*1000, ReceivedAt: base,
			Kind: model.MonEventKindTarget, MonClientId: "ams-1", InboundKind: "xray", InboundId: 1,
			Path: "direct", From: "UP", To: "DOWN", Notified: true}
		if err := db.Create(&ev).Error; err != nil {
			t.Fatal(err)
		}
	}

	var targets service.MonUITargets
	env := monUIDecode(t, monUIGet(r, "/panel/api/monitoring/targets", cookie))
	if !env.Success {
		t.Fatalf("targets: %s", env.Msg)
	}
	if err := json.Unmarshal(env.Obj, &targets); err != nil {
		t.Fatalf("targets obj: %v (%s)", err, env.Obj)
	}
	if len(targets.Inbounds) != 1 || len(targets.Inbounds[0].Targets) != 1 {
		t.Fatalf("targets = %+v, want one inbound with one target", targets.Inbounds)
	}
	// No registry snapshot has arrived, so the only target is retired and
	// leaves the badge empty.
	if !targets.Inbounds[0].Targets[0].Retired || targets.Inbounds[0].Worst != "" {
		t.Errorf("target = %+v worst %q, want retired with no badge", targets.Inbounds[0].Targets[0], targets.Inbounds[0].Worst)
	}

	var events []service.MonUIEvent
	env = monUIDecode(t, monUIGet(r, "/panel/api/monitoring/events?limit=500", cookie))
	if err := json.Unmarshal(env.Obj, &events); err != nil {
		t.Fatalf("events obj: %v (%s)", err, env.Obj)
	}
	if !env.Success || len(events) != 200 {
		t.Errorf("limit=500 returned %d events, want the 200 the handler clamps to", len(events))
	}
	if events[0].InboundRemark != "Reality main" {
		t.Errorf("event = %+v, want the inbound remark filled in", events[0])
	}

	var stats service.MonUIStats
	env = monUIDecode(t, monUIGet(r, "/panel/api/monitoring/stats?inboundKind=xray&inboundId=1&range=1h", cookie))
	if err := json.Unmarshal(env.Obj, &stats); err != nil {
		t.Fatalf("stats obj: %v (%s)", err, env.Obj)
	}
	if !env.Success || stats.Source != service.MonUISourceCurrent || stats.Series == nil || len(stats.Series) != 0 {
		t.Errorf("stats = %+v, want an empty current-table grid", stats)
	}

	// Parameter errors: the envelope says so, the status stays 200.
	for _, path := range []string{
		"/panel/api/monitoring/stats?inboundKind=xray&inboundId=1&range=90m",
		"/panel/api/monitoring/summary?range=30d", // summary only goes to 7d
		"/panel/api/monitoring/stats?inboundId=1",
		"/panel/api/monitoring/stats?inboundKind=xray",
		"/panel/api/monitoring/events?limit=abc",
	} {
		env := monUIDecode(t, monUIGet(r, path, cookie))
		if env.Success {
			t.Errorf("%s: success=true, want a parameter error", path)
		}
	}
}

// TestMonUISummaryIsTheStepEightCalculation: GET summary is Summary() and
// nothing else — the page and the daily Telegram digest print one set of
// numbers. Only the window edges differ, because the handler reads its own
// clock a moment later.
func TestMonUISummaryIsTheStepEightCalculation(t *testing.T) {
	r := newMonUIRouter(t)
	cookie := monUILogin(t, r)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "in-1", Remark: "Reality main", Enable: true,
		Settings:       `{"clients":[],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-2 * time.Hour).UnixMilli()
	for i := 0; i < 6; i++ {
		row := model.MonStatsCurrent{MonClientId: "ams-1", InboundKind: "xray", InboundId: 1, Path: "direct",
			BucketStart: start + int64(i)*300000, BucketMs: 300000, NOk: 9, NFail: 1}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}

	env := monUIDecode(t, monUIGet(r, "/panel/api/monitoring/summary?range=24h", cookie))
	if !env.Success {
		t.Fatalf("summary: %s", env.Msg)
	}
	want, err := (&service.MonitoringService{}).Summary(time.Now(), 24*time.Hour)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	var fromHandler, fromService map[string]any
	if err := json.Unmarshal(env.Obj, &fromHandler); err != nil {
		t.Fatalf("summary obj: %v (%s)", err, env.Obj)
	}
	if err := json.Unmarshal(raw, &fromService); err != nil {
		t.Fatal(err)
	}
	if to, from := fromHandler["to"].(float64), fromHandler["from"].(float64); to-from != float64(24*time.Hour/time.Millisecond) {
		t.Errorf("window = %v ms, want 24h", to-from)
	}
	for _, edge := range []string{"from", "to"} {
		delete(fromHandler, edge)
		delete(fromService, edge)
	}
	if !reflect.DeepEqual(fromHandler, fromService) {
		t.Errorf("handler summary\n %#v\ndiffers from Summary()\n %#v", fromHandler, fromService)
	}
}

// monUISend runs a request with a body-less method other than GET.
func monUISend(r *gin.Engine, method, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestMonUIProbeInfoReportsTheSet: GET probe is the settings tab's only way
// to the three state keys that AllSetting does not carry.
func TestMonUIProbeInfoReportsTheSet(t *testing.T) {
	r := newMonUIRouter(t)
	cookie := monUILogin(t, r)

	var info service.MonProbeSetInfo
	env := monUIDecode(t, monUIGet(r, "/panel/api/monitoring/probe", cookie))
	if !env.Success {
		t.Fatalf("probe: %s", env.Msg)
	}
	if err := json.Unmarshal(env.Obj, &info); err != nil {
		t.Fatalf("probe obj: %v (%s)", err, env.Obj)
	}
	if info.SubId != "" || info.LastEnsured != 0 || info.Clients != 0 || info.TtlHours != 24 {
		t.Errorf("probe = %+v, want an empty set and the default TTL", info)
	}

	setting := service.SettingService{}
	if err := setting.SetMonProbeSubId("k3j9d8s7f6g5h4j3"); err != nil {
		t.Fatal(err)
	}
	if err := setting.SetMonProbeLastEnsured(1757764680000); err != nil {
		t.Fatal(err)
	}
	env = monUIDecode(t, monUIGet(r, "/panel/api/monitoring/probe", cookie))
	if err := json.Unmarshal(env.Obj, &info); err != nil {
		t.Fatalf("probe obj: %v (%s)", err, env.Obj)
	}
	if info.SubId != "k3j9d8s7f6g5h4j3" || info.LastEnsured != 1757764680000 {
		t.Errorf("probe = %+v, want the stored subId and ensure stamp", info)
	}
}

// TestMonUITokenResetReturnsTheLiveToken: Regenerate hands the tab a new
// token that is already the panel's, so the old one stops working at once —
// checkMonAuth reads the setting on every request and holds no copy.
func TestMonUITokenResetReturnsTheLiveToken(t *testing.T) {
	r := newMonUIRouter(t)
	cookie := monUILogin(t, r)
	setting := service.SettingService{}
	if err := setting.SetMonToken("old-token"); err != nil {
		t.Fatal(err)
	}

	env := monUIDecode(t, monUISend(r, http.MethodPost, "/panel/api/monitoring/token/reset", cookie))
	if !env.Success {
		t.Fatalf("token reset: %s", env.Msg)
	}
	var obj struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(env.Obj, &obj); err != nil {
		t.Fatalf("token obj: %v (%s)", err, env.Obj)
	}
	if obj.Token == "" || obj.Token == "old-token" {
		t.Fatalf("token = %q, want a fresh one", obj.Token)
	}
	stored, err := setting.GetMonToken()
	if err != nil {
		t.Fatal(err)
	}
	if stored != obj.Token {
		t.Errorf("stored token %q is not the one handed to the page %q", stored, obj.Token)
	}
}

// TestMonUIProbeDeleteClearsTheSet: the tab's "Remove probe set" button is
// DeleteProbeSet, the same one the TTL sweep and the mon-server contract use.
func TestMonUIProbeDeleteClearsTheSet(t *testing.T) {
	r := newMonUIRouter(t)
	cookie := monUILogin(t, r)
	setting := service.SettingService{}
	if err := setting.SetMonProbeSubId("k3j9d8s7f6g5h4j3"); err != nil {
		t.Fatal(err)
	}
	if err := setting.SetMonProbeLastEnsured(1757764680000); err != nil {
		t.Fatal(err)
	}

	env := monUIDecode(t, monUISend(r, http.MethodDelete, "/panel/api/monitoring/probe", cookie))
	if !env.Success {
		t.Fatalf("probe delete: %s", env.Msg)
	}
	subId, err := setting.GetMonProbeSubId()
	if err != nil {
		t.Fatal(err)
	}
	if subId != "" {
		t.Errorf("subId %q survived the delete", subId)
	}

	var info service.MonProbeSetInfo
	env = monUIDecode(t, monUIGet(r, "/panel/api/monitoring/probe", cookie))
	if err := json.Unmarshal(env.Obj, &info); err != nil {
		t.Fatalf("probe obj: %v (%s)", err, env.Obj)
	}
	if info.SubId != "" || info.LastEnsured != 0 {
		t.Errorf("probe = %+v after the delete, want an empty set", info)
	}
}

// TestMonUIWriteRoutesNeedASession: the two routes that change something are
// as invisible without a session as the read-only ones.
func TestMonUIWriteRoutesNeedASession(t *testing.T) {
	r := newMonUIRouter(t)
	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/panel/api/monitoring/token/reset"},
		{http.MethodDelete, "/panel/api/monitoring/probe"},
	} {
		w := monUISend(r, call.method, call.path, "")
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Errorf("%s %s without a session: status %d body %q, want a bare 404", call.method, call.path, w.Code, w.Body.String())
		}
	}
}
