package job

import (
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// MonitoringJob is the panel's only monitoring computation: every minute it
// compares the last authorized mon-server request with monStaleMinutes and
// raises STALE (docs/spec/monitoring-panel.md §5). The first authorized
// request clears it again, in the contract's auth middleware.
type MonitoringJob struct {
	monitoringService service.MonitoringService
}

// NewMonitoringJob creates the minute job.
func NewMonitoringJob() *MonitoringJob {
	return new(MonitoringJob)
}

// Run raises STALE when the silence crossed the threshold.
func (j *MonitoringJob) Run() {
	if j.monitoringService.CheckStale(time.Now()) {
		logger.Warning("monitoring: no mon-server request within the STALE threshold")
	}
}

// MonitoringMaintenanceJob is the hourly sweep: the probe-set TTL, retention
// of events and aggregates, and a rollup rebuild after a step change.
type MonitoringMaintenanceJob struct {
	monitoringService service.MonitoringService
}

// NewMonitoringMaintenanceJob creates the hourly job.
func NewMonitoringMaintenanceJob() *MonitoringMaintenanceJob {
	return new(MonitoringMaintenanceJob)
}

// Run does the three sweeps, each on its own so one failure does not skip
// the others.
func (j *MonitoringMaintenanceJob) Run() {
	now := time.Now()
	if _, err := j.monitoringService.SweepProbeTTL(now); err != nil {
		logger.Warning("monitoring: probe-set TTL sweep failed:", err)
	}
	if n, err := j.monitoringService.Retention(now); err != nil {
		logger.Warning("monitoring: retention failed:", err)
	} else if n > 0 {
		logger.Infof("monitoring: retention removed %d rows", n)
	}
	if _, err := j.monitoringService.RebuildRollupIfIncomplete(now); err != nil {
		logger.Warning("monitoring: rollup rebuild failed:", err)
	}
}
