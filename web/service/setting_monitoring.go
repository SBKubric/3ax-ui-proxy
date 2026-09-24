package service

import (
	"strconv"

	"github.com/coinman-dev/3ax-ui/v2/util/random"
)

// Monitoring settings (docs/spec/monitoring-panel.md §2.2). Keys live in
// defaultValueMap; no migration is needed because a key appears on first save.
//
// Two groups. Preferences — monEnable, monToken, monStaleMinutes,
// monProbeTtlHours, monRetentionDays, monRollupRetentionDays,
// monRollupStepMinutes — are also fields of entity.AllSetting and travel
// through the settings form. State — monProbeSubId, monProbeLastEnsured,
// monLastContact, monStaleSince, monClientsSnapshot — is written by the
// monitoring service and job as mon-server talks to the panel, and is
// reachable only through the accessors here, so a settings save cannot roll
// it back.

// monTokenLength matches the panel's own secret: 32 characters from
// random.Seq, which mon-server presents as a bearer token.
const monTokenLength = 32

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

// GetMonEnable reports whether the /mon/v1 endpoints answer at all.
func (s *SettingService) GetMonEnable() (bool, error) {
	return s.getBool("monEnable")
}

func (s *SettingService) SetMonEnable(value bool) error {
	return s.setBool("monEnable", value)
}

// GetMonToken returns the bearer token mon-server must present. Empty means
// no token has been issued yet, and the endpoints stay closed.
func (s *SettingService) GetMonToken() (string, error) {
	return s.getString("monToken")
}

func (s *SettingService) SetMonToken(value string) error {
	return s.setString("monToken", value)
}

// ResetMonToken issues a fresh token and returns it. The previous token stops
// working the moment this is saved; the operator carries the new one to the
// mon-server config themselves.
func (s *SettingService) ResetMonToken() (string, error) {
	token := random.Seq(monTokenLength)
	if err := s.SetMonToken(token); err != nil {
		return "", err
	}
	return token, nil
}

// GetMonStaleMinutes is how long mon-server may stay silent before the panel
// declares every target STALE.
func (s *SettingService) GetMonStaleMinutes() (int, error) {
	return s.getInt("monStaleMinutes")
}

func (s *SettingService) SetMonStaleMinutes(value int) error {
	return s.setInt("monStaleMinutes", value)
}

// GetMonProbeSubId is the subscription id shared by every probe account of the
// panel's single probe set; empty until the first POST /probe/ensure.
func (s *SettingService) GetMonProbeSubId() (string, error) {
	return s.getString("monProbeSubId")
}

func (s *SettingService) SetMonProbeSubId(value string) error {
	return s.setString("monProbeSubId", value)
}

// GetMonProbeLastEnsured is when mon-server last asked for the probe set, in
// milliseconds; 0 means never.
func (s *SettingService) GetMonProbeLastEnsured() (int64, error) {
	return s.getInt64("monProbeLastEnsured")
}

func (s *SettingService) SetMonProbeLastEnsured(value int64) error {
	return s.setInt64("monProbeLastEnsured", value)
}

// GetMonProbeTtlHours is how long a probe set outlives its last ensure before
// the hourly job removes it.
func (s *SettingService) GetMonProbeTtlHours() (int, error) {
	return s.getInt("monProbeTtlHours")
}

func (s *SettingService) SetMonProbeTtlHours(value int) error {
	return s.setInt("monProbeTtlHours", value)
}

// GetMonLastContact is the time of the last authorised mon-server request, in
// milliseconds; 0 means mon-server has never reached the panel, and STALE is
// not declared.
func (s *SettingService) GetMonLastContact() (int64, error) {
	return s.getInt64("monLastContact")
}

func (s *SettingService) SetMonLastContact(value int64) error {
	return s.setInt64("monLastContact", value)
}

// GetMonStaleSince is the last contact before the silence the panel has
// declared STALE, in milliseconds; 0 while monitoring is not STALE. Kept so a
// restart in the middle of a silence neither forgets it nor announces it
// again.
func (s *SettingService) GetMonStaleSince() (int64, error) {
	return s.getInt64("monStaleSince")
}

func (s *SettingService) SetMonStaleSince(value int64) error {
	return s.setInt64("monStaleSince", value)
}

// GetMonClientsSnapshot is the JSON array of mon-clients from the last
// POST /probe/ensure, cached for the Monitoring page; "[]" before the first.
func (s *SettingService) GetMonClientsSnapshot() (string, error) {
	return s.getString("monClientsSnapshot")
}

func (s *SettingService) SetMonClientsSnapshot(value string) error {
	return s.setString("monClientsSnapshot", value)
}

// GetMonRetentionDays bounds mon_stats_current and mon_events, and is the
// window inside which a repeated event id counts as a duplicate.
func (s *SettingService) GetMonRetentionDays() (int, error) {
	return s.getInt("monRetentionDays")
}

func (s *SettingService) SetMonRetentionDays(value int) error {
	return s.setInt("monRetentionDays", value)
}

// GetMonRollupRetentionDays bounds mon_stats_rollup.
func (s *SettingService) GetMonRollupRetentionDays() (int, error) {
	return s.getInt("monRollupRetentionDays")
}

func (s *SettingService) SetMonRollupRetentionDays(value int) error {
	return s.setInt("monRollupRetentionDays", value)
}

// GetMonRollupStepMinutes is the width of a rollup bucket; changing it makes
// the hourly job rebuild the rollup from current stats.
func (s *SettingService) GetMonRollupStepMinutes() (int, error) {
	return s.getInt("monRollupStepMinutes")
}

func (s *SettingService) SetMonRollupStepMinutes(value int) error {
	return s.setInt("monRollupStepMinutes", value)
}
