package controller_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestMonProbeConfigsThroughAHop (contract 3 §4.4): ?hop= and its synonym
// ?edge= answer with the hop's path; a name the registry does not have, or two
// different names, is 409 unknown_hop, a pending hop 409 hop_not_joined —
// never the proxy path.
func TestMonProbeConfigsThroughAHop(t *testing.T) {
	r := newMonRouter(t)
	enableMonitoring(t)
	db := database.GetDB()
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "in-1", Remark: "r", Enable: true,
		Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000001","email":"alice","enable":true}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, hop := range []model.ChainHop{
		{Name: "core-1", Role: "inner", State: "joined", Host: "10.0.0.7"},
		{Name: "edge-a", Role: "edge", State: "joined", Host: "a.example.net", IsActive: true},
		{Name: "edge-b", Role: "edge", State: "pending", Host: "b.example.net"},
	} {
		if err := db.Create(&hop).Error; err != nil {
			t.Fatal(err)
		}
	}
	if w := monRequest(r, "POST", "/mon/v1/probe/ensure", monTestToken, `{"monClients":[{"id":"ams-1","paths":["direct","hops"]}]}`); w.Code != http.StatusOK {
		t.Fatalf("ensure: %d %s", w.Code, w.Body.String())
	}
	for query, want := range map[string]string{
		"?hop=core-1":                   `"path":"inner:core-1"`,
		"?edge=edge-a":                  `"path":"edge:edge-a"`,
		"?hop=edge-a&edge=edge-a":       `"path":"edge:edge-a"`,
		"?host=203.0.113.10&hop=core-1": `"path":"inner:core-1"`,
	} {
		w := monRequest(r, "GET", "/mon/v1/probe/configs"+query, monTestToken, "")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: %d %s, want 200 %s", query, w.Code, w.Body.String(), want)
		}
	}
	for query, code := range map[string]string{
		"?hop=ams-1":              `"error":"unknown_hop"`,
		"?edge=ams-1":             `"error":"unknown_hop"`,
		"?hop=edge-a&edge=core-1": `"error":"unknown_hop"`,
		"?hop=edge-b":             `"error":"hop_not_joined"`,
	} {
		w := monRequest(r, "GET", "/mon/v1/probe/configs"+query, monTestToken, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), code) {
			t.Errorf("%s: %d %s, want 409 %s", query, w.Code, w.Body.String(), code)
		}
	}
}
