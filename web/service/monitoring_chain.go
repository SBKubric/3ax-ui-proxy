package service

import (
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"

	"gorm.io/gorm"
)

// Monitoring through every hop of the chain (docs/spec/proxy-chain.md §6,
// contract 3). The chain registry decides which paths the panel serves: with
// probed hops — joined or legacy, inner and edge alike — they are direct plus
// one path per hop, and proxy is gone; without them, direct and proxy as in
// contract 2. Everything here reads the registry through the caller's
// transaction when it has one: SQLite runs on a single connection, and a
// query of our own from inside an open transaction would wait on itself.

// MonChainHop is one probed hop as GET /state reports it.
type MonChainHop struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Host  string `json:"host"`
	State string `json:"state"`
}

// MonChain is the chain field of GET /state (contract §4.1): the registry
// revision, the active edge (nil when none is active) and the probed hops,
// inner fronts from the panel outward, then edges by name.
type MonChain struct {
	Revision   int64         `json:"revision"`
	ActiveEdge *string       `json:"activeEdge"`
	Hops       []MonChainHop `json:"hops"`
}

// monHopProbed reports whether mon-server probes a hop in this state:
// pending has no relay yet and draining is on its way out (§6.1).
func monHopProbed(state string) bool {
	return state == chain.StateJoined || state == chain.StateLegacy
}

// monHopPath is the path of a hop: edge:<name> or inner:<name> by its role.
func monHopPath(role, name string) string {
	if role == chain.RoleInner {
		return monPathInnerPrefix + name
	}
	return monPathEdgePrefix + name
}

