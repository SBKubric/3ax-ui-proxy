package service

import (
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// monSetTarget stores or replaces the state of one target.
func monSetTarget(t *testing.T, client string, inboundId int, path, state string) {
	t.Helper()
	db := database.GetDB()
	db.Where("mon_client_id = ? AND inbound_kind = ? AND inbound_id = ? AND path = ?", client, "xray", inboundId, path).Delete(&model.MonTarget{})
	if err := db.Create(&model.MonTarget{MonClientId: client, InboundKind: "xray", InboundId: inboundId, Path: path, State: state}).Error; err != nil {
		t.Fatal(err)
	}
}

// TestMonitoringStandbyHint (proxy-chain.md §6.5): the DOWN alert of the
// active edge lists the standby edges and proposes /proxy <name> for the best
// — every inbound UP over a strict majority UP over no data, ties by name,
// never an edge with nothing UP and something DOWN; with no candidate it says
// so. Any other path's DOWN is an ordinary alert.
func TestMonitoringStandbyHint(t *testing.T) {
	newMonitoringTestService(t)
	initTestBotLocale(t, "en-US")
	for id := 1; id <= 3; id++ {
		monInbound(t, id, model.VLESS, true)
	}
	monRegister(t, MonClient{Id: "ams-1", Name: "Amsterdam", State: "ONLINE"}, MonClient{Id: "msk-1", Name: "Moscow", State: "ONLINE"})
	monHop(t, "core-1", "inner", "joined", 0, false, "10.0.0.7")
	monHop(t, "edge-a", "edge", "joined", 0, true, "a.example.net")
	monHop(t, "edge-b", "edge", "joined", 0, false, "b.example.net")
	monHop(t, "edge-c", "edge", "legacy", 0, false, "c.example.net")
	monHop(t, "edge-d", "edge", "joined", 0, false, "d.example.net")
	monHop(t, "edge-p", "edge", "pending", 0, false, "p.example.net")

	// edge-b: 2 of 3 inbounds UP; edge-c: all UP; edge-d: DOWN only. A DOWN
	// of a mon-client outside the registry does not count.
	monSetTarget(t, "ams-1", 1, "edge:edge-b", "UP")
	monSetTarget(t, "ams-1", 2, "edge:edge-b", "UP")
	monSetTarget(t, "ams-1", 3, "edge:edge-b", "DOWN")
	monSetTarget(t, "ams-1", 1, "edge:edge-c", "UP")
	monSetTarget(t, "msk-1", 2, "edge:edge-c", "UP")
	monSetTarget(t, "gone-1", 2, "edge:edge-c", "DOWN")
	monSetTarget(t, "ams-1", 1, "edge:edge-d", "DOWN")

	bot, _ := newTestBot(t)
	down := func(path string) string {
		ev := monEventRow(model.MonEventKindTarget, "ams-1", "xray", 1, path, "UP", model.MonStateDown, "tls_timeout", 1)
		return bot.monitoringEventMessage(&ev)
	}
	check := func(label, path, want string) {
		t.Helper()
		msg := down(path)
		if !strings.HasPrefix(msg, "⛔ DOWN") {
			t.Errorf("%s: %q is not a DOWN alert", label, msg)
		}
		if want == "" {
			if strings.Contains(msg, "Standby") || strings.Contains(msg, "standby") {
				t.Errorf("%s: %q carries a standby hint", label, msg)
			}
			return
		}
		if !strings.HasSuffix(msg, "\n"+want) {
			t.Errorf("%s: %q, want it to end with %q", label, msg, want)
		}
	}

	check("all UP wins", "edge:edge-a", "↪️ Standby: edge-b DOWN, edge-c UP, edge-d DOWN. Switch: /proxy edge-c")
	check("a standby's DOWN", "edge:edge-b", "")
	check("direct", "direct", "")
	check("an inner", "inner:core-1", "")

	// edge-c at 1 of 2 is a tie, which is no majority: edge-b's 2 of 3 wins.
	monSetTarget(t, "msk-1", 2, "edge:edge-c", "FLAPPING")
	check("strict majority", "edge:edge-a", "↪️ Standby: edge-b DOWN, edge-c FLAPPING, edge-d DOWN. Switch: /proxy edge-b")

	// No majority anywhere: an edge with no data is still proposed, by name.
	database.GetDB().Where("path = ?", "edge:edge-c").Delete(&model.MonTarget{})
	monSetTarget(t, "ams-1", 1, "edge:edge-b", "DOWN")
	check("no data", "edge:edge-a", "↪️ Standby: edge-b DOWN, edge-c UNKNOWN, edge-d DOWN. Switch: /proxy edge-b")

	// Everything down: no candidate.
	monSetTarget(t, "ams-1", 2, "edge:edge-b", "DOWN")
	monSetTarget(t, "ams-1", 1, "edge:edge-c", "DOWN")
	check("all down", "edge:edge-a", "↪️ Standby: edge-b DOWN, edge-c DOWN, edge-d DOWN. No healthy standby edge.")

	// No standby at all.
	database.GetDB().Where("name IN ?", []string{"edge-b", "edge-c", "edge-d"}).Delete(&model.ChainHop{})
	check("no standby", "edge:edge-a", "↪️ No healthy standby edge.")
}
