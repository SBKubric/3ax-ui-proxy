package sub

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// TestProbeLinksRenderLikeTheSubscription: one link per enabled inbound
// holding a probe account, the address taken from the host parameter on the
// direct path and from the override on the proxy path, disabled inbounds and
// inbounds without a probe left out, and the renderer registered for the
// monitoring service.
func TestProbeLinksRenderLikeTheSubscription(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	seed := func(tag string, enable bool, port int, probe bool) *model.Inbound {
		clients := []model.Client{{ID: "22222222-2222-2222-2222-222222222222", Email: "alice-" + tag, Enable: true}}
		ib := &model.Inbound{UserId: 1, Remark: tag, Enable: enable, Port: port, Protocol: model.VLESS, Tag: tag,
			StreamSettings: `{"network":"tcp","security":"none"}`}
		if err := db.Create(ib).Error; err != nil {
			t.Fatal(err)
		}
		if probe {
			clients = append(clients, model.Client{ID: "11111111-1111-1111-1111-111111111111",
				Email: service.ProbeEmail(model.MonKindXray, ib.Id), Enable: true, SubID: "probesubid000001"})
		}
		raw, _ := json.Marshal(map[string]any{"clients": clients, "decryption": "none"})
		if err := db.Model(ib).Update("settings", string(raw)).Error; err != nil {
			t.Fatal(err)
		}
		return ib
	}
	live := seed("live", true, 443, true)
	seed("paused", false, 8443, true)
	seed("no-probe", true, 9443, false)

	links, err := ProbeLinks("203.0.113.10", false)
	if err != nil {
		t.Fatalf("ProbeLinks direct: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("want one link (enabled inbound with a probe), got %v", links)
	}
	link := links[live.Id]
	if !strings.HasPrefix(link, "vless://11111111-1111-1111-1111-111111111111@203.0.113.10:443?") {
		t.Errorf("direct link address: %s", link)
	}
	if !strings.Contains(link, "probe-"+itoa(live.Id)) {
		t.Errorf("link remark should name the probe: %s", link)
	}

	settings := &service.SettingService{}
	if err := settings.SetProxyOverrideEnable(true); err != nil {
		t.Fatal(err)
	}
	if err := settings.SetProxyOverrideHost("front.example.net"); err != nil {
		t.Fatal(err)
	}
	proxied, err := ProbeLinks("", true)
	if err != nil {
		t.Fatalf("ProbeLinks proxy: %v", err)
	}
	if !strings.HasPrefix(proxied[live.Id], "vless://11111111-1111-1111-1111-111111111111@front.example.net:443?") {
		t.Errorf("proxy link address: %s", proxied[live.Id])
	}
	// The override is ignored on the direct path even when it is on.
	direct, err := ProbeLinks("203.0.113.10", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(direct[live.Id], "@203.0.113.10:443") {
		t.Errorf("direct link with the override on: %s", direct[live.Id])
	}

	// The service reaches this renderer through the init registration.
	if err := settings.SetMonProbeSubId("probesubid000001"); err != nil {
		t.Fatal(err)
	}
	configs, err := (&service.MonitoringService{}).ProbeConfigs("203.0.113.10")
	if err != nil {
		t.Fatalf("ProbeConfigs through the registered renderer: %v", err)
	}
	if len(configs.Items) != 1 || configs.Items[0].Link != direct[live.Id] || configs.Path != "direct" {
		t.Errorf("ProbeConfigs items: %+v", configs.Items)
	}
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
