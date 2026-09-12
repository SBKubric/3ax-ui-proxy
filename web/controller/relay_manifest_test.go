package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestGetRelayManifestServesASanitisedManifest: the Settings button and the
// CLI share one producer, so the endpoint must hand out a document the proxy
// front accepts and that carries none of the panel's secrets.
func TestGetRelayManifestServesASanitisedManifest(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	cfg := `{"inbounds":[{"listen":"0.0.0.0","port":443,"protocol":"vless","tag":"inbound-443",
	  "streamSettings":{"security":"reality","realitySettings":{"privateKey":"SECRET-KEY"}}}]}`
	if err := os.WriteFile(filepath.Join(bin, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// initRouter alone: NewServerController also starts background tasks that
	// need the database.
	(&ServerController{}).initRouter(r.Group("/panel/api/server"))

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panel/api/server/getRelayManifest", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
		Obj     string `json:"obj"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("not the panel envelope: %v\n%s", err, w.Body.String())
	}
	if !env.Success {
		t.Fatalf("success=false: %s", env.Msg)
	}
	if !strings.Contains(env.Obj, `"relayManifest"`) || !strings.Contains(env.Obj, `"port": 443`) {
		t.Fatalf("obj is not a relay manifest:\n%s", env.Obj)
	}
	if strings.Contains(env.Obj, "SECRET-KEY") {
		t.Fatalf("manifest leaks the Reality key:\n%s", env.Obj)
	}
}
