package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/web/controller"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/gin-gonic/gin"
)

// Tests for the `x-ui setting` monitoring flags (-showMonToken,
// -resetMonToken, -monEnable), the CLI duplicate of the Monitoring settings
// tab (docs/spec/monitoring-panel.md §7.3). Each test runs against its own
// throwaway SQLite database; the helpers never init the database themselves,
// so they can be driven straight from a test.
func newMonTestDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
}

// TestShowMonSettingOnAFreshDB: a panel nobody has configured says monitoring
// is off and names the absent token instead of printing an empty value.
func TestShowMonSettingOnAFreshDB(t *testing.T) {
	newMonTestDB(t)

	var out bytes.Buffer
	if err := showMonSetting(&out); err != nil {
		t.Fatalf("showMonSetting: %v", err)
	}
	if got, want := out.String(), "monEnable: false\nmonToken: (not issued)\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
}

// TestSetMonEnable covers the parsing of the -monEnable value: strconv.ParseBool
// spellings are accepted, anything else is refused with a message and leaves
// the setting as it was.
func TestSetMonEnable(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    bool
		wantOut string
		wantErr bool
	}{
		{name: "true", raw: "true", want: true, wantOut: "monEnable: true\n"},
		{name: "false", raw: "false", want: false, wantOut: "monEnable: false\n"},
		{name: "one", raw: "1", want: true, wantOut: "monEnable: true\n"},
		{name: "garbage", raw: "yes-please", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newMonTestDB(t)
			settingService := service.SettingService{}
			// A known starting point the garbage case must not disturb.
			if err := settingService.SetMonEnable(false); err != nil {
				t.Fatalf("SetMonEnable: %v", err)
			}

			var out bytes.Buffer
			err := setMonEnable(&out, tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("setMonEnable(%q): want an error, got nil (output %q)", tt.raw, out.String())
				}
				if !strings.Contains(err.Error(), tt.raw) {
					t.Errorf("error %q does not name the rejected value %q", err, tt.raw)
				}
				if out.Len() != 0 {
					t.Errorf("rejected value still printed %q", out.String())
				}
			} else {
				if err != nil {
					t.Fatalf("setMonEnable(%q): %v", tt.raw, err)
				}
				if out.String() != tt.wantOut {
					t.Errorf("output %q, want %q", out.String(), tt.wantOut)
				}
			}

			got, err := settingService.GetMonEnable()
			if err != nil {
				t.Fatalf("GetMonEnable: %v", err)
			}
			if got != tt.want {
				t.Errorf("monEnable is %v, want %v", got, tt.want)
			}
		})
	}
}

// TestResetMonToken: the printed token is the one the panel stores, and a
// second reset issues a different one.
func TestResetMonToken(t *testing.T) {
	newMonTestDB(t)
	settingService := service.SettingService{}

	first := resetAndParseToken(t)
	if len(first) != 32 {
		t.Errorf("token %q has %d characters, want 32", first, len(first))
	}
	stored, err := settingService.GetMonToken()
	if err != nil {
		t.Fatalf("GetMonToken: %v", err)
	}
	if stored != first {
		t.Errorf("stored token %q, printed %q", stored, first)
	}

	second := resetAndParseToken(t)
	if second == first {
		t.Errorf("the second reset reissued the same token %q", second)
	}
}

// resetAndParseToken runs the CLI reset helper and returns the token it
// printed, checking the shape of the output on the way (monEnable first, so
// the operator sees whether /mon/v1 is actually open).
func resetAndParseToken(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	if err := resetMonToken(&out); err != nil {
		t.Fatalf("resetMonToken: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "monEnable: ") || !strings.HasPrefix(lines[1], "monToken: ") {
		t.Fatalf("reset printed %q, want a monEnable line then a monToken line", out.String())
	}
	return strings.TrimPrefix(lines[1], "monToken: ")
}

// TestMonCLIOpensAndClosesTheContract is the ticket's acceptance check: a token
// issued from the CLI is what /mon/v1 accepts, a fresh reset locks the old one
// out, and turning monEnable off closes the endpoint for every token.
func TestMonCLIOpensAndClosesTheContract(t *testing.T) {
	newMonTestDB(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	controller.NewMonitoringController(r.Group("/"))

	state := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/mon/v1/state", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	var enableOut bytes.Buffer
	if err := setMonEnable(&enableOut, "true"); err != nil {
		t.Fatalf("setMonEnable: %v", err)
	}
	first := resetAndParseToken(t)

	if w := state(first); w.Code != http.StatusOK {
		t.Fatalf("state with the issued token: %d, want 200", w.Code)
	} else if w.Header().Get("X-Mon-Contract") != "2" {
		t.Errorf("X-Mon-Contract %q, want 2", w.Header().Get("X-Mon-Contract"))
	}

	second := resetAndParseToken(t)
	if w := state(first); w.Code != http.StatusNotFound {
		t.Errorf("state with the superseded token: %d, want 404", w.Code)
	}
	if w := state(second); w.Code != http.StatusOK {
		t.Errorf("state with the new token: %d, want 200", w.Code)
	}

	var disableOut bytes.Buffer
	if err := setMonEnable(&disableOut, "false"); err != nil {
		t.Fatalf("setMonEnable: %v", err)
	}
	if w := state(second); w.Code != http.StatusNotFound {
		t.Errorf("state with monitoring disabled: %d, want 404", w.Code)
	}
}

// TestRunMonSettingPrintsTheFinalStateOnce: `-monEnable true -resetMonToken`
// (and `-showMonToken` alongside) reports the state the panel ends up in, with
// no line printed twice.
func TestRunMonSettingPrintsTheFinalStateOnce(t *testing.T) {
	newMonTestDB(t)

	var out bytes.Buffer
	if err := runMonSetting(&out, "true", true, true); err != nil {
		t.Fatalf("runMonSetting: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output %q, want exactly two lines", out.String())
	}
	if lines[0] != "monEnable: true" {
		t.Errorf("first line %q, want monEnable: true", lines[0])
	}
	token := strings.TrimPrefix(lines[1], "monToken: ")
	if token == lines[1] || len(token) != 32 {
		t.Fatalf("second line %q, want a 32-character monToken", lines[1])
	}
	settingService := service.SettingService{}
	stored, err := settingService.GetMonToken()
	if err != nil {
		t.Fatalf("GetMonToken: %v", err)
	}
	if stored != token {
		t.Errorf("stored token %q, printed %q", stored, token)
	}
}
