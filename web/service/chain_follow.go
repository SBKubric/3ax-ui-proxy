package service

import (
	"bytes"
	"encoding/json"
	"net"
	"strconv"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"

	"gorm.io/gorm"
)

// The neighbour target of an edge and the chain-following inbounds that take
// it over (ADR 0005, docs/spec/proxy-chain.md §2.2, §4.7).
//
// Every box of the chain shows one port, and a prober that sends an unknown
// SNI to the active edge is handed the edge's neighbour — a real site next to
// the edge's address. The Reality inbounds clients connect through have to
// imitate that same site, so whenever the active edge changes, the panel
// rewrites their target and serverNames to the new edge's neighbour. Only the
// inbounds the owner marked as following the chain are touched; any other
// inbound keeps whatever cover it was given by hand.

// Refusals of this feature, stable like every other registry code.
const (
	CodeInvalidRealityTarget     = "invalid_reality_target"
	CodeInvalidRealityServerName = "invalid_reality_server_name"
	CodeRealityTargetEdgeOnly    = "reality_target_edge_only"
	CodeNoNeighbourTarget        = "no_neighbour_target"
	CodeFollowChainNotReality    = "follow_chain_not_reality"
)

// neighbourTarget validates a neighbour target the owner or the orchestrator
// sent, and returns the pair to store. An empty target means "none"; a server
// name without one is refused, because there is no site for it to name.
//
// The server name may be left empty, and the host part of the target stands
// in for it (ChainHop.NeighbourServerName) — except when that host is an
// address: SNI carries names only, so a target found by scanning the edge's
// network needs the name its certificate answers to spelled out.
func neighbourTarget(target, serverName string) (string, string, error) {
	target = strings.TrimSpace(target)
	serverName = strings.TrimSpace(serverName)
	if target == "" {
		if serverName != "" {
			return "", "", chainErrorf(CodeInvalidRealityTarget, "reality server name %q needs a reality target", serverName)
		}
		return "", "", nil
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" || len(target) > 262 || strings.ContainsAny(host, " /") {
		return "", "", chainErrorf(CodeInvalidRealityTarget, "reality target %q must be host:port", target)
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return "", "", chainErrorf(CodeInvalidRealityTarget, "reality target %q has no port between 1 and 65535", target)
	}
	if serverName == "" {
		if net.ParseIP(host) != nil {
			return "", "", chainErrorf(CodeInvalidRealityServerName,
				"reality target %q is an address; give the server name its site answers to", target)
		}
		return target, "", nil
	}
	if len(serverName) > 253 || strings.ContainsAny(serverName, " :/") || net.ParseIP(serverName) != nil {
		return "", "", chainErrorf(CodeInvalidRealityServerName, "reality server name %q must be a domain name", serverName)
	}
	return target, serverName, nil
}

// neighbourOnlyOnEdges refuses a neighbour target on an inner front: clients
// never reach an inner, so nothing would ever imitate its neighbour.
func neighbourOnlyOnEdges(hop *model.ChainHop, role string) error {
	if role != chain.RoleEdge && hop.RealityTarget != "" {
		return chainErrorf(CodeRealityTargetEdgeOnly, "%q is an %s front; only an edge has a neighbour target", hop.Name, role)
	}
	return nil
}

// followEdgeTx rewrites every chain-following inbound to the neighbour target
// of edge, inside the caller's transaction, and returns how many of them
// changed. An edge without a neighbour target is refused while any such
// inbound exists: the inbounds would keep imitating the previous edge's
// neighbour, a site in another network than the address clients now reach.
//
// Only the id and the stream settings are read: settings can be megabytes of
// clients, and none of it is needed here.
func followEdgeTx(tx *gorm.DB, edge *model.ChainHop) (int, error) {
	var inbounds []model.Inbound
	if err := tx.Model(&model.Inbound{}).Select("id", "stream_settings").
		Where("follow_chain = ?", true).Find(&inbounds).Error; err != nil {
		return 0, err
	}
	if len(inbounds) == 0 {
		return 0, nil
	}
	if edge.RealityTarget == "" {
		return 0, chainErrorf(CodeNoNeighbourTarget,
			"%q has no neighbour target, and %d chain-following inbound(s) need one; set its realityTarget first",
			edge.Name, len(inbounds))
	}
	changed := 0
	for _, inbound := range inbounds {
		stream, rewritten, err := withNeighbourTarget(inbound.StreamSettings, edge.RealityTarget, edge.NeighbourServerName())
		if err != nil {
			// The flag is refused on anything but Reality when it is set, so
			// this is a row edited behind the panel's back. It must not hold
			// every other inbound on the old edge's neighbour.
			logger.Warningf("chain: inbound %d follows the chain but cannot take a neighbour target: %v", inbound.Id, err)
			continue
		}
		if !rewritten {
			continue
		}
		if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
			UpdateColumn("stream_settings", stream).Error; err != nil {
			return 0, err
		}
		changed++
	}
	return changed, nil
}

