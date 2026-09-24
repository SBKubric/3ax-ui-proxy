package controller_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	// The probe link renderer registers itself when sub is linked, as it is in
	// the panel binary; an in-package test would not link it.
	_ "github.com/coinman-dev/3ax-ui/v2/sub"
	"github.com/coinman-dev/3ax-ui/v2/web/controller"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/gin-gonic/gin"
)

const monTestToken = "tok0123456789abcdef0123456789ab"

func newMonRouter(t *testing.T) *gin.Engine {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	// monLastContact (and the STALE flag) live in process-wide caches in
	// package service, shared by every MonitoringService regardless of which
	// test's temp DB backs it. Without a reset here, -shuffle=on can hand
	// this test a nonzero monLastContact left behind by an earlier test in
	// the same binary, even though this test's own DB starts empty.
	service.ResetMonContactForTest()
	service.ResetMonStaleForTest()
	service.ResetMonRegistryForTest()
	t.Cleanup(func() {
		service.ResetMonContactForTest()
		service.ResetMonStaleForTest()
		service.ResetMonRegistryForTest()
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	controller.NewMonitoringController(r.Group("/"))
	return r
}

func enableMonitoring(t *testing.T) {
	t.Helper()
	s := &service.SettingService{}
	if err := s.SetMonEnable(true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMonToken(monTestToken); err != nil {
		t.Fatal(err)
	}
}

func monRequest(r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestMonAuthHidesThePanel: without monitoring on, without a token, or with
// the wrong token, every route is a bare 404 with no body and no contract
// header — indistinguishable from a path that does not exist.
func TestMonAuthHidesThePanel(t *testing.T) {
	r := newMonRouter(t)
	routes := [][2]string{{"GET", "/mon/v1/state"}, {"POST", "/mon/v1/probe/ensure"}, {"GET", "/mon/v1/probe/configs"},
		{"DELETE", "/mon/v1/probe"}, {"POST", "/mon/v1/events"}, {"POST", "/mon/v1/stats"}}
	check := func(label, token string) {
		t.Helper()
		for _, rt := range routes {
			w := monRequest(r, rt[0], rt[1], token, "")
			if w.Code != http.StatusNotFound || w.Body.Len() != 0 || w.Header().Get("X-Mon-Contract") != "" {
				t.Errorf("%s %s %s: status %d body %q header %q, want a bare 404", label, rt[0], rt[1], w.Code, w.Body.String(), w.Header().Get("X-Mon-Contract"))
			}
		}
	}
	check("monitoring off, right token", monTestToken)
	s := &service.SettingService{}
	if err := s.SetMonEnable(true); err != nil {
		t.Fatal(err)
	}
	check("enabled but no token issued", monTestToken)
	if err := s.SetMonToken(monTestToken); err != nil {
		t.Fatal(err)
	}
	check("no header", "")
	check("wrong token", "tok0123456789abcdef0123456789aX")
	if got := (&service.MonitoringService{}).MonLastContact(); got != 0 {
		t.Errorf("monLastContact = %d after unauthorised requests, want 0", got)
	}

	w := monRequest(r, "GET", "/mon/v1/state", monTestToken, "")
	if w.Code != http.StatusOK || w.Header().Get("X-Mon-Contract") != "2" {
		t.Fatalf("authorised GET /state: status %d header %q body %s", w.Code, w.Header().Get("X-Mon-Contract"), w.Body.String())
	}
	if got := (&service.MonitoringService{}).MonLastContact(); got == 0 {
		t.Error("monLastContact not stamped by an authorised request")
	}
	if persisted, _ := s.GetMonLastContact(); persisted == 0 {
		t.Error("monLastContact not persisted on first contact")
	}
	var st service.MonState
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st.Contract != 2 || len(st.Revision) != 16 {
		t.Errorf("GET /state body: %v %s", err, w.Body.String())
	}
}

// TestMonRoutesSpeakTheContract walks the six routes with the token: the
// contract header, the status codes of §3 and §4, 413 on oversize batches,
// 400 with a code on a bad body, and the 409s of the probe configs.
func TestMonRoutesSpeakTheContract(t *testing.T) {
	r := newMonRouter(t)
	enableMonitoring(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "in-1", Remark: "r", Enable: true,
		Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000001","email":"alice","enable":true}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}

	// Oversize batches: 413 before anything is looked at.
	big := `{"events":[` + strings.Repeat(`{},`, 1000) + `{}]}`
	if w := monRequest(r, "POST", "/mon/v1/events", monTestToken, big); w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "batch_too_large") {
		t.Errorf("1001 events: %d %s", w.Code, w.Body.String())
	}
	bigStats := `{"stats":[` + strings.Repeat(`{},`, 2000) + `{}]}`
	if w := monRequest(r, "POST", "/mon/v1/stats", monTestToken, bigStats); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("2001 stats: %d %s", w.Code, w.Body.String())
	}
	bigClients := `{"monClients":[` + strings.Repeat(`{"id":"x"},`, 200) + `{"id":"y"}]}`
	if w := monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, bigClients); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("201 monClients: %d %s", w.Code, w.Body.String())
	}
	huge := `{"events":[{"reason":"` + strings.Repeat("x", 1<<20) + `"}]}`
	if w := monRequest(r, "POST", "/mon/v1/events", monTestToken, huge); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over 1 MiB: %d", w.Code)
	}

	// A bad element is rejected by index inside a 200; only an unreadable
	// body is 400 invalid_body.
	if w := monRequest(r, "POST", "/mon/v1/events", monTestToken, `{"events":[{"id":"x","kind":"foo"}]}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"accepted":0`) || !strings.Contains(w.Body.String(), `"rejected":[{"index":0,"id":"x","error":"events[0].id: `) {
		t.Errorf("bad event: %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{`not json`, `{"events":{}}`, `{"events":[]} {}`} {
		if w := monRequest(r, "POST", "/mon/v1/events", monTestToken, body); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_body") {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := monRequest(r, "POST", "/mon/v1/stats", monTestToken, `{"stats":[{"monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"proxy","bucketStart":7}]}`); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"rejected":[{"index":0,"error":"stats[0].bucketStart: `) {
		t.Errorf("bad stat: %d %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if w := monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, `{"monClients":[{"name":"no id"}]}`); w.Code != http.StatusBadRequest {
		t.Errorf("mon-client without id: %d %s", w.Code, w.Body.String())
	}
	// Contract v2: the id names a probe peer, so it is 1–32 of [A-Za-z0-9_-].
	if w := monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, `{"monClients":[{"id":"ams.1"}]}`); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `"error":"invalid_body"`) {
		t.Errorf("mon-client id outside the v2 grammar: %d %s", w.Code, w.Body.String())
	}

	// Probe configs before the set exists: 409.
	if w := monRequest(r, "GET", "/mon/v1/probe/configs?host=203.0.113.10", monTestToken, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "probe_not_ensured") {
		t.Errorf("configs before ensure: %d %s", w.Code, w.Body.String())
	}

	// Ensure, then configs on both paths, then events and stats round-trip.
	w := monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, `{"monClients":[{"id":"ams-1","name":"Amsterdam","region":"NL","state":"ONLINE","lastHeartbeat":1}]}`)
	if w.Code != http.StatusOK || w.Header().Get("X-Mon-Contract") != "2" {
		t.Fatalf("ensure: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m["present"] != float64(1) || len(m["subId"].(string)) != 16 ||
		!strings.Contains(w.Body.String(), `"unallocated":[]`) {
		t.Errorf("ensure body: %v %s", err, w.Body.String())
	}
	if w := monRequest(r, "GET", "/mon/v1/probe/configs", monTestToken, ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "override_disabled") {
		t.Errorf("proxy configs without override: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, "GET", "/mon/v1/probe/configs?host=203.0.113.10", monTestToken, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"path":"direct"`) || !strings.Contains(w.Body.String(), "@203.0.113.10:10001") {
		t.Errorf("direct configs: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, "POST", "/mon/v1/events", monTestToken, `{"events":[{"id":"019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a","ts":1757721540000,"kind":"target","monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"direct","from":"UP","to":"DOWN","reason":"tls_timeout","notified":false}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":1`) {
		t.Errorf("events: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, "POST", "/mon/v1/stats", monTestToken, `{"stats":[{"monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"direct","bucketStart":1757721300000,"nOk":5,"nFail":0,"latencyMinMs":41,"latencyAvgMs":47,"latencyMaxMs":58,"handshakeMs":null}]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ignored":[{"key":"ams-1/xray/1/direct/1757721300000","error":"retention_expired"}]`) {
		t.Errorf("stats (a 2025 bucket is past retention): %d %s", w.Code, w.Body.String())
	}
	if w := monRequest(r, "DELETE", "/mon/v1/probe", monTestToken, ""); w.Code != http.StatusNoContent {
		t.Errorf("delete probe: %d %s", w.Code, w.Body.String())
	}
	if w := monRequest(r, "GET", "/mon/v1/state", monTestToken, ""); !strings.Contains(w.Body.String(), `"subId":null`) {
		t.Errorf("state after delete: %s", w.Body.String())
	}
}

// TestMonEventsAndStatsPerElement: a mixed batch is applied in part — the
// valid elements land, each bad one is named by index (and id) in rejected —
// unknown fields at any level are ignored, and a hop path is an ordinary
// path.
func TestMonEventsAndStatsPerElement(t *testing.T) {
	r := newMonRouter(t)
	enableMonitoring(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "in-1", Remark: "r", Enable: true,
		Settings: `{"clients":[],"decryption":"none"}`, StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}

	w := monRequest(r, "POST", "/mon/v1/events", monTestToken, `{"batchId":"b-1","events":[
		{"id":"019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a","ts":1757721540000,"kind":"target","monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"edge:ams-2","from":"UP","to":"DOWN","reason":"tls_timeout","notified":true,"hopRole":"edge"},
		{"id":"019254a0-8a11-7e30-8c2d-2a3b4c5d6e7f","ts":1757721545000,"kind":"mon_client","monClientId":"msk-1","from":"ONLINE","to":"SLEEPING","notified":true},
		{"id":"019254a0-9b22-7f41-9d3e-3b4c5d6e7f80","ts":"late","kind":"panel","to":"PANEL_DOWN"},
		{"id":"019254a0-ac33-7052-8e4f-4c5d6e7f8091","ts":1757721546000,"kind":"mon_client","monClientId":"msk-1","from":"","to":"ONLINE","notified":true}
	]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mixed events: %d %s", w.Code, w.Body.String())
	}
	var ev struct {
		Accepted int `json:"accepted"`
		Rejected []struct {
			Index int    `json:"index"`
			Id    string `json:"id"`
			Error string `json:"error"`
		} `json:"rejected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Accepted != 2 || len(ev.Rejected) != 2 ||
		ev.Rejected[0].Index != 1 || ev.Rejected[0].Id != "019254a0-8a11-7e30-8c2d-2a3b4c5d6e7f" || !strings.Contains(ev.Rejected[0].Error, "events[1].to") ||
		ev.Rejected[1].Index != 2 || ev.Rejected[1].Id != "019254a0-9b22-7f41-9d3e-3b4c5d6e7f80" {
		t.Errorf("mixed events result: %s", w.Body.String())
	}
	var target model.MonTarget
	if err := db.Where("path = ?", "edge:ams-2").First(&target).Error; err != nil || target.State != "DOWN" {
		t.Errorf("edge target: %+v %v", target, err)
	}

	bucket := (time.Now().UnixMilli() / 300000) * 300000
	b := strconv.FormatInt(bucket, 10)
	w = monRequest(r, "POST", "/mon/v1/stats", monTestToken, `{"stats":[
		{"monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"inner:core-1","bucketStart":`+b+`,"nOk":5,"nFail":0,"latencyMinMs":41,"latencyAvgMs":47,"latencyMaxMs":58,"handshakeMs":null,"jitterMs":3},
		{"monClientId":"ams-1","inboundKind":"xray","inboundId":1,"path":"hop:core-1","bucketStart":`+b+`,"nOk":5,"nFail":0}
	]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":1`) ||
		!strings.Contains(w.Body.String(), `"rejected":[{"index":1,"error":"stats[1].path: unknown value \"hop:core-1\""}]`) {
		t.Errorf("mixed stats: %d %s", w.Code, w.Body.String())
	}

	// An all-good batch still carries an empty rejected list, so mon-server
	// can tell the new answer from the old one.
	w = monRequest(r, "POST", "/mon/v1/stats", monTestToken, `{"stats":[]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"rejected":[]`) {
		t.Errorf("empty stats: %d %s", w.Code, w.Body.String())
	}

	// Unknown fields are ignored on the other routes too.
	w = monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, `{"monClients":[{"id":"ams-1","state":"ONLINE","paths":["direct"]}],"generation":2}`)
	if w.Code != http.StatusOK {
		t.Errorf("ensure with unknown fields: %d %s", w.Code, w.Body.String())
	}
}
