package job

import (
	"encoding/json"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/coinman-dev/3ax-ui/v2/xray"
)

// TestLdapAutoDeleteKeepsProbeAccount: with auto-delete on, the sync removes
// every client the directory does not list — which would include the
// monitoring probe of an LDAP-managed inbound, since it is never in LDAP.
func TestLdapAutoDeleteKeepsProbeAccount(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()

	clients := []model.Client{
		{ID: "aaaaaaaa-0000-0000-0000-000000000001", Email: "alice", Enable: false},
		{ID: "aaaaaaaa-0000-0000-0000-000000000002", Email: "gone", Enable: false},
		{ID: "aaaaaaaa-0000-0000-0000-000000000003", Email: service.ProbeXrayEmail(1), Enable: false},
	}
	settings, _ := json.Marshal(map[string]any{"clients": clients, "decryption": "none"})
	if err := db.Create(&model.Inbound{Id: 1, Port: 10001, Protocol: model.VLESS, Tag: "ldap-in", Remark: "ldap",
		Settings: string(settings), StreamSettings: "{}", Sniffing: "{}", Enable: false}).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		if err := db.Create(&xray.ClientTraffic{InboundId: 1, Email: c.Email, Enable: true}).Error; err != nil {
			t.Fatal(err)
		}
	}

	j := NewLdapSyncJob()
	j.deleteClientsNotInLDAP("ldap-in", map[string]struct{}{"alice": {}})

	ib, err := j.inboundService.GetInbound(1)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := j.inboundService.GetClients(ib)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range remaining {
		got[c.Email] = true
	}
	if !got["alice"] || !got["probe-1"] || got["gone"] || len(got) != 2 {
		t.Errorf("clients after sync: %v, want alice and probe-1 only", got)
	}
}
