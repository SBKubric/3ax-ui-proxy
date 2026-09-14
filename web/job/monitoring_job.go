package job

import (
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// MonitoringJob is the panel's own share of the monitoring work
// (docs/spec/monitoring-panel.md §5). Two schedules share the type: every
// minute it watches for mon-server going silent (STALE); every hour it expires
// an un-ensured probe set, prunes the time-series tables to their retention,
// and repairs the rollup after a step change.
type MonitoringJob struct {
	cadence           MonitoringCadence
	monitoringService service.MonitoringService
}

// MonitoringCadence selects which share of the work a MonitoringJob does.
type MonitoringCadence int

const (
	// MonitoringEveryMinute: STALE detection. Schedule at "@every 1m".
	MonitoringEveryMinute MonitoringCadence = iota
	// MonitoringHourly: probe-set TTL, retention, rollup repair. Schedule at "@hourly".
	MonitoringHourly
)

// NewMonitoringJob creates the job for one cadence.
func NewMonitoringJob(cadence MonitoringCadence) *MonitoringJob {
	return &MonitoringJob{cadence: cadence}
}

// Run does the cadence's work once.
func (j *MonitoringJob) Run() {
	now := time.Now()
	switch j.cadence {
	case MonitoringEveryMinute:
		if j.monitoringService.CheckMonStale(now) {
			_, since := j.monitoringService.IsMonStale()
			logger.Warningf("monitoring: mon-server silent since %s, targets are STALE", time.UnixMilli(since).Format(time.RFC3339))
		}
	case MonitoringHourly:
		if removed, err := j.monitoringService.ExpireProbeSet(now); err != nil {
			logger.Warning("monitoring: probe set TTL:", err)
		} else if removed {
			logger.Info("monitoring: probe set removed, mon-server has not ensured it in time")
		}
		if removed, err := j.monitoringService.PruneRetention(now); err != nil {
			logger.Warning("monitoring: retention:", err, removed)
		}
		if _, err := j.monitoringService.RebuildMissingRollup(now); err != nil {
			logger.Warning("monitoring: rollup repair:", err)
		}
	}
}