// monChainTx reads the registry as the contract shows it, or nil when the
// registry is empty. A nil tx means the panel's own connection.
func monChainTx(tx *gorm.DB) (*MonChain, error) {
	if tx == nil {
		tx = database.GetDB()
	}
	var rows []model.ChainHop
	if err := tx.Model(&model.ChainHop{}).
		Order("CASE role WHEN '" + chain.RoleInner + "' THEN 0 ELSE 1 END, " +
			"CASE role WHEN '" + chain.RoleInner + "' THEN position ELSE 0 END, name, id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	revision, err := revisionTx(tx)
	if err != nil {
		return nil, err
	}
	out := &MonChain{Revision: revision, Hops: []MonChainHop{}}
	for _, hop := range rows {
		if hop.IsActive && out.ActiveEdge == nil {
			name := hop.Name
			out.ActiveEdge = &name
		}
		if monHopProbed(hop.State) {
			out.Hops = append(out.Hops, MonChainHop{Name: hop.Name, Role: hop.Role, Host: hop.Host, State: hop.State})
		}
	}
	return out, nil
}

// probed reports whether the chain has anything the revision hashes: a
// probed hop or an active edge. A registry of pending hops alone hashes like
// an empty one, because entering the registry without a confirmed join must
// not rebuild targets (proxy-chain.md §6.1).
func (c *MonChain) probed() bool {
	return c != nil && (len(c.Hops) > 0 || c.ActiveEdge != nil)
}

// revisionMaterial is the chain as the revision hashes it (contract §4.2):
// the active edge and the probed hops, without the registry revision, as
// maps so the canonical JSON sorts their keys like everything else.
func (c *MonChain) revisionMaterial() map[string]any {
	hops := make([]map[string]any, 0, len(c.Hops))
	for _, h := range c.Hops {
		hops = append(hops, map[string]any{"name": h.Name, "role": h.Role, "host": h.Host, "state": h.State})
	}
	return map[string]any{"activeEdge": c.ActiveEdge, "hops": hops}
}

// monProbedPaths is the probed set of paths (contract §3) in the priority
// order AmneziaWG probe peers are handed out in (§4.3): direct, the active
// edge, the standby edges by name, the inner fronts from the panel outward.
// Without probed hops it is direct and proxy.
func monProbedPaths(c *MonChain) []string {
	if c == nil || len(c.Hops) == 0 {
		return []string{model.MonPathDirect, model.MonPathProxy}
	}
	paths := []string{model.MonPathDirect}
	var standby, inner []string
	for _, h := range c.Hops {
		path := monHopPath(h.Role, h.Name)
		switch {
		case h.Role == chain.RoleInner:
			inner = append(inner, path)
		case c.ActiveEdge != nil && h.Name == *c.ActiveEdge:
			paths = append(paths, path)
		default:
			standby = append(standby, path)
		}
	}
	paths = append(paths, standby...)
	return append(paths, inner...)
}

// monProbePair is one pair of mon-client × path that gets an AmneziaWG
// probe peer.
type monProbePair struct {
	MonClientId string
	Path        string
}

// monPathHops is the paths word for every probed hop.
const monPathHops = "hops"

// monClientPaths expands a mon-client's paths over the probed set: direct;
// hops — every probed hop, or proxy while there is none; any path of the set
// named explicitly. A name outside the set (an unknown hop, a pending one,
// proxy once hops are probed) gets nothing. Absent paths mean the default,
// direct and hops.
func monClientPaths(paths []string, probed []string) map[string]bool {
	if paths == nil {
		paths = []string{model.MonPathDirect, monPathHops}
	}
	inSet := map[string]bool{}
	for _, p := range probed {
		inSet[p] = true
	}
	out := map[string]bool{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p != monPathHops {
			if inSet[p] {
				out[p] = true
			}
			continue
		}
		for _, q := range probed {
			if q != model.MonPathDirect {
				out[q] = true
			}
		}
	}
	return out
}

// monProbePairs lists the pairs of mon-client × path of the snapshot in the
// order AmneziaWG probe peers are handed out (contract 3 §4.3): by path in
// the priority order of monProbedPaths, then by monClientId. Duplicate
// mon-clients count once.
func monProbePairs(snapshot []MonClient, probed []string) []monProbePair {
	clients := sortedMonClients(snapshot)
	wants := make([]map[string]bool, len(clients))
	for i, mc := range clients {
		wants[i] = monClientPaths(mc.Paths, probed)
	}
	pairs := []monProbePair{}
	for _, path := range probed {
		for i, mc := range clients {
			if wants[i][path] {
				pairs = append(pairs, monProbePair{MonClientId: mc.Id, Path: path})
			}
		}
	}
	return pairs
}

// pruneMonTargetsTx is the panel's half of a change to the probed set
// (proxy-chain.md §6.1, contract §6): a hop deleted or renamed, gone out of
// joined/legacy, the first probed hop appearing (proxy goes) or the last one
// leaving (proxy comes back). It deletes, in the registry write's own
// transaction, every mon_targets row whose path is no longer in the set.
// Events and aggregates age out with the ordinary retention; nothing is sent
// to Telegram. Every registry write calls it: working out the set again is
// cheaper than working out which writes change it.
func pruneMonTargetsTx(tx *gorm.DB) error {
	chain, err := monChainTx(tx)
	if err != nil {
		return err
	}
	return tx.Where("path NOT IN ?", monProbedPaths(chain)).Delete(&model.MonTarget{}).Error
}

// deleteMonitoringByHopTx drops the stored monitoring of a hop leaving the
// registry — targets, events and both aggregate tables of its path, and of
// path proxy when it was the active edge — in the transaction that deletes
// the row, as the inbound cascade does (proxy-chain.md §6.1). A rename is not
// a deletion: the old name's history ages out with the ordinary retention.
func deleteMonitoringByHopTx(tx *gorm.DB, hop *model.ChainHop) error {
	paths := []string{monHopPath(hop.Role, hop.Name)}
	if hop.IsActive {
		paths = append(paths, model.MonPathProxy)
	}
	for _, m := range []any{&model.MonTarget{}, &model.MonEvent{}, &model.MonStatsCurrent{}, &model.MonStatsRollup{}} {
		if err := tx.Where("path IN ?", paths).Delete(m).Error; err != nil {
			return err
		}
	}
	return nil
}

// Badge states of a hop beyond the target states: NONE for a hop nobody
// probes or has data on yet (grey "no data", not a target's UNKNOWN), STALE
// while the panel has not heard from mon-server (it overrides everything).
const (
	MonHopHealthNone  = "NONE"
	MonHopHealthStale = "STALE"
)

// MonHopHealth is one hop's badge in the chain editor and on the Monitoring
// page (proxy-chain.md §6.4). Active marks the active edge, which the
// Monitoring page's filter chips and summary line single out.
type MonHopHealth struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	State  string `json:"state"`
	Active bool   `json:"active"`
}

// WorstLiveHopState folds every target of one hop's path — all inbounds at
// once, live mon-clients only — into one state, as WorstLiveTargetState does
// for an inbound. Empty when the hop has no live target.
func (s *MonitoringService) WorstLiveHopState(hopName, role string) (string, error) {
	return s.worstLiveState(database.GetDB().Model(&model.MonTarget{}).Where("path = ?", monHopPath(role, hopName)))
}

