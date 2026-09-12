package sub

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// vlessUserEncryption runs the JSON subscription for one VLESS inbound stored
// with the given settings and returns the "encryption" the outbound's user
// carries. Xray-core 26 refuses a VLESS outbound whose user has no
// encryption ("" included), so this is what a client actually starts with.
func vlessUserEncryption(t *testing.T, settings string) string {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	resetInboundsCache()
	ib := &model.Inbound{
		UserId: 1, Remark: "vless", Enable: true, Port: 443, Listen: "",
		Protocol: "vless", Tag: "inbound-443",
		Settings:       settings,
		StreamSettings: `{"network":"tcp","security":"none","tcpSettings":{}}`,
		Sniffing:       `{"enabled":false}`,
	}
	if err := database.GetDB().Create(ib).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}

	svc := NewSubJsonService("", "", "", "", NewSubService(false, "-ieo", ""))
	out, _, err := svc.GetJson("sub-enc", "example.com")
	if err != nil {
		t.Fatalf("GetJson: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("GetJson returned no config for the subscription")
	}

	var cfg struct {
		Outbounds []struct {
			Protocol string `json:"protocol"`
			Settings struct {
				Vnext []struct {
					Users []map[string]any `json:"users"`
				} `json:"vnext"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("subscription is not a single JSON config: %v\n%s", err, out)
	}
	for _, ob := range cfg.Outbounds {
		if ob.Protocol != "vless" {
			continue
		}
		if len(ob.Settings.Vnext) != 1 || len(ob.Settings.Vnext[0].Users) != 1 {
			t.Fatalf("vless outbound has no single vnext/user: %s", out)
		}
		enc, _ := ob.Settings.Vnext[0].Users[0]["encryption"].(string)
		return enc
	}
	t.Fatalf("no vless outbound in subscription:\n%s", out)
	return ""
}

const vlessClientJSON = `{"id":"a5899a9e-dc44-4ec4-bba1-72b6e29bd2d1","email":"stand-client","subId":"sub-enc","enable":true,"flow":"xtls-rprx-vision"}`

// TestJsonSubVlessUserDefaultsEncryptionToNone: an inbound saved without an
// "encryption" key (the stand's, and any created before the panel learned the
// field) must still hand out a config Xray accepts.
func TestJsonSubVlessUserDefaultsEncryptionToNone(t *testing.T) {
	got := vlessUserEncryption(t, `{"clients":[`+vlessClientJSON+`],"decryption":"none"}`)
	if got != "none" {
		t.Fatalf("users[0].encryption = %q, want %q", got, "none")
	}
}

// TestJsonSubVlessUserKeepsConfiguredEncryption: when the inbound carries a
// real VLESS encryption string, the client must get exactly that, not "none".
func TestJsonSubVlessUserKeepsConfiguredEncryption(t *testing.T) {
	const enc = "mlkem768x25519plus.native.600s.dGVzdC1jbGllbnQta2V5"
	got := vlessUserEncryption(t, `{"clients":[`+vlessClientJSON+`],"decryption":"none","encryption":"`+enc+`"}`)
	if got != enc {
		t.Fatalf("users[0].encryption = %q, want %q", got, enc)
	}
}
