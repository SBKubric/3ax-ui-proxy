package service

import (
	"path/filepath"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
)

func newMonitoringSettingService(t *testing.T) *SettingService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	return &SettingService{}
}

// TestMonitoringSettingsDefaults pins the defaults of spec §2.2 for all
// eleven keys on a database that has never seen a settings save.
func TestMonitoringSettingsDefaults(t *testing.T) {
	s := newMonitoringSettingService(t)

	all, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if all.MonEnable || all.MonToken != "" || all.MonStaleMinutes != 15 || all.MonProbeTtlHours != 24 ||
		all.MonRetentionDays != 7 || all.MonRollupRetentionDays != 30 || all.MonRollupStepMinutes != 60 ||
		all.MonProbePeerLimit != 32 {
		t.Errorf("AllSetting monitoring defaults: %+v", struct {
			Enable                                             bool
			Token                                              string
			Stale, Ttl, Retention, RollupRetention, RollupStep int
		}{all.MonEnable, all.MonToken, all.MonStaleMinutes, all.MonProbeTtlHours, all.MonRetentionDays, all.MonRollupRetentionDays, all.MonRollupStepMinutes})
	}

	subId, _ := s.GetMonProbeSubId()
	lastEnsured, _ := s.GetMonProbeLastEnsured()
	lastContact, _ := s.GetMonLastContact()
	snapshot, _ := s.GetMonClientsSnapshot()
	if subId != "" || lastEnsured != 0 || lastContact != 0 || snapshot != "[]" {
		t.Errorf("state defaults: subId=%q lastEnsured=%d lastContact=%d snapshot=%q", subId, lastEnsured, lastContact, snapshot)
	}
}

// TestMonitoringSettingsRoundTrip writes every key and reads it back: the
// seven preferences through UpdateAllSetting/GetAllSetting and the typed
// accessors, the four state keys through their accessors only.
func TestMonitoringSettingsRoundTrip(t *testing.T) {
	s := newMonitoringSettingService(t)

	all, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	all.MonEnable = true
	all.MonToken = "tok-0123456789abcdef0123456789ab"
	all.MonStaleMinutes = 5
	all.MonProbeTtlHours = 48
	all.MonRetentionDays = 3
	all.MonRollupRetentionDays = 60
	all.MonRollupStepMinutes = 30
	all.MonProbePeerLimit = 0
	if err := s.UpdateAllSetting(all); err != nil {
		t.Fatalf("UpdateAllSetting: %v", err)
	}

	got, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}
	if !got.MonEnable || got.MonToken != all.MonToken || got.MonStaleMinutes != 5 || got.MonProbeTtlHours != 48 ||
		got.MonRetentionDays != 3 || got.MonRollupRetentionDays != 60 || got.MonRollupStepMinutes != 30 ||
		got.MonProbePeerLimit != 0 {
		t.Errorf("AllSetting round-trip lost a monitoring field: %+v", got)
	}

	checkBool := func(name string, get func() (bool, error), want bool) {
		v, err := get()
		if err != nil || v != want {
			t.Errorf("%s = %v, %v; want %v", name, v, err, want)
		}
	}
	checkInt := func(name string, get func() (int, error), want int) {
		v, err := get()
		if err != nil || v != want {
			t.Errorf("%s = %v, %v; want %v", name, v, err, want)
		}
	}
	checkInt64 := func(name string, get func() (int64, error), want int64) {
		v, err := get()
		if err != nil || v != want {
			t.Errorf("%s = %v, %v; want %v", name, v, err, want)
		}
	}
	checkString := func(name string, get func() (string, error), want string) {
		v, err := get()
		if err != nil || v != want {
			t.Errorf("%s = %q, %v; want %q", name, v, err, want)
		}
	}

	checkBool("GetMonEnable", s.GetMonEnable, true)
	checkString("GetMonToken", s.GetMonToken, all.MonToken)
	checkInt("GetMonStaleMinutes", s.GetMonStaleMinutes, 5)
	checkInt("GetMonProbeTtlHours", s.GetMonProbeTtlHours, 48)
	checkInt("GetMonRetentionDays", s.GetMonRetentionDays, 3)
	checkInt("GetMonRollupRetentionDays", s.GetMonRollupRetentionDays, 60)
	checkInt("GetMonRollupStepMinutes", s.GetMonRollupStepMinutes, 30)
	checkInt("GetMonProbePeerLimit", s.GetMonProbePeerLimit, 0)

	// The typed setters take the same path the form does.
	if err := s.SetMonEnable(false); err != nil {
		t.Fatalf("SetMonEnable: %v", err)
	}
	if err := s.SetMonStaleMinutes(20); err != nil {
		t.Fatalf("SetMonStaleMinutes: %v", err)
	}
	if err := s.SetMonProbeTtlHours(12); err != nil {
		t.Fatalf("SetMonProbeTtlHours: %v", err)
	}
	if err := s.SetMonRetentionDays(14); err != nil {
		t.Fatalf("SetMonRetentionDays: %v", err)
	}
	if err := s.SetMonRollupRetentionDays(90); err != nil {
		t.Fatalf("SetMonRollupRetentionDays: %v", err)
	}
	if err := s.SetMonRollupStepMinutes(15); err != nil {
		t.Fatalf("SetMonRollupStepMinutes: %v", err)
	}
	if err := s.SetMonProbePeerLimit(8); err != nil {
		t.Fatalf("SetMonProbePeerLimit: %v", err)
	}
	if err := s.SetMonToken("other"); err != nil {
		t.Fatalf("SetMonToken: %v", err)
	}
	got, _ = s.GetAllSetting()
	if got.MonEnable || got.MonStaleMinutes != 20 || got.MonProbeTtlHours != 12 || got.MonRetentionDays != 14 ||
		got.MonRollupRetentionDays != 90 || got.MonRollupStepMinutes != 15 || got.MonToken != "other" || got.MonProbePeerLimit != 8 {
		t.Errorf("typed setters not visible through AllSetting: %+v", got)
	}

	// State keys: millisecond timestamps must survive as int64.
	const ms = int64(1757721540000)
	if err := s.SetMonProbeSubId("probe-sub-1"); err != nil {
		t.Fatalf("SetMonProbeSubId: %v", err)
	}
	if err := s.SetMonProbeLastEnsured(ms); err != nil {
		t.Fatalf("SetMonProbeLastEnsured: %v", err)
	}
	if err := s.SetMonLastContact(ms + 1); err != nil {
		t.Fatalf("SetMonLastContact: %v", err)
	}
	if err := s.SetMonClientsSnapshot(`[{"monClientId":"ams-1","state":"ONLINE"}]`); err != nil {
		t.Fatalf("SetMonClientsSnapshot: %v", err)
	}
	checkString("GetMonProbeSubId", s.GetMonProbeSubId, "probe-sub-1")
	checkInt64("GetMonProbeLastEnsured", s.GetMonProbeLastEnsured, ms)
	checkInt64("GetMonLastContact", s.GetMonLastContact, ms+1)
	checkString("GetMonClientsSnapshot", s.GetMonClientsSnapshot, `[{"monClientId":"ams-1","state":"ONLINE"}]`)
}

