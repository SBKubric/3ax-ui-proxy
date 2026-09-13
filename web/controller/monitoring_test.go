package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

const testMonToken = "0123456789abcdef0123456789abcdef"

// newMonRouter opens a fresh database with monitoring enabled and a token
// set, and mounts the contract the way web.go does.
func newMonRouter(t *testing.T) *gin.Engine {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	service.ResetMonRuntime()
	t.Cleanup(service.ResetMonRuntime)
	settings := &service.SettingService{}
	if err := settings.SetMonEnable(true); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetMonToken(testMonToken); err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.NoRoute(func(c *gin.Context) { c.AbortWithStatus(http.StatusNotFound) })
	NewMonitoringController(r.Group("/"))
	return r
}

func monRequest(r *gin.Engine, method, path, auth string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestMonAuthIsABare404: no token, a wrong token, a wrong scheme, monitoring
// off and an empty configured token all get 404 with an empty body and no
// contract header; an unknown route under the prefix too.
func TestMonAuthIsABare404(t *testing.T) {
	r := newMonRouter(t)
	settings := &service.SettingService{}
	bare := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 || w.Header().Get("X-Mon-Contract") != "" {
			t.Errorf("%s: want a bare 404, got %d %q header=%q", name, w.Code, w.Body.String(), w.Header().Get("X-Mon-Contract"))
		}
	}
	bare("no header", monRequest(r, http.MethodGet, "/mon/v1/state", "", nil))
	bare("wrong token", monRequest(r, http.MethodGet, "/mon/v1/state", "Bearer "+strings.Repeat("x", 32), nil))
	bare("prefix of the token", monRequest(r, http.MethodGet, "/mon/v1/state", "Bearer "+testMonToken[:31], nil))
	bare("wrong scheme", monRequest(r, http.MethodGet, "/mon/v1/state", "Basic "+testMonToken, nil))
	bare("unknown route", monRequest(r, http.MethodGet, "/mon/v1/nope", "Bearer "+testMonToken, nil))
	bare("POST on a GET route", monRequest(r, http.MethodPost, "/mon/v1/state", "Bearer "+testMonToken, nil))

	if err := settings.SetMonToken(""); err != nil {
		t.Fatal(err)
	}
	bare("empty configured token", monRequest(r, http.MethodGet, "/mon/v1/state", "Bearer ", nil))
	if err := settings.SetMonToken(testMonToken); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetMonEnable(false); err != nil {
		t.Fatal(err)
	}
	bare("monitoring off", monRequest(r, http.MethodGet, "/mon/v1/state", "Bearer "+testMonToken, nil))

	if got := (&service.MonitoringService{}).LastContact(); got != 0 {
		t.Errorf("unauthorized requests must not count as contact, lastContact = %d", got)
	}
}

