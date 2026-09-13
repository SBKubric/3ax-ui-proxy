package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/coinman-dev/3ax-ui/v2/web/session"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

// TestMonitoringUIRoutesNeedTheSession: without a session every route is a
// bare 404 like the rest of /panel/api; with one they answer in the
// {success,msg,obj} envelope, the limit is clamped and the token can be
// regenerated.
func TestMonitoringUIRoutesNeedTheSession(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	service.ResetMonRuntime()
	t.Cleanup(service.ResetMonRuntime)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sessions.Sessions("3ax-ui", cookie.NewStore([]byte("secret"))))
	r.GET("/login", func(c *gin.Context) {
		if err := session.SetLoginUser(c, &model.User{Id: 1, Username: "admin"}); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
		}
	})
	api := r.Group("/panel/api")
	api.Use((&APIController{}).checkAPIAuth)
	NewMonitoringUIController(api.Group("/monitoring"))
	r.NoRoute(func(c *gin.Context) { c.AbortWithStatus(http.StatusNotFound) })

	get := func(path, cookieHeader string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookieHeader != "" {
			req.Header.Set("Cookie", cookieHeader)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/panel/api/monitoring/targets", "/panel/api/monitoring/events", "/panel/api/monitoring/stats?inboundKind=xray&inboundId=1", "/panel/api/monitoring/summary", "/panel/api/monitoring/status"} {
		if w := get(path, ""); w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Errorf("%s without a session: %d %q", path, w.Code, w.Body.String())
		}
	}

	login := get("/login", "")
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no cookie")
	}
	cookieHeader := cookies[0].Name + "=" + cookies[0].Value

	var env struct {
		Success bool            `json:"success"`
		Msg     string          `json:"msg"`
		Obj     json.RawMessage `json:"obj"`
	}
	decode := func(w *httptest.ResponseRecorder) {
		t.Helper()
		env.Success, env.Msg, env.Obj = false, "", nil
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("not the panel envelope: %v\n%s", err, w.Body.String())
		}
	}

	w := get("/panel/api/monitoring/targets", cookieHeader)
	decode(w)
	if w.Code != http.StatusOK || !env.Success || !strings.Contains(string(env.Obj), `"inbounds":[]`) || !strings.Contains(string(env.Obj), `"thresholdMinutes":15`) {
		t.Errorf("targets: %d %s", w.Code, w.Body.String())
	}
	w = get("/panel/api/monitoring/events?limit=9999", cookieHeader)
	decode(w)
	if !env.Success || string(env.Obj) != "[]" {
		t.Errorf("events: %s", w.Body.String())
	}
	w = get("/panel/api/monitoring/stats?inboundKind=xray&inboundId=1&range=7d", cookieHeader)
	decode(w)
	if !env.Success || !strings.Contains(string(env.Obj), `"stepMs":3600000`) {
		t.Errorf("stats 7d should come from the hourly rollup: %s", w.Body.String())
	}
	w = get("/panel/api/monitoring/stats?inboundKind=xray&inboundId=1&range=1h", cookieHeader)
	decode(w)
	if !env.Success || !strings.Contains(string(env.Obj), `"stepMs":300000`) {
		t.Errorf("stats 1h should come from the fine rows: %s", w.Body.String())
	}
	w = get("/panel/api/monitoring/stats?inboundKind=xray&inboundId=1&range=30d", cookieHeader)
	decode(w)
	if !env.Success {
		t.Errorf("stats 30d: %s", w.Body.String())
	}
	w = get("/panel/api/monitoring/summary?range=7d", cookieHeader)
	decode(w)
	if !env.Success || !strings.Contains(string(env.Obj), `"inbounds":[]`) {
		t.Errorf("summary: %s", w.Body.String())
	}
	w = get("/panel/api/monitoring/summary?range=30d", cookieHeader)
	decode(w)
	if env.Success {
		t.Errorf("summary must refuse 30d: %s", w.Body.String())
	}
	w = get("/panel/api/monitoring/status", cookieHeader)
	decode(w)
	if !env.Success || !strings.Contains(string(env.Obj), `"count":0`) {
		t.Errorf("status: %s", w.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/panel/api/monitoring/token/regenerate", nil)
	req.Header.Set("Cookie", cookieHeader)
	post := httptest.NewRecorder()
	r.ServeHTTP(post, req)
	decode(post)
	var token string
	if !env.Success || json.Unmarshal(env.Obj, &token) != nil || len(token) != 32 {
		t.Fatalf("regenerate: %s", post.Body.String())
	}
	if stored, _ := (&service.SettingService{}).GetMonToken(); stored != token {
		t.Errorf("regenerated token not stored: %q vs %q", stored, token)
	}
}
