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
	direct := svc.ProbeLink(ib, "probe-12", "203.0.113.10", "")
	if !strings.HasPrefix(direct, "vless://aaaaaaaa-0000-0000-0000-000000000001@203.0.113.10:443") {
		t.Errorf("direct link = %q", direct)
	}
	proxy := svc.ProbeLink(ib, "probe-12", "203.0.113.10", "front.example.net")
	if !strings.HasPrefix(proxy, "vless://aaaaaaaa-0000-0000-0000-000000000001@front.example.net:443") {
		t.Errorf("proxy link = %q", proxy)
	}
	if svc.address != "" || svc.overrideOn {
		t.Error("ProbeLink wrote to the shared service")
	}
}

// TestProbeLinkIgnoresExternalProxy: the probe link is rendered without the
// inbound's externalProxy, so direct and proxy point at different places (the
// caller's host and the override host) instead of both at ep.dest, and the
// link is a single line even when externalProxy lists several endpoints.
func TestProbeLinkIgnoresExternalProxy(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	for key, value := range map[string]string{"proxyOverrideEnable": "true", "proxyOverrideHost": "front.example.net"} {
		if err := db.Create(&model.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	externalProxy := `"externalProxy":[{"forceTls":"same","dest":"cdn.example.com","port":8443,"remark":"cdn"},{"forceTls":"same","dest":"cdn2.example.com","port":2053,"remark":"cdn2"}]`
	inbounds := []*model.Inbound{
		{Id: 21, Port: 443, Protocol: model.VLESS, Tag: "in-21", Remark: "vless", Enable: true,
			Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000021","email":"probe-21","enable":true}],"decryption":"none"}`,
			StreamSettings: `{"network":"tcp","security":"none",` + externalProxy + `}`, Sniffing: "{}"},
		{Id: 22, Port: 444, Protocol: model.VMESS, Tag: "in-22", Remark: "vmess", Enable: true,
			Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000022","email":"probe-22","enable":true}]}`,
			StreamSettings: `{"network":"tcp","security":"none",` + externalProxy + `}`, Sniffing: "{}"},
		{Id: 23, Port: 445, Protocol: model.Trojan, Tag: "in-23", Remark: "trojan", Enable: true,
			Settings:       `{"clients":[{"password":"pw23","email":"probe-23","enable":true}]}`,
			StreamSettings: `{"network":"tcp","security":"none",` + externalProxy + `}`, Sniffing: "{}"},
		{Id: 24, Port: 446, Protocol: model.Shadowsocks, Tag: "in-24", Remark: "ss", Enable: true,
			Settings:       `{"method":"aes-256-gcm","password":"","network":"tcp,udp","clients":[{"password":"pw24","method":"aes-256-gcm","email":"probe-24","enable":true}]}`,
			StreamSettings: `{"network":"tcp","security":"none",` + externalProxy + `}`, Sniffing: "{}"},
	}
	svc := NewSubService(false, "-ieo", "")
	for _, ib := range inbounds {
		if err := db.Create(ib).Error; err != nil {
			t.Fatal(err)
		}
		email := "probe-" + ib.Tag[len("in-"):]
		direct := svc.ProbeLink(ib, email, "203.0.113.10", "")
		proxy := svc.ProbeLink(ib, email, "203.0.113.10", "front.example.net")
		for name, link := range map[string]string{"direct": direct, "proxy": proxy} {
			if link == "" || strings.Contains(link, "\n") {
				t.Errorf("%s %s link is not one line: %q", ib.Protocol, name, link)
			}
			if strings.Contains(link, "cdn.example.com") || strings.Contains(link, "cdn2.example.com") {
				t.Errorf("%s %s link carries externalProxy: %q", ib.Protocol, name, link)
			}
		}
		if direct == proxy {
			t.Errorf("%s: direct and proxy links are the same: %q", ib.Protocol, direct)
		}
		if ib.Protocol == model.VMESS {
			continue // base64 body; the address checks below read plain URLs
		}
		if !strings.Contains(direct, "@203.0.113.10:") {
			t.Errorf("%s direct link = %q, want the caller's host", ib.Protocol, direct)
		}
		if !strings.Contains(proxy, "@front.example.net:") {
			t.Errorf("%s proxy link = %q, want the override host", ib.Protocol, proxy)
		}
	}
}

// TestProbeLinkDirectUsesPublicListen: an inbound bound to one public address
// is reachable there only, so the direct path dials Listen, not the caller's
// host; the proxy path still dials the override host.
func TestProbeLinkDirectUsesPublicListen(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	db := database.GetDB()
	for key, value := range map[string]string{"proxyOverrideEnable": "true", "proxyOverrideHost": "front.example.net"} {
		if err := db.Create(&model.Setting{Key: key, Value: value}).Error; err != nil {
			t.Fatal(err)
		}
	}
	ib := &model.Inbound{Id: 31, Listen: "198.51.100.7", Port: 443, Protocol: model.VLESS, Tag: "in-31", Remark: "listen", Enable: true,
		Settings:       `{"clients":[{"id":"aaaaaaaa-0000-0000-0000-000000000031","email":"probe-31","enable":true}],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none","externalProxy":[{"forceTls":"same","dest":"cdn.example.com","port":8443,"remark":"cdn"}]}`, Sniffing: "{}"}
	if err := db.Create(ib).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewSubService(false, "-ieo", "")
	if direct := svc.ProbeLink(ib, "probe-31", "203.0.113.10", ""); !strings.HasPrefix(direct, "vless://aaaaaaaa-0000-0000-0000-000000000031@198.51.100.7:443") {
		t.Errorf("direct link = %q, want the public Listen", direct)
	}
	if proxy := svc.ProbeLink(ib, "probe-31", "203.0.113.10", "front.example.net"); !strings.HasPrefix(proxy, "vless://aaaaaaaa-0000-0000-0000-000000000031@front.example.net:443") {
		t.Errorf("proxy link = %q, want the override host", proxy)
	}
	// A hop's path dials the hop, not the public Listen: the chain relays the
	// same port one-to-one (proxy-chain.md §6.1).
	if hop := svc.ProbeLink(ib, "probe-31", "203.0.113.10", "10.0.0.7"); !strings.HasPrefix(hop, "vless://aaaaaaaa-0000-0000-0000-000000000031@10.0.0.7:443") {
		t.Errorf("hop link = %q, want the hop host", hop)
	}
}
