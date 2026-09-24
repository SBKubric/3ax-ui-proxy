package controller_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestMonProbeConfigsHopIsUnknown: ?hop= and its synonym ?edge= are read,
// and until per-hop probing every name is 409 unknown_hop (unknown_edge for
// ?edge= alone), never the proxy path (proxy-chain.md §6.1).
func TestMonProbeConfigsHopIsUnknown(t *testing.T) {
	r := newMonRouter(t)
	enableMonitoring(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "in-1", Remark: "r", Enable: true,
		Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000001","email":"alice","enable":true}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"proxyOverrideEnable": "true", "proxyOverrideHost": "front.example.net"} {
		if err := db.Create(&model.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if w := monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, `{"monClients":[{"id":"ams-1"}]}`); w.Code != http.StatusOK {
		t.Fatalf("ensure: %d %s", w.Code, w.Body.String())
	}
	if w := monRequest(r, "GET", "/mon/v1/probe/configs", monTestToken, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"path":"proxy"`) {
		t.Fatalf("proxy configs: %d %s", w.Code, w.Body.String())
	}
	for query, code := range map[string]string{
		"?hop=ams-1":                   `"error":"unknown_hop"`,
		"?edge=ams-1":                  `"error":"unknown_edge"`,
		"?hop=ams-1&edge=ams-1":        `"error":"unknown_hop"`,
		"?hop=ams-1&edge=core":         `"error":"unknown_hop"`,
		"?host=203.0.113.10&hop=ams-1": `"error":"unknown_hop"`,
	} {
		w := monRequest(r, "GET", "/mon/v1/probe/configs"+query, monTestToken, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), code) {
			t.Errorf("%s: %d %s, want 409 %s", query, w.Code, w.Body.String(), code)
		}
	}
	if w := monRequest(r, "GET", "/mon/v1/probe/configs?hop=", monTestToken, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"path":"proxy"`) {
		t.Errorf("empty hop: %d %s", w.Code, w.Body.String())
	}
}
