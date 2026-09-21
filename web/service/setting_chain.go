package service

import (
	"encoding/json"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/util/common"

	"gorm.io/gorm"
)

// Chain registry settings (docs/spec/proxy-chain.md §2.2). Keys live in
// defaultValueMap, so a key appears on its first save and no migration is
// needed.
//
// Two groups, as with monitoring. Preferences — chainExtraPorts,
// chainPollSeconds, chainStaleMinutes, chainJoinTokenHours — are fields of
// entity.AllSetting and travel through the settings form. State —
// chainRevision — is written by ChainService inside the same transaction as
// the registry write and is deliberately absent from AllSetting: a form save
// that rolled the revision back would make every box believe it is up to date
// while the registry has moved on.

const (
	chainRevisionKey       = "chainRevision"
	chainPanelHostKey      = "chainPanelHost"
	chainExtraPortsKey     = "chainExtraPorts"
	chainPollSecondsKey    = "chainPollSeconds"
	chainStaleMinutesKey   = "chainStaleMinutes"
	chainJoinTokenHoursKey = "chainJoinTokenHours"
)

// ChainExtraPort is one relayed port the panel cannot work out for itself:
// something the operator runs beside the panel and wants carried to the edge
// (§3.8). Note is the operator's own label; it never leaves the panel.
type ChainExtraPort struct {
	Port    int    `json:"port"`
	Network string `json:"network"`
	Note    string `json:"note,omitempty"`
}

// validate checks one extra port against the document's own vocabulary: the
// network is a chain.Network* value, and "tcp,udp" is one value rather than
// two, because the relay needs both listeners on the same port.
func (p ChainExtraPort) validate() error {
	if p.Port < 1 || p.Port > 65535 {
		return common.NewErrorf("chainExtraPorts: port %d is out of range 1-65535", p.Port)
	}
	switch p.Network {
	case chain.NetworkTCP, chain.NetworkUDP, chain.NetworkTCPUDP:
		return nil
	}
	return common.NewErrorf("chainExtraPorts: network %q must be one of %q, %q, %q",
		p.Network, chain.NetworkTCP, chain.NetworkUDP, chain.NetworkTCPUDP)
}

// GetChainRevision is the monotonic revision of the registry; it is the ETag
// of every chain document, so it only ever grows.
func (s *SettingService) GetChainRevision() (int64, error) {
	return s.getInt64(chainRevisionKey)
}

func (s *SettingService) SetChainRevision(value int64) error {
	return s.setInt64(chainRevisionKey, value)
}

// GetChainPanelHost is the address the innermost hop dials to reach the panel
// itself (§3.2). The panel cannot work it out: what it knows about itself is a
// listen address, and behind NAT, a tunnel or a reverse proxy that is not what
// a front can dial. So the owner states it, and a chain with hops in it and no
// panel host is a refusal rather than a document with an empty nextHop.
func (s *SettingService) GetChainPanelHost() (string, error) {
	host, err := s.getString(chainPanelHostKey)
	return strings.TrimSpace(host), err
}

func (s *SettingService) SetChainPanelHost(value string) error {
	return s.setString(chainPanelHostKey, strings.TrimSpace(value))
}

// GetChainExtraPortsRaw returns the stored JSON as it is, for the settings
// form and for callers that only pass it through.
func (s *SettingService) GetChainExtraPortsRaw() (string, error) {
	return s.getString(chainExtraPortsKey)
}

// GetChainExtraPorts parses and validates the operator's extra relayed ports.
// A value that does not parse is an error rather than an empty list: silently
// dropping the operator's ports would take those services off the chain with
// no sign of why.
func (s *SettingService) GetChainExtraPorts() ([]ChainExtraPort, error) {
	raw, err := s.GetChainExtraPortsRaw()
	if err != nil {
		return nil, err
	}
	return parseChainExtraPorts(raw)
}

// parseChainExtraPorts is the stored value turned into ports, so the settings
// accessor and the transaction-bound reader below cannot disagree about what
// the operator wrote.
func parseChainExtraPorts(raw string) ([]ChainExtraPort, error) {
	ports := []ChainExtraPort{}
	if raw = strings.TrimSpace(raw); raw == "" {
		return ports, nil
	}
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return nil, common.NewErrorf("chainExtraPorts is not a JSON array of ports: %v", err)
	}
	for _, port := range ports {
		if err := port.validate(); err != nil {
			return nil, err
		}
	}
	return ports, nil
}

