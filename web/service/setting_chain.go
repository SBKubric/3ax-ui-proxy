package service

import (
	"encoding/json"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/util/common"
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
// (§3.4) — and a change that only reorders nothing at all does not.
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
	if err := s.setString(chainExtraPortsKey, string(encoded)); err != nil {
		return err
	}
	if previous == string(encoded) {
		return nil
	}
	revision, err := s.GetChainRevision()
	if err != nil {
		return err
	}
	return s.SetChainRevision(revision + 1)
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
