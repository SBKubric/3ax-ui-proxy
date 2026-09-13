package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/web/controller"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// TestMonitoringCLIChangesTheSettingsAndInvalidatesTheOldToken: the setting
// flags create and reset the token and flip the switch, and a reset token
// is refused by the contract's auth right away while the new one passes.
func TestMonitoringCLIChangesTheSettingsAndInvalidatesTheOldToken(t *testing.T) {
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	t.Setenv("XUI_LOG_FOLDER", t.TempDir())
	service.ResetMonRuntime()
	t.Cleanup(service.ResetMonRuntime)

	if err := updateMonitoringSetting(true, true, true, false); err != nil {
		t.Fatalf("reset+enable: %v", err)
	}
	settings := &service.SettingService{}
	first, err := settings.GetMonToken()
	if err != nil || len(first) != 32 {
		t.Fatalf("token after reset: %q %v", first, err)
	}
	if enabled, _ := settings.GetMonEnable(); !enabled {
		t.Fatal("-enableMonitoring did not enable monitoring")
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	controller.NewMonitoringController(r.Group("/"))
	state := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/mon/v1/state", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := state(first); code != http.StatusOK {
		t.Fatalf("fresh token refused: %d", code)
	}

	if err := updateMonitoringSetting(false, true, false, false); err != nil {
		t.Fatalf("second reset: %v", err)
	}
	second, _ := settings.GetMonToken()
	if second == first || len(second) != 32 {
		t.Fatalf("second reset did not change the token: %q -> %q", first, second)
	}
	if code := state(first); code != http.StatusNotFound {
		t.Errorf("old token still accepted: %d", code)
	}
	if code := state(second); code != http.StatusOK {
		t.Errorf("new token refused: %d", code)
	}

	if err := updateMonitoringSetting(false, false, false, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if enabled, _ := settings.GetMonEnable(); enabled {
		t.Fatal("-disableMonitoring did not disable monitoring")
	}
	if code := state(second); code != http.StatusNotFound {
		t.Errorf("monitoring off must be a 404: %d", code)
	}
	if err := database.CloseDB(); err != nil {
		t.Log(err)
	}
}
