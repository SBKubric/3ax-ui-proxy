package service

import (
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// Hourly upkeep (monitoring-panel.md §5): the probe set's TTL, retention of
// the three time-series tables, and rollup repair. Run by MonitoringJob.

// monPruneBatch is how many rows one DELETE removes: the panel has a single
// SQLite connection with a 5 s busy timeout, so a week of stats must not go
// in one statement. No VACUUM.
const monPruneBatch = 5000

// monProbeTtlHoursFallback stands in when monProbeTtlHours is unreadable or
// not a positive number, so a broken setting cannot make the set immortal or
// sweep it at once.
const monProbeTtlHoursFallback = 24

// ExpireProbeSet removes the probe set when mon-server has not ensured it for
// longer than monProbeTtlHours, and sweeps stray probe accounts left behind
// without a subId. Returns true when something was removed.
func (s *MonitoringService) ExpireProbeSet(now time.Time) (bool, error) {
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return false, err
	}
	lastEnsured, err := s.settingService.GetMonProbeLastEnsured()
	if err != nil {
		return false, err
	}
	if subId == "" {
		if lastEnsured == 0 && !s.hasProbeAccounts() {
			return false, nil
		}
		return true, s.DeleteProbeSet()
	}
	ttl, err := s.settingService.GetMonProbeTtlHours()
	if err != nil || ttl <= 0 {
		ttl = monProbeTtlHoursFallback
	}
	if now.UnixMilli()-lastEnsured <= int64(ttl)*60*60*1000 {
		return false, nil
	}
	return true, s.DeleteProbeSet()
}

// hasProbeAccounts reports whether any probe client exists, xray or tunnel.
func (s *MonitoringService) hasProbeAccounts() bool {
	return s.countProbeAccounts() > 0
}

// countProbeAccounts counts the probe clients actually present, xray and
// tunnel together. It counts what is there rather than what ensure should
// have made: a probe deleted by hand (§3.2) is missing until the next ensure,
// and the settings tab says so.
//
// A table that cannot be read contributes nothing; this is a display and a
// sweep guard, not a decision about state.
func (s *MonitoringService) countProbeAccounts() int {
	n := 0
	inbounds, err := s.inboundService.GetAllInbounds()
	if err == nil {
		for _, ib := range inbounds {
			if !monXrayProtocols[ib.Protocol] {
				continue
			}
			clients, err := s.inboundService.GetClients(ib)
			if err != nil {
				continue
			}
			for _, c := range clients {
				if IsProbeAccount(c.Email) {
					n++
				}
			}
		}
	}
	tunnelClients, err := s.awgService.GetClients()
	if err == nil {
		for _, c := range tunnelClients {
			if IsProbeAccount(c.Email) {
				n++
			}
		}
	}
	return n
}

// MonProbeSetInfo is the probe-set line of the Monitoring settings tab
// (§7.3): which subId the set shares, when mon-server last ensured it, how
// long it survives without an ensure, and how many probe accounts are there
// now.
type MonProbeSetInfo struct {
	SubId       string `json:"subId"`
	LastEnsured int64  `json:"lastEnsured"`
	TtlHours    int    `json:"ttlHours"`
	Clients     int    `json:"clients"`
}

// ProbeSetInfo is GET /panel/api/monitoring/probe. The three state keys it
// reads (monProbeSubId, monProbeLastEnsured and the TTL) are not part of
// AllSetting, so the settings page has no other way to them.
func (s *MonitoringService) ProbeSetInfo() (*MonProbeSetInfo, error) {
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return nil, err
	}
	lastEnsured, err := s.settingService.GetMonProbeLastEnsured()
	if err != nil {
		return nil, err
	}
	ttl, err := s.settingService.GetMonProbeTtlHours()
	if err != nil || ttl <= 0 {
		ttl = monProbeTtlHoursFallback
	}
	return &MonProbeSetInfo{
		SubId: subId, LastEnsured: lastEnsured, TtlHours: ttl,
		Clients: s.countProbeAccounts(),
	}, nil
}

// PruneRetention drops mon_stats_current and mon_events older than
// monRetentionDays and mon_stats_rollup older than monRollupRetentionDays, in
// batches of monPruneBatch rowids. Returns rows removed per table.
func (s *MonitoringService) PruneRetention(now time.Time) (map[string]int64, error) {
	cutoff := s.retentionCutoff(now.UnixMilli())
	rollupDays, err := s.settingService.GetMonRollupRetentionDays()
	if err != nil || rollupDays <= 0 {
		rollupDays = 30
	}
	rollupCutoff := now.UnixMilli() - int64(rollupDays)*24*60*60*1000

	removed := map[string]int64{}
	for _, t := range []struct {
		table, column string
		cutoff        int64
	}{
		{"mon_stats_current", "bucket_start", cutoff},
		{"mon_events", "ts", cutoff},
		{"mon_stats_rollup", "bucket_start", rollupCutoff},
	} {
		n, err := pruneTable(t.table, t.column, t.cutoff)
		removed[t.table] = n
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func pruneTable(table, column string, cutoff int64) (int64, error) {
	db := database.GetDB()
	var total int64
	for {
		res := db.Exec("DELETE FROM "+table+" WHERE rowid IN (SELECT rowid FROM "+table+" WHERE "+column+" < ? LIMIT ?)",
			cutoff, monPruneBatch)
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
		if res.RowsAffected < monPruneBatch {
			return total, nil
		}
	}
}

// RebuildMissingRollup recomputes, for the current step, every rollup bucket
// inside the retention window that has current-stats rows but no rollup row.
// After a change of monRollupStepMinutes that is the whole window; otherwise
// it is nothing, or a gap left by an interrupted upsert. Rows of a previous
// step are left to age out under their own retention. Returns the number of
// buckets rebuilt.
func (s *MonitoringService) RebuildMissingRollup(now time.Time) (int, error) {
	stepMs := s.rollupStepMs()
	cutoff := s.retentionCutoff(now.UnixMilli())
	db := database.GetDB()

	type bucket struct {
		MonClientId string
		InboundKind string
		InboundId   int
		Path        string
		RollupStart int64
	}
	var wanted []bucket
	if err := db.Raw(`SELECT DISTINCT mon_client_id, inbound_kind, inbound_id, path,
			bucket_start - (bucket_start % ?) AS rollup_start
		FROM mon_stats_current WHERE bucket_start >= ?`, stepMs, cutoff).Scan(&wanted).Error; err != nil {
		return 0, err
	}
	if len(wanted) == 0 {
		return 0, nil
	}
	var have []bucket
	if err := db.Raw(`SELECT mon_client_id, inbound_kind, inbound_id, path, bucket_start AS rollup_start
		FROM mon_stats_rollup WHERE step_ms = ? AND bucket_start >= ?`, stepMs, cutoff-stepMs).Scan(&have).Error; err != nil {
		return 0, err
	}
	present := make(map[bucket]bool, len(have))
	for _, b := range have {
		present[b] = true
	}
	rebuilt := 0
	for _, b := range wanted {
		if present[b] {
			continue
		}
		key := monStatKey{b.MonClientId, b.InboundKind, b.InboundId, b.Path, b.RollupStart}
		if err := recomputeRollupBucket(db, key, stepMs); err != nil {
			return rebuilt, err
		}
		rebuilt++
	}
	if rebuilt > 0 {
		logger.Infof("monitoring: rebuilt %d rollup bucket(s) at step %d ms", rebuilt, stepMs)
	}
	return rebuilt, nil
}
