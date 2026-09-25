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