// HopsHealth is GET /panel/api/chain/hops/health: one badge per hop of the
// registry, in the order given. A hop that is not probed (pending, draining)
// or has no live target yet is NONE; a probed hop is STALE while the panel
// is; otherwise its WorstLiveHopState.
func (s *MonitoringService) HopsHealth(hops []model.ChainHop) ([]MonHopHealth, error) {
	stale, _ := s.IsMonStale()
	out := make([]MonHopHealth, 0, len(hops))
	for _, hop := range hops {
		state := MonHopHealthNone
		if monHopProbed(hop.State) {
			worst, err := s.WorstLiveHopState(hop.Name, hop.Role)
			if err != nil {
				return nil, err
			}
			if worst != "" {
				state = worst
			}
			if stale {
				state = MonHopHealthStale
			}
		}
		out = append(out, MonHopHealth{Name: hop.Name, Role: hop.Role, State: state, Active: hop.IsActive})
	}
	return out, nil
}

// MonStandbyEdge is one standby edge as the Telegram hint lists it: its
// badge state over all inbounds (WorstLiveHopState, NONE without data).
type MonStandbyEdge struct {
	Name  string
	State string
}

// Candidate tiers for a switch-over (proxy-chain.md §6.5), best first.
const (
	monStandbyAllUp      = iota // every inbound with data UP
	monStandbyMajorityUp        // strictly more than half of them UP
	monStandbyUnproven          // UNKNOWN, no data, or UP no better than half
	monStandbyDown              // no inbound UP and some DOWN: not a candidate
)

// StandbyHint is what the DOWN alert of the active edge adds (§6.5): every
// probed edge but the active one with its state, and the one to switch to —
// all inbounds UP beats a strict majority UP beats UNKNOWN or no data, ties
// by name; an edge with nothing UP and something DOWN is never proposed, and
// candidate is empty when no edge qualifies. ok is false when path is not
// the active edge's, and then there is no hint at all.
func (s *MonitoringService) StandbyHint(path string) (standby []MonStandbyEdge, candidate string, ok bool, err error) {
	c, err := monChainTx(nil)
	if err != nil || c == nil || c.ActiveEdge == nil || path != monHopPath(chain.RoleEdge, *c.ActiveEdge) {
		return nil, "", false, err
	}
	best := monStandbyDown
	for _, h := range c.Hops {
		if h.Role != chain.RoleEdge || h.Name == *c.ActiveEdge {
			continue
		}
		state, err := s.WorstLiveHopState(h.Name, h.Role)
		if err != nil {
			return nil, "", false, err
		}
		if state == "" {
			state = MonHopHealthNone
		}
		standby = append(standby, MonStandbyEdge{Name: h.Name, State: state})
		tier, err := s.standbyTier(monHopPath(h.Role, h.Name))
		if err != nil {
			return nil, "", false, err
		}
		// Hops come with the edges sorted by name, so the first of a tier
		// wins the tie.
		if tier < best {
			best, candidate = tier, h.Name
		}
	}
	return standby, candidate, true, nil
}

// standbyTier grades one edge's path by how many inbounds it carries: each
// inbound's targets on the path are folded over live mon-clients, then
// counted. The majority is strictly more than half; a tie is not one.
func (s *MonitoringService) standbyTier(path string) (int, error) {
	live := map[string]bool{}
	for _, c := range s.RegistrySnapshot() {
		live[c.Id] = true
	}
	var rows []model.MonTarget
	if err := database.GetDB().Where("path = ?", path).Find(&rows).Error; err != nil {
		return 0, err
	}
	byInbound := map[MonInboundRef][]string{}
	for _, r := range rows {
		if live[r.MonClientId] {
			ref := MonInboundRef{r.InboundKind, r.InboundId}
			byInbound[ref] = append(byInbound[ref], r.State)
		}
	}
	n, up, down := len(byInbound), 0, false
	for _, states := range byInbound {
		switch worstOfStates(states) {
		case model.MonStateUp:
			up++
		case model.MonStateDown:
			down = true
		}
	}
	switch {
	case n > 0 && up == n:
		return monStandbyAllUp, nil
	case 2*up > n:
		return monStandbyMajorityUp, nil
	case up == 0 && down:
		return monStandbyDown, nil
	}
	return monStandbyUnproven, nil
}
