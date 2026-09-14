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
