package service

import (
	"sync"
	"time"
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
	clearMonStale(now)
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
// a flag in memory — mon_targets rows are not touched — set by the minute job
// and cleared by the next authorised request. Both edges are announced once
// through the MonStaleNotifier the bot registers (§6). Until mon-server has
// reached the panel at all (monLastContact = 0) STALE is never declared.

// MonStaleNotifier is the Telegram side of STALE. since is the last contact
// before the silence; silentFor how long it lasted.
type MonStaleNotifier interface {
	NotifyMonitoringStale(since time.Time)
	NotifyMonitoringBack(silentFor time.Duration)
}

var monStale struct {
	sync.Mutex
	stale bool
	since int64 // ms: the last contact before the silence
	n     MonStaleNotifier
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
	return monStale.stale, monStale.since
}

// CheckMonStale is the minute job: declare STALE when the last contact is
// older than the threshold. Returns true when this call made the transition.
func (s *MonitoringService) CheckMonStale(now time.Time) bool {
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
	if monStale.stale {
		monStale.Unlock()
		return false
	}
	monStale.stale, monStale.since = true, last
	n := monStale.n
	monStale.Unlock()
	if n != nil {
		n.NotifyMonitoringStale(time.UnixMilli(last))
	}
	return true
}

// clearMonStale is the other edge, taken by the first authorised request.
func clearMonStale(now time.Time) {
	monStale.Lock()
	if !monStale.stale {
		monStale.Unlock()
		return
	}
	since := monStale.since
	monStale.stale, monStale.since = false, 0
	n := monStale.n
	monStale.Unlock()
	if n != nil {
		n.NotifyMonitoringBack(now.Sub(time.UnixMilli(since)))
	}
}

func resetMonStaleForTest() {
	monStale.Lock()
	monStale.stale, monStale.since, monStale.n = false, 0, nil
	monStale.Unlock()
}