// TestUpdateAllSettingKeepsMonitoringState is why the four state keys are
// not AllSetting fields: the settings form loads a snapshot, mon-server keeps
// talking, and a later Save must not push the stale snapshot back over
// monLastContact (a rollback there would fake a STALE), the probe subId, or
// the registry cache.
func TestUpdateAllSettingKeepsMonitoringState(t *testing.T) {
	s := newMonitoringSettingService(t)

	loaded, err := s.GetAllSetting()
	if err != nil {
		t.Fatalf("GetAllSetting: %v", err)
	}

	// mon-server talks to the panel while the form is open.
	const ms = int64(1757721540000)
	if err := s.SetMonProbeSubId("probe-sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMonProbeLastEnsured(ms); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMonLastContact(ms); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMonClientsSnapshot(`[{"monClientId":"ams-1"}]`); err != nil {
		t.Fatal(err)
	}

	loaded.MonStaleMinutes = 30
	if err := s.UpdateAllSetting(loaded); err != nil {
		t.Fatalf("UpdateAllSetting: %v", err)
	}

	subId, _ := s.GetMonProbeSubId()
	lastEnsured, _ := s.GetMonProbeLastEnsured()
	lastContact, _ := s.GetMonLastContact()
	snapshot, _ := s.GetMonClientsSnapshot()
	if subId != "probe-sub-1" || lastEnsured != ms || lastContact != ms || snapshot != `[{"monClientId":"ams-1"}]` {
		t.Errorf("settings save rolled monitoring state back: subId=%q lastEnsured=%d lastContact=%d snapshot=%q",
			subId, lastEnsured, lastContact, snapshot)
	}
	if v, _ := s.GetMonStaleMinutes(); v != 30 {
		t.Errorf("monStaleMinutes = %d after save, want 30", v)
	}
}

// TestResetMonToken checks the token is issued like the panel secret — 32
// characters — and that a second reset really replaces it.
func TestResetMonToken(t *testing.T) {
	s := newMonitoringSettingService(t)

	first, err := s.ResetMonToken()
	if err != nil {
		t.Fatalf("ResetMonToken: %v", err)
	}
	if len(first) != 32 {
		t.Errorf("token length = %d, want 32: %q", len(first), first)
	}
	if stored, _ := s.GetMonToken(); stored != first {
		t.Errorf("GetMonToken = %q after reset, want %q", stored, first)
	}

	second, err := s.ResetMonToken()
	if err != nil {
		t.Fatalf("second ResetMonToken: %v", err)
	}
	if len(second) != 32 || second == first {
		t.Errorf("second token = %q, want a different 32-character token than %q", second, first)
	}
	if stored, _ := s.GetMonToken(); stored != second {
		t.Errorf("GetMonToken = %q after second reset, want %q", stored, second)
	}
}
