package sub

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// TestProbeLinkChoosesTheAddress: the direct path puts the caller's host in
// the link even with the override on; the proxy path puts the override host.
func TestProbeLinkChoosesTheAddress(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	for key, value := range map[string]string{"proxyOverrideEnable": "true", "proxyOverrideHost": "front.example.net"} {
		if err := db.Create(&model.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	ib := &model.Inbound{Id: 12, Port: 443, Protocol: model.VLESS, Tag: "in-12", Remark: "Reality", Enable: true,
		Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000001","email":"probe-12","enable":true,"subId":"k3j9d8s7f6g5h4j3"}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`, Sniffing: "{}"}
	if err := db.Create(ib).Error; err != nil {
		t.Fatal(err)
	}

	svc := NewSubService(false, "-ieo", "")
	direct := svc.ProbeLink(ib, "probe-12", "203.0.113.10", false)
	if !strings.HasPrefix(direct, "vless://aaaaaaaa-0000-0000-0000-000000000001@203.0.113.10:443") {
		t.Errorf("direct link = %q", direct)
	}
	proxy := svc.ProbeLink(ib, "probe-12", "203.0.113.10", true)
	if !strings.HasPrefix(proxy, "vless://aaaaaaaa-0000-0000-0000-000000000001@front.example.net:443") {
		t.Errorf("proxy link = %q", proxy)
	}
	if svc.address != "" || svc.overrideOn {
		t.Error("ProbeLink wrote to the shared service")
	}
}
