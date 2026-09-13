package job

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// TestMonitoringJobsRunTheSweeps: the minute job raises STALE from the last
// contact and the settings; the hourly job removes rows past retention.
// The behaviour itself is covered in the service package; this checks the
// jobs are wired to it.
func TestMonitoringJobsRunTheSweeps(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	service.ResetMonRuntime()
	t.Cleanup(service.ResetMonRuntime)
	m := &service.MonitoringService{}

	NewMonitoringJob().Run()
	if stale, _ := m.Stale(); stale {
		t.Fatal("STALE without any contact")
	}
	m.TouchLastContact(time.Now().Add(-20 * time.Minute))
	NewMonitoringJob().Run()
	if stale, _ := m.Stale(); !stale {
		t.Fatal("the minute job did not raise STALE")
	}

	old := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	if err := database.GetDB().Create(&model.MonEvent{Id: "00000000-0000-7000-8000-000000000001", Ts: old, Kind: "panel", ToState: "PANEL_DOWN"}).Error; err != nil {
		t.Fatal(err)
	}
	NewMonitoringMaintenanceJob().Run()
	var n int64
	database.GetDB().Model(&model.MonEvent{}).Count(&n)
	if n != 0 {
		t.Fatalf("the hourly job left %d events past retention", n)
	}
}
