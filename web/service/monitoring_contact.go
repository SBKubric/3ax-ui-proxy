package service

import (
	"sync"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// Last contact with mon-server (monitoring-panel.md §4.2, §5). Every
// authorised request touches it; the in-memory value is what the STALE job
// reads every minute, and the setting behind it is written at most once per
// monContactPersistEvery so a busy mon-server does not hammer SQLite. On a
// restart the setting seeds the memory.

const monContactPersistEvery = 10 * time.Second

var monContact struct {
	sync.Mutex
	loaded    bool
	last      int64 // ms, what the panel believes
	persisted int64 // ms, what the setting holds
}

// TouchMonLastContact records an authorised mon-server request at now.
func (s *MonitoringService) TouchMonLastContact(now time.Time) {
	ms := now.UnixMilli()
	monContact.Lock()
	defer monContact.Unlock()
	s.loadContactLocked()
	if ms > monContact.last {
		monContact.last = ms
	}
	if ms-monContact.persisted >= monContactPersistEvery.Milliseconds() {
		if err := s.settingService.SetMonLastContact(ms); err == nil {
			monContact.persisted = ms
		}
	}
	monContact.Unlock()
	s.clearMonStale(now)
	monContact.Lock()
}

// MonLastContact is the time of the last authorised mon-server request in
// milliseconds; 0 when mon-server has never reached the panel.
func (s *MonitoringService) MonLastContact() int64 {
	monContact.Lock()
	defer monContact.Unlock()
	s.loadContactLocked()
	return monContact.last
}

func (s *MonitoringService) loadContactLocked() {
	if monContact.loaded {
		return
	}
	if v, err := s.settingService.GetMonLastContact(); err == nil {
		monContact.last, monContact.persisted = v, v
	}
	monContact.loaded = true
}

// resetMonContactForTest clears the cache between tests.
func resetMonContactForTest() {
	monContact.Lock()
	monContact.loaded, monContact.last, monContact.persisted = false, 0, 0
	monContact.Unlock()
}

// ResetMonContactForTest clears the process-wide last-contact cache. The
// cache is shared by every MonitoringService in the process (it backs the
// STALE job across requests), so tests outside this package that exercise
// checkMonAuth/TouchMonLastContact — e.g. web/controller's monitoring tests —
// must call this in setup to avoid picking up a value left by another test
// that ran earlier in the same binary (-shuffle=on reorders them).
func ResetMonContactForTest() {
	resetMonContactForTest()
}

// ResetMonStaleForTest is the STALE-flag counterpart of
// ResetMonContactForTest, for the same reason.
func ResetMonStaleForTest() {
	resetMonStaleForTest()
}

// ResetMonRegistryForTest forgets the cached mon-client snapshot, for the
// same reason: the cache outlives each test's database, so a snapshot one
// test ensured would make another test's targets look current.
func ResetMonRegistryForTest() {
	monRegistry.Lock()
	monRegistry.loaded, monRegistry.clients = false, nil
	monRegistry.Unlock()
}

// --- default probe link renderer ---------------------------------------------

// The xray link renderer lives in package sub, which imports this package
// (and, through the sub server, package web), so neither the service nor the
// controller can import it. sub registers its renderer here at init; a
// MonitoringService without an explicit Links falls back to it.
var probeLinkDefault struct {
	sync.RWMutex
	r ProbeLinkRenderer
}

// SetProbeLinkRenderer installs the process-wide link renderer.
func SetProbeLinkRenderer(r ProbeLinkRenderer) {
	probeLinkDefault.Lock()
	probeLinkDefault.r = r
	probeLinkDefault.Unlock()
}

func (s *MonitoringService) links() ProbeLinkRenderer {
	if s.Links != nil {
		return s.Links
	}
	probeLinkDefault.RLock()
	defer probeLinkDefault.RUnlock()
	return probeLinkDefault.r
}

// --- STALE -------------------------------------------------------------------

// STALE is the panel's one own state (monitoring-panel.md §5): mon-server has
// been silent longer than monStaleMinutes, so every target is suspect. It is
// a flag — mon_targets rows are not touched — set by the minute job and
// cleared by the next authorised request. Both edges are announced once
// through the MonStaleNotifier the bot registers (§6). Until mon-server has
// reached the panel at all (monLastContact = 0) STALE is never declared, and
// with monEnable=false it is neither declared nor sent.
//
// The flag lives in memory with its start mirrored in monStaleSince, so a
// restart in the middle of a silence picks it up again instead of announcing
// it a second time (SBKubric/3ax-ui-monitoring#50, item 8).

// MonStaleNotifier is the Telegram side of STALE. since is the last contact
// before the silence; silentFor how long it lasted.
type MonStaleNotifier interface {
	NotifyMonitoringStale(since time.Time)
	NotifyMonitoringBack(silentFor time.Duration)
}

// monStaleInterval is one closed stretch of silence, [from, to] in ms. The
// digest and GET summary need how long monitoring was silent inside their
// window (§6, §7.4), which the flag alone cannot answer once the silence is
// over, so every stretch is remembered.
type monStaleInterval struct {
	from int64
	to   int64
}

// monStaleHistory is how far back the intervals are kept: the longest summary
// range is 7 days, so anything older can never be asked about.
const monStaleHistory = 7 * 24 * time.Hour

var monStale struct {
	sync.Mutex
	loaded bool // monStaleSince has been read since the process started
	stale  bool
	since  int64 // ms: the last contact before the silence
	n      MonStaleNotifier
	// intervals are the closed stretches of silence, oldest first. They live
	// in memory only: a restart loses the history, so a summary taken right
	// after one reports less STALE time than really happened (v1 limitation).
	intervals []monStaleInterval
}

// SetMonStaleNotifier installs (or, with nil, removes) the Telegram hook.
func SetMonStaleNotifier(n MonStaleNotifier) {
	monStale.Lock()
	monStale.n = n
	monStale.Unlock()
}

// IsMonStale reports whether the panel currently considers monitoring
// silent, and since when (ms) if so.
func (s *MonitoringService) IsMonStale() (bool, int64) {
	monStale.Lock()
	defer monStale.Unlock()
	s.loadStaleLocked()
	return monStale.stale, monStale.since
}

// loadStaleLocked restores a silence that was going on when the panel last
// stopped. The caller holds monStale's lock.
func (s *MonitoringService) loadStaleLocked() {
	if monStale.loaded {
		return
	}
	monStale.loaded = true
	if since, err := s.settingService.GetMonStaleSince(); err == nil && since > 0 && !monStale.stale {
		monStale.stale, monStale.since = true, since
	}
}

// persistStaleSince mirrors the flag's start into the setting, 0 when clear.
func (s *MonitoringService) persistStaleSince(since int64) {
	if err := s.settingService.SetMonStaleSince(since); err != nil {
		logger.Warning("monitoring: could not persist monStaleSince:", err)
	}
}

// CheckMonStale is the minute job: declare STALE when the last contact is
// older than the threshold. Returns true when this call made the transition.
// With monitoring switched off nothing is declared, and a STALE left from
// before the switch is dropped silently: nobody is expected to call.
func (s *MonitoringService) CheckMonStale(now time.Time) bool {
	if enabled, err := s.settingService.GetMonEnable(); err != nil || !enabled {
		s.dropMonStale(now)
		return false
	}
	last := s.MonLastContact()
	if last == 0 {
		return false
	}
	minutes, err := s.settingService.GetMonStaleMinutes()
	if err != nil || minutes <= 0 {
		minutes = 15
	}
	if now.UnixMilli()-last <= int64(minutes)*60*1000 {
		return false
	}
	monStale.Lock()
	s.loadStaleLocked()
	if monStale.stale {
		monStale.Unlock()
		return false
	}
	monStale.stale, monStale.since = true, last
	s.persistStaleSince(last)
	n := monStale.n
	monStale.Unlock()
	if n != nil {
		n.NotifyMonitoringStale(time.UnixMilli(last))
	}
	return true
}

// clearMonStale is the other edge, taken by the first authorised request.
func (s *MonitoringService) clearMonStale(now time.Time) {
	if since, ok := s.endMonStale(now); ok {
		monStale.Lock()
		n := monStale.n
		monStale.Unlock()
		if n != nil {
			n.NotifyMonitoringBack(now.Sub(time.UnixMilli(since)))
		}
	}
}

// dropMonStale ends a silence without announcing it (monitoring switched off).
func (s *MonitoringService) dropMonStale(now time.Time) {
	s.endMonStale(now)
}

// endMonStale clears the flag and its setting and records the stretch;
// reports the silence's start and whether there was one.
func (s *MonitoringService) endMonStale(now time.Time) (int64, bool) {
	monStale.Lock()
	defer monStale.Unlock()
	s.loadStaleLocked()
	if !monStale.stale {
		return 0, false
	}
	since := monStale.since
	monStale.stale, monStale.since = false, 0
	s.persistStaleSince(0)
	appendMonStaleIntervalLocked(since, now.UnixMilli())
	return since, true
}

// appendMonStaleIntervalLocked records a finished stretch of silence and
// forgets the ones no summary can still ask about. The caller holds the lock.
func appendMonStaleIntervalLocked(from, to int64) {
	if to <= from {
		return
	}
	monStale.intervals = append(monStale.intervals, monStaleInterval{from: from, to: to})
	cutoff := to - monStaleHistory.Milliseconds()
	kept := monStale.intervals[:0]
	for _, iv := range monStale.intervals {
		if iv.to >= cutoff {
			kept = append(kept, iv)
		}
	}
	monStale.intervals = kept
}

// StaleMsWithin is how many milliseconds of [from, to) the panel spent
// considering monitoring silent: the closed stretches it remembers plus the
// one still open, each clipped to the window. Only what happened since the
// last restart is counted (see monStale.intervals).
func (s *MonitoringService) StaleMsWithin(from, to int64, now time.Time) int64 {
	monStale.Lock()
	defer monStale.Unlock()
	s.loadStaleLocked()
	var total int64
	for _, iv := range monStale.intervals {
		total += overlapMs(iv.from, iv.to, from, to)
	}
	if monStale.stale {
		total += overlapMs(monStale.since, now.UnixMilli(), from, to)
	}
	return total
}

// overlapMs is the length of [aFrom,aTo) ∩ [bFrom,bTo), never negative.
func overlapMs(aFrom, aTo, bFrom, bTo int64) int64 {
	if aFrom < bFrom {
		aFrom = bFrom
	}
	if aTo > bTo {
		aTo = bTo
	}
	if aTo <= aFrom {
		return 0
	}
	return aTo - aFrom
}

func resetMonStaleForTest() {
	monStale.Lock()
	monStale.loaded, monStale.stale, monStale.since, monStale.n, monStale.intervals = false, false, 0, nil, nil
	monStale.Unlock()
}
