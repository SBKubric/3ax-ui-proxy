package job

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// TestLdapAutoDeleteKeepsProbeAccounts: LDAP knows nothing about probe
// accounts, so the auto-delete pass must skip them instead of erasing the
// monitoring probe from every LDAP-managed inbound on each sync.
func TestLdapAutoDeleteKeepsProbeAccounts(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	inboundService := &service.InboundService{}
	probe := model.Client{ID: "11111111-1111-1111-1111-111111111111", Email: "probe-1", Enable: true}
	// Disabled so the deletion does not need the xray API (which is not running here).
	gone := model.Client{ID: "22222222-2222-2222-2222-222222222222", Email: "left-company", Enable: false}
	settings, _ := json.Marshal(map[string]any{"clients": []model.Client{probe, gone}})
	ib := &model.Inbound{UserId: 1, Remark: "ldap", Enable: true, Port: 10443, Protocol: model.VLESS, Tag: "ldap-inbound",
		Settings: string(settings), StreamSettings: `{"network":"tcp","security":"none"}`}
	if err := db.Create(ib).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range []model.Client{probe, gone} {
		if err := inboundService.AddClientStat(db, ib.Id, &c); err != nil {
			t.Fatal(err)
		}
	}

	j := NewLdapSyncJob()
	j.deleteClientsNotInLDAP("ldap-inbound", map[string]struct{}{})

	after, err := inboundService.GetInbound(ib.Id)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := inboundService.GetClients(after)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].Email != "probe-1" {
		t.Fatalf("clients after LDAP auto-delete: %+v, want only the probe", clients)
	}
}
