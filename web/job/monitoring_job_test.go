package job

import "testing"

// TestMonitoringJobRunsOnAnEmptyPanel: both cadences must run to completion
// on a panel with no inbounds, no probe set and no monitoring data — the
// state every install starts in.
func TestMonitoringJobRunsOnAnEmptyPanel(t *testing.T) {
	setupIntegrationDB(t)
	NewMonitoringJob(MonitoringEveryMinute).Run()
	NewMonitoringJob(MonitoringHourly).Run()
}
