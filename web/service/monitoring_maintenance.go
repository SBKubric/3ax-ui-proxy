package service

import (
	"fmt"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// What MonitoringJob runs (docs/spec/monitoring-panel.md §5): STALE every
// minute; probe-set TTL, retention and the rollup rebuild every hour. The
// logic lives here so the job stays a schedule and the behaviour is testable
// against a clock.

// monRetentionBatch is how many rows one retention DELETE removes; small
// enough not to hold the single SQLite connection for long.
const monRetentionBatch = 5000

// CheckStale raises the panel's STALE flag when the last authorized contact
// is older than monStaleMinutes, and reports whether this call raised it
// (the hook fires once per silence). While mon-server has never called at
// all there is nothing to be silent about.
func (s *MonitoringService) CheckStale(now time.Time) bool {
	lastContact := s.LastContact()
	if lastContact == 0 {
		return false
	}
	minutes, err := s.settingService.GetMonStaleMinutes()
	if err != nil || minutes <= 0 {
		minutes = 15
	}
	if now.UnixMilli()-lastContact <= int64(minutes)*60_000 {
		return false
	}
	if !s.setStale(lastContact) {
		return false
	}
	if hooks := currentMonStatusHooks(); hooks.Stale != nil {
		hooks.Stale(lastContact)
	}
	return true
}

// SweepProbeTTL removes the probe set when no ensure has arrived for
// monProbeTtlHours, and removes stray probe accounts when there is no set
// (an empty subId) they could belong to. It reports whether it removed one.
func (s *MonitoringService) SweepProbeTTL(now time.Time) (bool, error) {
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return false, err
	}
	if subId == "" {
		count, err := s.ProbeAccountCount()
		if err != nil {
			return false, err
		}
		if count == 0 {
			return false, nil
		}
		logger.Infof("monitoring: %d probe accounts without a probe set, removing them", count)
		return true, s.DeleteProbeSet()
	}
	lastEnsured, err := s.settingService.GetMonProbeLastEnsured()
	if err != nil {
		return false, err
	}
	if lastEnsured == 0 {
		return false, nil // a subId minted by an ensure that never finished; nothing to expire yet
	}
	ttlHours, err := s.settingService.GetMonProbeTtlHours()
	if err != nil || ttlHours <= 0 {
		ttlHours = 24
	}
	if now.UnixMilli()-lastEnsured <= int64(ttlHours)*3_600_000 {
		return false, nil
	}
	logger.Infof("monitoring: no probe ensure for %d hours, removing the probe set", ttlHours)
	return true, s.DeleteProbeSet()
}

// Retention deletes what is older than the retention windows, in batches of
// monRetentionBatch rows by rowid and without VACUUM: mon_stats_current and
// mon_events by monRetentionDays, mon_stats_rollup by
// monRollupRetentionDays. It reports how many rows went.
func (s *MonitoringService) Retention(now time.Time) (int64, error) {
	floor, err := s.retentionFloor(now)
	if err != nil {
		return 0, err
	}
	rollupDays, err := s.settingService.GetMonRollupRetentionDays()
	if err != nil || rollupDays <= 0 {
		rollupDays = 30
	}
	rollupFloor := now.Add(-time.Duration(rollupDays) * 24 * time.Hour).UnixMilli()

	var total int64
	for _, sweep := range []struct {
		table, column string
		floor         int64
	}{
		{"mon_stats_current", "bucket_start", floor},
		{"mon_events", "ts", floor},
		{"mon_stats_rollup", "bucket_start", rollupFloor},
	} {
		n, err := deleteInBatches(sweep.table, sweep.column, sweep.floor)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// deleteInBatches removes the rows of table whose column is below floor,
// monRetentionBatch at a time, so no single statement holds the connection
// long. Table and column are code constants, never input.
func deleteInBatches(table, column string, floor int64) (int64, error) {
	db := database.GetDB()
	stmt := fmt.Sprintf("DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s < ? LIMIT %d)",
		table, table, column, monRetentionBatch)
	var total int64
	for {
		res := db.Exec(stmt, floor)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if res.RowsAffected < monRetentionBatch {
			return total, nil
		}
	}
}

// RebuildRollupIfIncomplete rebuilds the rollup for the current step when
// the window has fewer rollup rows of that step than the current rows call
// for — which is what a changed monRollupStepMinutes looks like, and also a
// rollup that was never built. Rows of a previous step are not touched; they
// age out under their own retention. It reports whether it rebuilt.
func (s *MonitoringService) RebuildRollupIfIncomplete(now time.Time) (bool, error) {
	stepMs, err := s.rollupStepMs()
	if err != nil {
		return false, err
	}
	floor, err := s.retentionFloor(now)
	if err != nil {
		return false, err
	}
	db := database.GetDB()
	var expected, actual int64
	if err := db.Raw(`SELECT COUNT(*) FROM (SELECT DISTINCT mon_client_id, inbound_kind, inbound_id, path, bucket_start - (bucket_start % ?)
		FROM mon_stats_current WHERE bucket_start >= ?)`, stepMs, floor).Scan(&expected).Error; err != nil {
		return false, err
	}
	if expected == 0 {
		return false, nil
	}
	if err := db.Raw(`SELECT COUNT(*) FROM mon_stats_rollup WHERE step_ms = ? AND bucket_start >= ?`,
		stepMs, floor-floor%stepMs).Scan(&actual).Error; err != nil {
		return false, err
	}
	if actual >= expected {
		return false, nil
	}
	logger.Infof("monitoring: rollup has %d of %d buckets at step %d ms, rebuilding", actual, expected, stepMs)
	return true, s.RebuildRollup(stepMs, floor)
}