// withNeighbourTarget returns stream settings whose Reality target is target
// and whose serverNames are serverName alone — strict SNI, so a client with an
// old link fails loudly instead of quietly imitating a site in the wrong
// network (ADR 0005). The key is "target", which the inbound form writes; a
// stale legacy "dest" beside it is dropped so the two cannot disagree.
//
// rewritten is false when the settings already say exactly this, so a switch
// back and forth does not restart xray for nothing.
func withNeighbourTarget(streamSettings, target, serverName string) (string, bool, error) {
	decoder := json.NewDecoder(strings.NewReader(streamSettings))
	decoder.UseNumber() // keep xver, maxTimediff and friends exactly as written
	var stream map[string]any
	if err := decoder.Decode(&stream); err != nil {
		return "", false, err
	}
	if security, _ := stream["security"].(string); security != "reality" {
		return "", false, chainErrorf(CodeFollowChainNotReality, "security is %q, not reality", security)
	}
	reality, _ := stream["realitySettings"].(map[string]any)
	if reality == nil {
		reality = map[string]any{}
		stream["realitySettings"] = reality
	}

	_, hasDest := reality["dest"]
	names, _ := reality["serverNames"].([]any)
	if reality["target"] == target && !hasDest && len(names) == 1 && names[0] == serverName {
		return streamSettings, false, nil
	}
	reality["target"] = target
	delete(reality, "dest")
	reality["serverNames"] = []string{serverName}
	// The client half the form keeps beside it names the same site; a stale
	// name there would contradict the one the inbound now accepts.
	if settings, ok := reality["settings"].(map[string]any); ok {
		settings["serverName"] = serverName
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(stream); err != nil {
		return "", false, err
	}
	return strings.TrimRight(out.String(), "\n"), true, nil
}

// chainFollowersRestart tells xray to pick the rewritten inbounds up. It runs
// after the transaction commits: xray regenerates its config from the
// database, and a restart that raced the commit would load the old one.
func chainFollowersRestart(changed int) {
	if changed > 0 {
		logger.Infof("chain: %d chain-following inbound(s) took the new neighbour target; restarting xray", changed)
		(&XrayService{}).SetToNeedRestart()
	}
}

// PrepareFollower runs on every save of an inbound, before it is written and
// before xray hears of it. A chain-following inbound must be Reality — its
// target and serverNames are all the panel switches — and one flagged while an
// edge is active takes that edge's neighbour at once rather than at the next
// switch, which could be weeks away. An active edge without a neighbour is
// refused, as SetActive refuses it: the inbound would advertise a cover site
// that has nothing to do with the address clients reach.
//
// With no active edge the inbound keeps the cover it was given: the panel is
// publishing the real server's own address then, and there is no edge whose
// network the cover has to match.
func (s *ChainService) PrepareFollower(inbound *model.Inbound) error {
	if !inbound.FollowChain {
		return nil
	}
	if !isReality(inbound.StreamSettings) {
		return chainErrorf(CodeFollowChainNotReality,
			"inbound %q cannot follow the chain: only a Reality inbound has a target and serverNames to switch", inbound.Remark)
	}
	db := database.GetDB()
	var edge model.ChainHop
	err := db.Where("is_active = ?", true).First(&edge).Error
	if database.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if edge.RealityTarget == "" {
		return chainErrorf(CodeNoNeighbourTarget,
			"the active edge %q has no neighbour target for inbound %q to follow; set its realityTarget first", edge.Name, inbound.Remark)
	}
	stream, _, err := withNeighbourTarget(inbound.StreamSettings, edge.RealityTarget, edge.NeighbourServerName())
	if err != nil {
		return err
	}
	inbound.StreamSettings = stream
	return nil
}

// isReality reports whether stream settings put Reality on the inbound.
func isReality(streamSettings string) bool {
	var stream struct {
		Security string `json:"security"`
	}
	return json.Unmarshal([]byte(streamSettings), &stream) == nil && stream.Security == "reality"
}

// FollowingInbounds counts the chain-following inbounds, enabled or not. A
// switched-off one still counts: its links are out there, and they break on
// the next switch the moment it is switched back on.
func (s *ChainService) FollowingInbounds() (int64, error) {
	db := database.GetDB()
	if db == nil {
		return 0, nil
	}
	var count int64
	err := db.Model(&model.Inbound{}).Where("follow_chain = ?", true).Count(&count).Error
	return count, err
}