// SetChainExtraPorts validates and stores the list. The port composition is
// part of every document, so a change here moves the chain to a new revision
// (§3.4) — and storing the same list again does not.
//
// The value and the revision are written in one transaction, like every other
// registry write: a reader must never catch the new port list under the old
// revision and conclude it is up to date. The previous value is read before
// the transaction opens, because this SQLite runs on a single connection.
func (s *SettingService) SetChainExtraPorts(ports []ChainExtraPort) error {
	if ports == nil {
		ports = []ChainExtraPort{}
	}
	for _, port := range ports {
		if err := port.validate(); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(ports)
	if err != nil {
		return err
	}
	previous, err := s.GetChainExtraPortsRaw()
	if err != nil {
		return err
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := saveSettingTx(tx, chainExtraPortsKey, string(encoded)); err != nil {
			return err
		}
		if previous == string(encoded) {
			return nil
		}
		return bumpRevisionTx(tx)
	})
}

// getSettingTx is getString inside a transaction, falling back to the default
// for a key that has never been saved — the same answer the settings accessors
// give, without the second connection they would need.
func getSettingTx(tx *gorm.DB, key string) (string, error) {
	var setting model.Setting
	err := tx.Where("key = ?", key).First(&setting).Error
	if database.IsNotFound(err) {
		value, known := defaultValueMap[key]
		if !known {
			return "", common.NewErrorf("setting %q has no default", key)
		}
		return value, nil
	}
	if err != nil {
		return "", err
	}
	return setting.Value, nil
}

// saveSettingTx is saveSetting inside a transaction: the settings accessors go
// through the global handle, which would wait for the connection the
// transaction is holding.
func saveSettingTx(tx *gorm.DB, key, value string) error {
	var setting model.Setting
	err := tx.Where("key = ?", key).First(&setting).Error
	if database.IsNotFound(err) {
		return tx.Create(&model.Setting{Key: key, Value: value}).Error
	}
	if err != nil {
		return err
	}
	setting.Value = value
	return tx.Save(&setting).Error
}

// GetChainPollSeconds is how often a hop polls its next hop for a new
// document.
func (s *SettingService) GetChainPollSeconds() (int, error) {
	return s.getInt(chainPollSecondsKey)
}

func (s *SettingService) SetChainPollSeconds(value int) error {
	return s.setInt(chainPollSecondsKey, value)
}

// GetChainStaleMinutes is how long a hop goes without a fresh document before
// it calls itself stale. It never stops relaying because of it (§3.6).
func (s *SettingService) GetChainStaleMinutes() (int, error) {
	return s.getInt(chainStaleMinutesKey)
}

func (s *SettingService) SetChainStaleMinutes(value int) error {
	return s.setInt(chainStaleMinutesKey, value)
}

// GetChainJoinTokenHours is the TTL of a join token — one box, one entry, 24
// hours by default (§4.1).
func (s *SettingService) GetChainJoinTokenHours() (int, error) {
	return s.getInt(chainJoinTokenHoursKey)
}

func (s *SettingService) SetChainJoinTokenHours(value int) error {
	return s.setInt(chainJoinTokenHoursKey, value)
}

// chainActiveEdgeHost is the registry half of GetProxyOverride: the host of
// the active edge, if the chain has one.
func chainActiveEdgeHost() (string, bool) {
	return (&ChainService{}).ActiveEdgeHost()
}

// legacyProxyOverride is the pre-chain behaviour of GetProxyOverride, kept for
// a panel that has no registry yet: the proxyOverrideEnable/proxyOverrideHost
// pair, enabled and non-empty. MigrateLegacyOverride imports it into the
// registry on the first start after the upgrade, after which this is dead
// weight for that panel — but it stays, because the keys are still the only
// override on a panel where the migration found nothing to import.
func (s *SettingService) legacyProxyOverride() (string, bool) {
	enabled, err := s.GetProxyOverrideEnable()
	if err != nil || !enabled {
		return "", false
	}
	host, err := s.GetProxyOverrideHost()
	if err != nil {
		return "", false
	}
	if host = strings.TrimSpace(host); host == "" {
		return "", false
	}
	return host, true
}

// DisableProxyOverride is what "/proxy off" means once the chain registry
// exists: no edge is active any more, and the legacy pair goes off with it.
// Clearing only the legacy keys would leave the registry's active edge
// publishing its host and the command looking broken.
func (s *SettingService) DisableProxyOverride() error {
	if err := (&ChainService{}).ClearActive(); err != nil {
		return err
	}
	return s.SetProxyOverrideEnable(false)
}
