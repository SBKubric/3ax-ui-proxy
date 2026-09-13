package service

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
)

// TestMonitoringSettingsRoundTrip: all eleven monitoring keys default as the
// spec table says, survive UpdateAllSetting → GetAllSetting unchanged, and the
// typed getters read back what the settings page wrote.
func TestMonitoringSettingsRoundTrip(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := &SettingService{}

	all, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.MonEnable || all.MonToken != "" || all.MonStaleMinutes != 15 || all.MonProbeSubId != "" ||
		all.MonProbeLastEnsured != 0 || all.MonProbeTtlHours != 24 || all.MonLastContact != 0 ||
		all.MonClientsSnapshot != "[]" || all.MonRetentionDays != 7 || all.MonRollupRetentionDays != 30 ||
		all.MonRollupStepMinutes != 60 {
		t.Fatalf("defaults differ from the spec table: %+v", all)
	}

	all.MonEnable = true
	all.MonToken = "tok"
	all.MonStaleMinutes = 5
	all.MonProbeSubId = "k3j9d8s7f6g5h4j3"
	all.MonProbeLastEnsured = 1757721540000
	all.MonProbeTtlHours = 48
	all.MonLastContact = 1757721600000
	all.MonClientsSnapshot = `[{"id":"ams-1"}]`
	all.MonRetentionDays = 3
	all.MonRollupRetentionDays = 60
	all.MonRollupStepMinutes = 30
	if err := s.UpdateAllSetting(all); err != nil {
		t.Fatalf("UpdateAllSetting: %v", err)
	}

	got, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting after save: %v", err)
	}
	if *got != *all {
		t.Fatalf("round trip changed the settings:\n got %+v\nwant %+v", got, all)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"monEnable", must(s.GetMonEnable()), true},
		{"monToken", must(s.GetMonToken()), "tok"},
		{"monStaleMinutes", must(s.GetMonStaleMinutes()), 5},
		{"monProbeSubId", must(s.GetMonProbeSubId()), "k3j9d8s7f6g5h4j3"},
		{"monProbeLastEnsured", must(s.GetMonProbeLastEnsured()), int64(1757721540000)},
		{"monProbeTtlHours", must(s.GetMonProbeTtlHours()), 48},
		{"monLastContact", must(s.GetMonLastContact()), int64(1757721600000)},
		{"monClientsSnapshot", must(s.GetMonClientsSnapshot()), `[{"id":"ams-1"}]`},
		{"monRetentionDays", must(s.GetMonRetentionDays()), 3},
		{"monRollupRetentionDays", must(s.GetMonRollupRetentionDays()), 60},
		{"monRollupStepMinutes", must(s.GetMonRollupStepMinutes()), 30},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: getter returned %v, want %v", c.name, c.got, c.want)
		}
	}

	// Setters are what mon-server's requests and the job go through.
	if err := s.SetMonLastContact(42); err != nil {
		t.Fatal(err)
	}
	if v := must(s.GetMonLastContact()); v != 42 {
		t.Errorf("SetMonLastContact: got %d", v)
	}
	if err := s.SetMonProbeSubId(""); err != nil {
		t.Fatal(err)
	}
	if v := must(s.GetMonProbeSubId()); v != "" {
		t.Errorf("SetMonProbeSubId(\"\"): got %q", v)
	}
}

// TestMonTokenGeneration: a token is 32 characters like the session secret,
// and regenerating stores a different one each time.
func TestMonTokenGeneration(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	s := &SettingService{}
	if got := GenerateMonToken(); len(got) != 32 {
		t.Fatalf("GenerateMonToken length %d, want 32", len(got))
	}
	first, err := s.RegenerateMonToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RegenerateMonToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("tokens %q / %q are not 32 characters", first, second)
	}
	if first == second {
		t.Fatalf("regenerate returned the same token twice: %q", first)
	}
	if stored := must(s.GetMonToken()); stored != second {
		t.Fatalf("stored token %q, want the last generated %q", stored, second)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
