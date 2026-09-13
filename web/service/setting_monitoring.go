package service

import (
	"strconv"

	"github.com/coinman-dev/3ax-ui/v2/util/random"
)

// Monitoring settings (docs/spec/monitoring-panel.md §2.2). Kept in their own
// file so the fork's monitoring keys never collide with an upstream edit of
// setting.go; the keys themselves live in defaultValueMap.

// monTokenLength matches the panel's own session secret.
const monTokenLength = 32

// GetMonEnable reports whether the /mon/v1 contract is open to mon-server.
func (s *SettingService) GetMonEnable() (bool, error) {
	return s.getBool("monEnable")
}

// SetMonEnable opens or closes the /mon/v1 contract.
func (s *SettingService) SetMonEnable(value bool) error {
	return s.setBool("monEnable", value)
}

// GetMonToken returns the bearer token mon-server has to present.
func (s *SettingService) GetMonToken() (string, error) {
	return s.getString("monToken")
}

// SetMonToken stores a bearer token; the previous one stops working at once.
func (s *SettingService) SetMonToken(value string) error {
	return s.setString("monToken", value)
}

// GenerateMonToken returns a fresh random token of the same shape as the
// panel's session secret. It does not store it: the caller decides whether the
// value goes into the settings or only into a form.
func GenerateMonToken() string {
	return random.Seq(monTokenLength)
}

// RegenerateMonToken replaces the stored token with a new one and returns it.
func (s *SettingService) RegenerateMonToken() (string, error) {
	token := GenerateMonToken()
	if err := s.SetMonToken(token); err != nil {
		return "", err
	}
	return token, nil
}

// GetMonStaleMinutes is the silence, in minutes, after which the panel
// declares monitoring STALE.
func (s *SettingService) GetMonStaleMinutes() (int, error) {
	return s.getInt("monStaleMinutes")
}

// SetMonStaleMinutes sets the STALE threshold.
func (s *SettingService) SetMonStaleMinutes(value int) error {
	return s.setInt("monStaleMinutes", value)
}

// GetMonProbeSubId is the subId every probe account shares; empty until the
// first POST /probe/ensure.
func (s *SettingService) GetMonProbeSubId() (string, error) {
	return s.getString("monProbeSubId")
}

// SetMonProbeSubId stores (or clears) the probe set's subId.
func (s *SettingService) SetMonProbeSubId(value string) error {
	return s.setString("monProbeSubId", value)
}

// GetMonProbeLastEnsured is the time of the last POST /probe/ensure, ms.
func (s *SettingService) GetMonProbeLastEnsured() (int64, error) {
	return s.getInt64("monProbeLastEnsured")
}

// SetMonProbeLastEnsured records a POST /probe/ensure.
func (s *SettingService) SetMonProbeLastEnsured(value int64) error {
	return s.setInt64("monProbeLastEnsured", value)
}

// GetMonProbeTtlHours is how long the probe set survives without an ensure.
func (s *SettingService) GetMonProbeTtlHours() (int, error) {
	return s.getInt("monProbeTtlHours")
}

// SetMonProbeTtlHours sets the probe-set TTL.
func (s *SettingService) SetMonProbeTtlHours(value int) error {
	return s.setInt("monProbeTtlHours", value)
}

// GetMonLastContact is the time of the last authorized mon-server request, ms;
// zero while mon-server has never called.
func (s *SettingService) GetMonLastContact() (int64, error) {
	return s.getInt64("monLastContact")
}

// SetMonLastContact records an authorized mon-server request.
func (s *SettingService) SetMonLastContact(value int64) error {
	return s.setInt64("monLastContact", value)
}

// GetMonClientsSnapshot is the registry snapshot mon-server last sent, as the
// JSON array it came in; a UI cache, never truth.
func (s *SettingService) GetMonClientsSnapshot() (string, error) {
	return s.getString("monClientsSnapshot")
}

// SetMonClientsSnapshot replaces the cached registry snapshot.
func (s *SettingService) SetMonClientsSnapshot(value string) error {
	return s.setString("monClientsSnapshot", value)
}

// GetMonRetentionDays is the retention of events and 5-minute aggregates, and
// the deduplication window of event ids.
func (s *SettingService) GetMonRetentionDays() (int, error) {
	return s.getInt("monRetentionDays")
}

// SetMonRetentionDays sets the fine retention.
func (s *SettingService) SetMonRetentionDays(value int) error {
	return s.setInt("monRetentionDays", value)
}

// GetMonRollupRetentionDays is the retention of rollup aggregates.
func (s *SettingService) GetMonRollupRetentionDays() (int, error) {
	return s.getInt("monRollupRetentionDays")
}

// SetMonRollupRetentionDays sets the rollup retention.
func (s *SettingService) SetMonRollupRetentionDays(value int) error {
	return s.setInt("monRollupRetentionDays", value)
}

// GetMonRollupStepMinutes is the rollup step.
func (s *SettingService) GetMonRollupStepMinutes() (int, error) {
	return s.getInt("monRollupStepMinutes")
}

// SetMonRollupStepMinutes sets the rollup step; the job rebuilds the rollup
// with the new step on its next hourly run.
func (s *SettingService) SetMonRollupStepMinutes(value int) error {
	return s.setInt("monRollupStepMinutes", value)
}

func (s *SettingService) getInt64(key string) (int64, error) {
	str, err := s.getString(key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(str, 10, 64)
}

func (s *SettingService) setInt64(key string, value int64) error {
	return s.setString(key, strconv.FormatInt(value, 10))
}