// TestMonAuthorizedState: the right token gets the state with the contract
// header and field, and counts as contact.
func TestMonAuthorizedState(t *testing.T) {
	r := newMonRouter(t)
	w := monRequest(r, http.MethodGet, "/mon/v1/state", "Bearer "+testMonToken, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Mon-Contract") != "1" {
		t.Errorf("X-Mon-Contract header missing: %v", w.Header())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type %q", ct)
	}
	var state struct {
		Contract int             `json:"contract"`
		Revision string          `json:"revision"`
		Probe    map[string]any  `json:"probe"`
		Inbounds json.RawMessage `json:"inbounds"`
		Stale    map[string]any  `json:"stale"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, w.Body.String())
	}
	if state.Contract != 1 || len(state.Revision) != 16 || state.Probe["subId"] != nil || string(state.Inbounds) != "[]" {
		t.Errorf("state body: %s", w.Body.String())
	}
	if got := (&service.MonitoringService{}).LastContact(); got == 0 {
		t.Errorf("an authorized request must count as contact")
	}
}

// TestMonBatchLimits: a batch over the element limit and a body over one
// MiB are both 413 batch_too_large; an unparsable or invalid body is 400
// invalid_body with a message naming the element.
func TestMonBatchLimits(t *testing.T) {
	r := newMonRouter(t)
	auth := "Bearer " + testMonToken
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	decode := func(w *httptest.ResponseRecorder) {
		t.Helper()
		body.Error, body.Message = "", ""
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("error body is not JSON: %s", w.Body.String())
		}
	}

	events := make([]map[string]any, service.MonMaxEvents+1)
	for i := range events {
		events[i] = map[string]any{"id": "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", "ts": 1, "kind": "panel", "from": "PANEL_UP", "to": "PANEL_DOWN", "notified": true}
	}
	w := monRequest(r, http.MethodPost, "/mon/v1/events", auth, map[string]any{"events": events})
	decode(w)
	if w.Code != http.StatusRequestEntityTooLarge || body.Error != "batch_too_large" {
		t.Errorf("oversized events batch: %d %s", w.Code, w.Body.String())
	}

	huge := []byte(`{"stats":[` + strings.Repeat(`{"monClientId":"a","inboundKind":"xray","inboundId":1,"path":"direct","bucketStart":300000,"nOk":1,"nFail":0,"pad":"`+strings.Repeat("p", 900)+`"},`, 1200) + `]}`)
	w = monRequest(r, http.MethodPost, "/mon/v1/stats", auth, huge)
	decode(w)
	if w.Code != http.StatusRequestEntityTooLarge || body.Error != "batch_too_large" {
		t.Errorf("oversized body (%d bytes): %d %s", len(huge), w.Code, body.Error)
	}

	w = monRequest(r, http.MethodPost, "/mon/v1/events", auth, []byte(`{"events": [}`))
	decode(w)
	if w.Code != http.StatusBadRequest || body.Error != "invalid_body" {
		t.Errorf("unparsable body: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, http.MethodPost, "/mon/v1/events", auth, map[string]any{"events": []map[string]any{
		{"id": "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", "ts": 1, "kind": "foo"},
	}})
	decode(w)
	if w.Code != http.StatusBadRequest || body.Error != "invalid_body" || !strings.Contains(body.Message, "events[0].kind") {
		t.Errorf("invalid element: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, http.MethodPost, "/mon/v1/probe/ensure", auth, map[string]any{"monClients": []map[string]any{{"id": "bad id"}}})
	decode(w)
	if w.Code != http.StatusBadRequest || !strings.Contains(body.Message, "monClients[0].id") {
		t.Errorf("invalid mon-client id: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Mon-Contract") != "1" {
		t.Errorf("authorized error responses carry the contract header too")
	}
}

// TestMonContractRoundTrip walks the cycle of contract §7 over HTTP: state,
// ensure, probe configs on both paths, events, stats, delete.
func TestMonContractRoundTrip(t *testing.T) {
	r := newMonRouter(t)
	auth := "Bearer " + testMonToken
	db := database.GetDB()
	ib := &model.Inbound{UserId: 1, Remark: "Reality main", Enable: true, Port: 443, Protocol: model.VLESS, Tag: "inbound-443",
		Settings:       `{"clients":[{"id":"22222222-2222-2222-2222-222222222222","email":"alice","enable":true}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`}
	if err := db.Create(ib).Error; err != nil {
		t.Fatal(err)
	}
	service.RegisterProbeLinkRenderer(func(host string, override bool) (map[int]string, error) {
		if override {
			host = "front.example.net"
		}
		return map[int]string{ib.Id: "vless://probe@" + host + ":443#probe"}, nil
	})
	t.Cleanup(func() { service.RegisterProbeLinkRenderer(nil) })

	w := monRequest(r, http.MethodGet, "/mon/v1/probe/configs", auth, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"probe_not_ensured"`) {
		t.Fatalf("configs before ensure: %d %s", w.Code, w.Body.String())
	}

	w = monRequest(r, http.MethodPost, "/mon/v1/probe/ensure", auth, map[string]any{"monClients": []map[string]any{
		{"id": "ams-1", "name": "Amsterdam #1", "region": "NL", "state": "ONLINE", "lastHeartbeat": 1757721590000},
	}})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"xray_unavailable"`) {
		t.Fatalf("ensure without xray should be 503 xray_unavailable: %d %s", w.Code, w.Body.String())
	}
	// Give the inbound its probe by hand: the core is not running here.
	probe := `{"clients":[{"id":"22222222-2222-2222-2222-222222222222","email":"alice","enable":true},` +
		`{"id":"11111111-1111-1111-1111-111111111111","email":"probe-` + itoa(ib.Id) + `","enable":true,"subId":"probesubid000001"}],"decryption":"none"}`
	if err := db.Model(ib).Update("settings", probe).Error; err != nil {
		t.Fatal(err)
	}
	w = monRequest(r, http.MethodPost, "/mon/v1/probe/ensure", auth, map[string]any{"monClients": []map[string]any{
		{"id": "ams-1", "name": "Amsterdam #1", "region": "NL", "state": "ONLINE", "lastHeartbeat": 1757721590000},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("ensure: %d %s", w.Code, w.Body.String())
	}
	var ensure struct {
		SubId   string `json:"subId"`
		Present int    `json:"present"`
		Created []any  `json:"created"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ensure); err != nil {
		t.Fatal(err)
	}
	// The subId was minted by the first ensure, before creation failed.
	if len(ensure.SubId) != 16 || ensure.Present != 1 || len(ensure.Created) != 0 {
		t.Fatalf("ensure: %s", w.Body.String())
	}

	w = monRequest(r, http.MethodGet, "/mon/v1/probe/configs", auth, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"override_disabled"`) {
		t.Fatalf("proxy configs without override: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, http.MethodGet, "/mon/v1/probe/configs?host=203.0.113.10", auth, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"path":"direct"`) || !strings.Contains(w.Body.String(), "@203.0.113.10:443") {
		t.Fatalf("direct configs: %d %s", w.Code, w.Body.String())
	}

	w = monRequest(r, http.MethodPost, "/mon/v1/events", auth, map[string]any{"events": []map[string]any{
		{"id": "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", "ts": 1757721540000, "kind": "target", "monClientId": "ams-1",
			"inboundKind": "xray", "inboundId": ib.Id, "path": "direct", "from": "UP", "to": "DOWN", "reason": "tls_timeout", "notified": false},
		{"id": "019254a0-7c3e-7d2a-9b4f-1f2e3d4c5b6a", "ts": 1757721540000, "kind": "target", "monClientId": "ams-1",
			"inboundKind": "xray", "inboundId": ib.Id, "path": "direct", "from": "UP", "to": "DOWN", "reason": "tls_timeout", "notified": false},
		{"id": "019254a0-9b22-7f41-9d3e-3b4c5d6e7f80", "ts": 1757721000000, "kind": "target", "monClientId": "ams-1",
			"inboundKind": "xray", "inboundId": 999, "path": "direct", "from": "UP", "to": "DOWN", "reason": "x", "notified": true},
	}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":1,"duplicates":1`) || !strings.Contains(w.Body.String(), `"unknown_inbound"`) {
		t.Fatalf("events: %d %s", w.Code, w.Body.String())
	}

	w = monRequest(r, http.MethodPost, "/mon/v1/stats", auth, map[string]any{"stats": []map[string]any{
		{"monClientId": "ams-1", "inboundKind": "xray", "inboundId": ib.Id, "path": "direct", "bucketStart": bucketNow(),
			"nOk": 5, "nFail": 0, "latencyMinMs": 41, "latencyAvgMs": 47, "latencyMaxMs": 58, "handshakeMs": nil},
	}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"accepted":1,"ignored":[]`) {
		t.Fatalf("stats: %d %s", w.Code, w.Body.String())
	}

	w = monRequest(r, http.MethodDelete, "/mon/v1/probe", auth, nil)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	w = monRequest(r, http.MethodGet, "/mon/v1/state", auth, nil)
	if !strings.Contains(w.Body.String(), `"subId":null`) {
		t.Fatalf("state after delete should have no subId: %s", w.Body.String())
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func bucketNow() int64 {
	now := database.GetDB().NowFunc().UnixMilli()
	return now - now%300_000
}
