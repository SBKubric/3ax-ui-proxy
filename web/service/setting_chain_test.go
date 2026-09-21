package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
)

func newChainSettingService(t *testing.T) *SettingService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	return &SettingService{}
}

// The defaults of spec §2.2 on a database that has never seen a settings save.
func TestChainSettingsDefaults(t *testing.T) {
	s := newChainSettingService(t)

	revision, err := s.GetChainRevision()
	if err != nil || revision != 0 {
		t.Errorf("GetChainRevision = %d, %v; want 0, nil", revision, err)
	}
	poll, err := s.GetChainPollSeconds()
	if err != nil || poll != 30 {
		t.Errorf("GetChainPollSeconds = %d, %v; want 30, nil", poll, err)
	}
	stale, err := s.GetChainStaleMinutes()
	if err != nil || stale != 60 {
		t.Errorf("GetChainStaleMinutes = %d, %v; want 60, nil", stale, err)
	}
	hours, err := s.GetChainJoinTokenHours()
	if err != nil || hours != 24 {
		t.Errorf("GetChainJoinTokenHours = %d, %v; want 24, nil", hours, err)
	}
	ports, err := s.GetChainExtraPorts()
	if err != nil || len(ports) != 0 {
		t.Errorf("GetChainExtraPorts = %v, %v; want empty, nil", ports, err)
	}

	all, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.ChainExtraPorts != "[]" || all.ChainPollSeconds != 30 ||
		all.ChainStaleMinutes != 60 || all.ChainJoinTokenHours != 24 {
		t.Errorf("AllSetting chain preferences: %+v", struct {
			Extra             string
			Poll, Stale, Join int
		}{all.ChainExtraPorts, all.ChainPollSeconds, all.ChainStaleMinutes, all.ChainJoinTokenHours})
	}
}

// chainRevision is state, not a preference: it must not travel through the
// settings form, or saving the form would roll the registry's revision back
// and every box would stop seeing changes (§2.2).
func TestChainRevisionIsNotAFormField(t *testing.T) {
	s := newChainSettingService(t)
	if err := s.SetChainRevision(42); err != nil {
		t.Fatalf("SetChainRevision: %v", err)
	}

	all, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if err := s.UpdateAllSetting(all); err != nil {
		t.Fatalf("UpdateAllSetting: %v", err)
	}

	revision, err := s.GetChainRevision()
	if err != nil || revision != 42 {
		t.Fatalf("GetChainRevision after a form save = %d, %v; want 42", revision, err)
	}
}

func TestChainExtraPortsRoundTrip(t *testing.T) {
	s := newChainSettingService(t)

	want := []ChainExtraPort{
		{Port: 8080, Network: "tcp", Note: "web"},
		{Port: 51821, Network: "udp"},
		{Port: 443, Network: "tcp,udp", Note: "both"},
	}
	if err := s.SetChainExtraPorts(want); err != nil {
		t.Fatalf("SetChainExtraPorts: %v", err)
	}

	got, err := s.GetChainExtraPorts()
	if err != nil {
		t.Fatalf("GetChainExtraPorts: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d ports, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("port %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// The stored form is the JSON the settings form edits by hand, so its
	// field names are part of the contract.
	raw, err := s.GetChainExtraPortsRaw()
	if err != nil {
		t.Fatalf("GetChainExtraPortsRaw: %v", err)
	}
	if !strings.Contains(raw, `"port":8080`) || !strings.Contains(raw, `"network":"tcp"`) ||
		!strings.Contains(raw, `"note":"web"`) {
		t.Errorf("stored JSON = %s", raw)
	}

	if err := s.SetChainExtraPorts(nil); err != nil {
		t.Fatalf("SetChainExtraPorts(nil): %v", err)
	}
	if raw, _ := s.GetChainExtraPortsRaw(); raw != "[]" {
		t.Errorf("empty list stored as %q, want []", raw)
	}
}

func TestChainExtraPortsValidation(t *testing.T) {
	s := newChainSettingService(t)

	tests := []struct {
		name  string
		ports []ChainExtraPort
		ok    bool
	}{
		{"tcp", []ChainExtraPort{{Port: 1, Network: "tcp"}}, true},
		{"udp", []ChainExtraPort{{Port: 65535, Network: "udp"}}, true},
		{"both", []ChainExtraPort{{Port: 443, Network: "tcp,udp"}}, true},
		{"port zero", []ChainExtraPort{{Port: 0, Network: "tcp"}}, false},
		{"port negative", []ChainExtraPort{{Port: -1, Network: "tcp"}}, false},
		{"port too large", []ChainExtraPort{{Port: 65536, Network: "tcp"}}, false},
		{"network empty", []ChainExtraPort{{Port: 80}}, false},
		{"network unknown", []ChainExtraPort{{Port: 80, Network: "sctp"}}, false},
		{"network reversed", []ChainExtraPort{{Port: 80, Network: "udp,tcp"}}, false},
		{"network uppercase", []ChainExtraPort{{Port: 80, Network: "TCP"}}, false},
		{"one bad among good", []ChainExtraPort{{Port: 80, Network: "tcp"}, {Port: 0, Network: "udp"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.SetChainExtraPorts(tt.ports)
			if tt.ok && err != nil {
				t.Fatalf("SetChainExtraPorts(%+v) = %v, want nil", tt.ports, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("SetChainExtraPorts(%+v) was accepted", tt.ports)
			}
		})
	}

	// A hand-edited value that does not parse must surface as an error rather
	// than as an empty port list that silently drops the operator's ports.
	setSetting(t, "chainExtraPorts", `[{"port":"eighty"}]`)
	if _, err := s.GetChainExtraPorts(); err == nil {
		t.Fatal("malformed chainExtraPorts was accepted")
	}
	setSetting(t, "chainExtraPorts", `[{"port":70000,"network":"tcp"}]`)
	if _, err := s.GetChainExtraPorts(); err == nil {
		t.Fatal("out-of-range port in stored JSON was accepted")
	}
}

func TestChainScalarSettersRoundTrip(t *testing.T) {
	s := newChainSettingService(t)

	if err := s.SetChainRevision(7); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetChainRevision(); got != 7 {
		t.Errorf("revision = %d, want 7", got)
	}
	if err := s.SetChainPollSeconds(15); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetChainPollSeconds(); got != 15 {
		t.Errorf("pollSeconds = %d, want 15", got)
	}
	if err := s.SetChainStaleMinutes(120); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetChainStaleMinutes(); got != 120 {
		t.Errorf("staleMinutes = %d, want 120", got)
	}
	if err := s.SetChainJoinTokenHours(1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetChainJoinTokenHours(); got != 1 {
		t.Errorf("joinTokenHours = %d, want 1", got)
	}
}
