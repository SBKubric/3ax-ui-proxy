package service

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"

	"gorm.io/gorm"
)

// ChainService is the only way into the chain registry (docs/spec/proxy-chain.md
// §2). Everything that writes — the panel API, the bot, the join flow, the
// migration — goes through here, because the invariants of §2.7 and the
// revision of §3.4 only hold if one place owns them.
//
// The panel never calls a box: a write is a new revision, and the boxes come
// and fetch it themselves (§2.1). So there is nothing asynchronous here — each
// method is one transaction and returns.
type ChainService struct {
	settingService SettingService
}

// The legacy host override is imported into the registry at the end of
// InitDB's migrations. It is registered rather than called from database/,
// which cannot import this package without an import cycle (§2.3).
func init() {
	database.RegisterPostMigrate((&ChainService{}).MigrateLegacyOverride)
}

// Error codes. They are part of the API: the controller puts them in the
// message, and the UI and the bot key their wording off them, so they are
// stable snake_case strings rather than prose.
const (
	CodeUnknownHop       = "unknown_hop"
	CodeNameTaken        = "name_taken"
	CodeInvalidName      = "invalid_name"
	CodeInvalidHost      = "invalid_host"
	CodeInvalidRole      = "invalid_role"
	CodeInvalidSubPort   = "invalid_sub_port"
	CodeInvalidSubScheme = "invalid_sub_scheme"
	CodeInvalidPosition  = "invalid_position"
	CodeNotAnEdge        = "not_an_edge"
	CodeHopNotJoined     = "hop_not_joined"
	CodeActiveEdgeInUse  = "active_edge_in_use"
	CodeFieldImmutable   = "field_immutable"
)

// Defaults for a hop the owner did not spell out: the panel's own sub port and
// TLS, which is what a freshly installed box listens on.
const (
	defaultHopSubPort   = 2096
	defaultHopSubScheme = "https"
)

// ChainError is a refusal with a stable code. The registry never repairs a
// broken request quietly — it says which rule it broke.
type ChainError struct {
	Code    string
	Message string
}

func (e *ChainError) Error() string {
	return e.Code + ": " + e.Message
}

func chainErrorf(code, format string, args ...any) error {
	return &ChainError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ChainErrorCode returns the stable code of a registry refusal, or "" for any
// other error.
func ChainErrorCode(err error) string {
	var chainErr *ChainError
	if errors.As(err, &chainErr) {
		return chainErr.Code
	}
	return ""
}

// ChainState is the whole registry as the UI and the bot read it (§2.4).
type ChainState struct {
	Revision    int64            `json:"revision"`
	ActiveEdge  string           `json:"activeEdge"`
	PollSeconds int              `json:"pollSeconds"`
	Hops        []model.ChainHop `json:"hops"`

	// Draining is one card per departing hop (§4.5): the editor needs the
	// names it is still waiting for, which the hop row itself does not carry.
	Draining []DrainingHop `json:"draining"`
}

// AddHopInput describes a hop the owner is creating. Position only means
// something for an inner front; an edge always hangs off the last inner, which
// the service works out itself.
type AddHopInput struct {
	Name      string
	Host      string
	Role      string
	SubPort   int
	SubScheme string
	Position  *int

	// The neighbour target of an edge (ADR 0005), both optional: the
	// orchestrator usually writes them later through Update, once it has
	// found a site next to the edge's address.
	RealityTarget     string
	RealityServerName string
}

// UpdateHopInput changes what the owner is allowed to change on an existing
// hop (§2.4). Role, position, state and activity are not in here: they move
// through Add, Delete and SetActive, which keep the invariants.
type UpdateHopInput struct {
	Name      *string
	Host      *string
	SubPort   *int
	SubScheme *string

	// An empty RealityTarget removes the neighbour target, server name
	// included.
	RealityTarget     *string
	RealityServerName *string
}

// List returns the registry with the current revision and poll interval.
func (s *ChainService) List() (*ChainState, error) {
	revision, err := s.settingService.GetChainRevision()
	if err != nil {
		return nil, err
	}
	poll, err := s.settingService.GetChainPollSeconds()
	if err != nil {
		return nil, err
	}
	hops, err := orderedHops(database.GetDB())
	if err != nil {
		return nil, err
	}
	draining, err := drainingCards(database.GetDB(), hops)
	if err != nil {
		return nil, err
	}
	state := &ChainState{Revision: revision, PollSeconds: poll, Hops: hops, Draining: draining}
	for _, hop := range hops {
		if hop.IsActive {
			state.ActiveEdge = hop.Name
			break
		}
	}
	return state, nil
}

// Add creates a hop in state pending and returns its join token in the clear —
// the only time the panel ever shows it (§4.1).
//
// The revision does not move: a pending hop appears in no document, so nothing
// a box could fetch has changed. That is what makes inserting an inner safe at
// any distance in time from installing its box (§2.6.3) — the outer neighbour
// is re-chained onto it only when it really joins.
func (s *ChainService) Add(in AddHopInput) (*model.ChainHop, string, int64, error) {
	name, err := validName(in.Name)
	if err != nil {
		return nil, "", 0, err
	}
	host, err := validHost(in.Host)
	if err != nil {
		return nil, "", 0, err
	}
	if in.Role != chain.RoleInner && in.Role != chain.RoleEdge {
		return nil, "", 0, chainErrorf(CodeInvalidRole, "role must be %q or %q, got %q", chain.RoleInner, chain.RoleEdge, in.Role)
	}
	subPort, err := validSubPort(in.SubPort)
	if err != nil {
		return nil, "", 0, err
	}
	subScheme, err := validSubScheme(in.SubScheme)
	if err != nil {
		return nil, "", 0, err
	}
	realityTarget, realityServerName, err := neighbourTarget(in.RealityTarget, in.RealityServerName)
	if err != nil {
		return nil, "", 0, err
	}

	token := chain.NewSecret()
	expires, err := s.joinTokenExpiry()
	if err != nil {
		return nil, "", 0, err
	}

	hop := &model.ChainHop{
		Name:              name,
		Host:              host,
		Role:              in.Role,
		SubPort:           subPort,
		SubScheme:         subScheme,
		RealityTarget:     realityTarget,
		RealityServerName: realityServerName,
		State:             chain.StatePending,
		JoinTokenHash:     chain.HashSecret(token),
		JoinTokenExpires:  expires,
	}
	if err := neighbourOnlyOnEdges(hop, in.Role); err != nil {
		return nil, "", 0, err
	}

	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := nameIsFree(tx, name, 0); err != nil {
			return err
		}
		if in.Role == chain.RoleInner {
			inners, err := innerHops(tx)
			if err != nil {
				return err
			}
			position := len(inners)
			if in.Position != nil {
				position = *in.Position
			}
			if position < 0 || position > len(inners) {
				return chainErrorf(CodeInvalidPosition, "position %d is outside 0..%d", position, len(inners))
			}
			// Everything from the insertion point outward steps one place out;
			// reconcile then re-chains next_hop_id along the new order.
			if err := tx.Model(&model.ChainHop{}).
				Where("role = ? AND position >= ?", chain.RoleInner, position).
				UpdateColumn("position", gorm.Expr("position + 1")).Error; err != nil {
				return err
			}
			hop.Position = position
		}
		if err := tx.Create(hop).Error; err != nil {
			return err
		}
		if err := reconcileTopology(tx); err != nil {
			return err
		}
		// reconcileTopology writes next_hop_id through its own copies of the
		// rows, not through hop, so hop is reloaded to pick up whatever it
		// computed — the same value list would show for this hop right away,
		// and add's own answer must not lag one call behind it.
		reloaded, err := loadHop(tx, hop.Id)
		if err != nil {
			return err
		}
		hop = reloaded
		return nil
	})
	if err != nil {
		return nil, "", 0, err
	}
	return hop, token, expires, nil
}

// Update changes the name, host, sub port or sub scheme of a hop. Any of them
// changes the outer neighbour's document, so the revision moves — but only if
// something really changed, because a spurious bump restarts relays for
// nothing (§3.5).
func (s *ChainService) Update(id int, in UpdateHopInput) error {
	followers := 0
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		hop, err := loadHop(tx, id)
		if err != nil {
			return err
		}
		if err := refuseIfDraining(hop); err != nil {
			return err
		}
		changed := false
		if in.Name != nil {
			name, err := validName(*in.Name)
			if err != nil {
				return err
			}
			if name != hop.Name {
				if err := nameIsFree(tx, name, hop.Id); err != nil {
					return err
				}
				hop.Name, changed = name, true
			}
		}
		if in.Host != nil {
			host, err := validHost(*in.Host)
			if err != nil {
				return err
			}
			if host != hop.Host {
				hop.Host, changed = host, true
			}
		}
		if in.SubPort != nil {
			subPort, err := validSubPort(*in.SubPort)
			if err != nil {
				return err
			}
			if subPort != hop.SubPort {
				hop.SubPort, changed = subPort, true
			}
		}
		if in.SubScheme != nil {
			subScheme, err := validSubScheme(*in.SubScheme)
			if err != nil {
				return err
			}
			if subScheme != hop.SubScheme {
				hop.SubScheme, changed = subScheme, true
			}
		}
		neighbourChanged, err := updateNeighbourTarget(hop, in)
		if err != nil {
			return err
		}
		if !changed && !neighbourChanged {
			return nil
		}
		if err := tx.Save(hop).Error; err != nil {
			return err
		}
		// The active edge's neighbour is what the followers imitate right
		// now, so a new one reaches them in this same write (ADR 0005).
		if neighbourChanged && hop.IsActive {
			rewritten, err := followEdgeTx(tx, hop)
			if err != nil {
				return err
			}
			followers = rewritten
		}
		if err := pruneMonTargetsTx(tx); err != nil {
			return err
		}
		return bumpRevisionTx(tx)
	})
	if err != nil {
		return err
	}
	chainFollowersRestart(followers)
	return nil
}

// updateNeighbourTarget applies the neighbour target half of an update to hop
// and reports whether it changed. A target sent empty removes the neighbour,
// server name included; a server name sent alone keeps the stored target.
func updateNeighbourTarget(hop *model.ChainHop, in UpdateHopInput) (bool, error) {
	if in.RealityTarget == nil && in.RealityServerName == nil {
		return false, nil
	}
	target, serverName := hop.RealityTarget, hop.RealityServerName
	if in.RealityTarget != nil {
		target = *in.RealityTarget
		if strings.TrimSpace(target) == "" {
			serverName = ""
		}
	}
	if in.RealityServerName != nil {
		serverName = *in.RealityServerName
	}
	target, serverName, err := neighbourTarget(target, serverName)
	if err != nil {
		return false, err
	}
	if target == hop.RealityTarget && serverName == hop.RealityServerName {
		return false, nil
	}
	hop.RealityTarget, hop.RealityServerName = target, serverName
	return true, neighbourOnlyOnEdges(hop, hop.Role)
}

// Delete takes a hop out of the chain (§4.5). Which of the two deletes it
// performs depends on whether anything still hangs off that hop:
//
//   - nothing does, or skipDrain was asked for: the row and the hop secret go
//     at once. That is every edge, every brand-new pending hop, every legacy
//     hop and the last inner — there is nobody left to serve.
//   - something does: the row stays as draining. The neighbours are re-chained
//     past it in this transaction, but the hop keeps its host, its secret and
//     its own next hop, because it is the only channel that can carry this
//     very revision to them (§4.5.2). SweepDraining drops the row once they
//     have confirmed it, or once chainDrainMinutes have passed.
//
// The active edge is refused unless force is set, and force is only allowed
// when no other joined edge exists — that is decommissioning the chain, not
// switching over. Handing the override to a standby by itself would be an
// automatic failover, which is out of scope; disabling it quietly would
// publish the real server's address, which is the one thing the whole
// construction hides (§2.6.4).
func (s *ChainService) Delete(id int, force, skipDrain bool) (*DeleteResult, error) {
	result := &DeleteResult{}
	// The setting is read before the transaction opens: this SQLite runs on a
	// single connection, and a settings read inside would wait on itself.
	minutes := s.drainMinutes()
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		hop, err := loadHop(tx, id)
		if err != nil {
			return err
		}
		result.Hop = hop.Name

		// A second del on a hop that is already draining changes nothing and
		// moves no revision: it answers with the card the first one returned,
		// because extending a departure would make the deadline meaningless
		// (§4.5.1). skipDrain is the exception the runbook uses — it ends the
		// departure now, which is what an owner who needs the name back asks
		// for.
		if hop.State == chain.StateDraining {
			if !skipDrain {
				result.State = DeleteStateDraining
				result.DrainRevision = hop.DrainRevision
				result.DrainUntil = hop.DrainUntil
				result.SafeToPowerOffWhen = SafeToPowerOff{
					Hops:     drainOuterNames(hop.DrainOuter),
					Revision: hop.DrainRevision,
				}
				return nil
			}
		}
		if hop.IsActive {
			var others int64
			if err := tx.Model(&model.ChainHop{}).
				Where("id <> ? AND role = ? AND state IN ?", hop.Id, chain.RoleEdge,
					[]string{chain.StateJoined, chain.StateLegacy}).
				Count(&others).Error; err != nil {
				return err
			}
			switch {
			case others > 0:
				return chainErrorf(CodeActiveEdgeInUse,
					"%q is the active edge; make another edge active before deleting it", hop.Name)
			case !force:
				return chainErrorf(CodeActiveEdgeInUse,
					"%q is the active edge and the last one; deleting it publishes the real server address, so it needs force", hop.Name)
			default:
				logger.Warning("chain: override disabled, the real server address is now published")
			}
		}

		// Everything that still hangs off this hop and still has a box of its
		// own is what the departure exists for.
		outer, err := liveOuterNeighbours(tx, hop.Id)
		if err != nil {
			return err
		}

		if len(outer) > 0 && !skipDrain {
			revision, until, names, err := startDrainingTx(tx, hop, outer, minutes)
			if err != nil {
				return err
			}
			result.State = DeleteStateDraining
			result.DrainRevision = revision
			result.DrainUntil = until
			result.SafeToPowerOffWhen = SafeToPowerOff{Hops: names, Revision: revision}
			return nil
		}

		if err := tx.Delete(&model.ChainHop{}, hop.Id).Error; err != nil {
			return err
		}
		if err := deleteMonitoringByHopTx(tx, hop); err != nil {
			return err
		}
		if err := pruneMonTargetsTx(tx); err != nil {
			return err
		}
		if err := tx.Model(&model.ChainHop{}).Where("next_hop_id = ?", hop.Id).
			Updates(map[string]any{"next_hop_id": hop.NextHopId}).Error; err != nil {
			return err
		}
		if err := reconcileTopology(tx); err != nil {
			return err
		}
		if err := bumpRevisionTx(tx); err != nil {
			return err
		}
		revision, err := revisionTx(tx)
		if err != nil {
			return err
		}
		// The secret died with the row, so there is nobody left to wait for:
		// the answer says deleted, with no deadline and an empty list (§4.5.1).
		result.State = DeleteStateDeleted
		result.SafeToPowerOffWhen = SafeToPowerOff{Hops: []string{}, Revision: revision}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// SetActive makes one joined edge the active one — the single registry write
// behind /proxy <name> and the switch button. Nothing changes on any box
// (§4.7): the only consequence is which host the panel publishes.
func (s *ChainService) SetActive(id int) error {
	followers := 0
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		hop, err := loadHop(tx, id)
		if err != nil {
			return err
		}
		if err := refuseIfDraining(hop); err != nil {
			return err
		}
		if hop.Role != chain.RoleEdge {
			return chainErrorf(CodeNotAnEdge, "%q is an %s front; only an edge can be active", hop.Name, hop.Role)
		}
		if hop.State != chain.StateJoined && hop.State != chain.StateLegacy {
			return chainErrorf(CodeHopNotJoined, "%q is %s; only a hop that has entered the chain can be active", hop.Name, hop.State)
		}
		if hop.IsActive {
			return nil
		}
		// The inbounds follow the edge in the same transaction: a switch that
		// moved the host but left them on the old edge's neighbour would give
		// clients an address in one network and a cover site in another.
		changed, err := followEdgeTx(tx, hop)
		if err != nil {
			return err
		}
		followers = changed
		if err := tx.Model(&model.ChainHop{}).Where("is_active = ?", true).
			Update("is_active", false).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ChainHop{}).Where("id = ?", hop.Id).
			Update("is_active", true).Error; err != nil {
			return err
		}
		return bumpRevisionTx(tx)
	})
	if err != nil {
		return err
	}
	chainFollowersRestart(followers)
	return nil
}

// ClearActive turns the override off without deleting anything: what /proxy
// off means once the chain exists. With no active edge the panel publishes the
// real server's address again, so this is as loud a step as a forced delete.
func (s *ChainService) ClearActive() error {
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.ChainHop{}).Where("is_active = ?", true).Update("is_active", false)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		logger.Warning("chain: override disabled, the real server address is now published")
		// Owner decision on #139: the followers stay as the last active edge
		// left them, so their links keep working with only the host changed.
		// Their cover now sits in another network than the address clients
		// reach, which is worth a line in the log.
		var followers int64
		if err := tx.Model(&model.Inbound{}).Where("follow_chain = ?", true).Count(&followers).Error; err != nil {
			return err
		}
		if followers > 0 {
			logger.Warningf("chain: %d chain-following inbound(s) keep the last active edge's neighbour target", followers)
		}
		return bumpRevisionTx(tx)
	})
}

// ReissueToken hands out a new join token for an existing hop; the old one is
// dead the moment the hash is overwritten (§4.1).
//
// The hop goes back to pending, whichever state it was in: in v1 there is no
// way to rotate a hop secret without entering again (§4.2). That includes the
// imported legacy hop — a token for it exists precisely so a real box can take
// the hand-set host over, and once it enters it is an ordinary hop like any
// other (§2.3). Its secret hash stays until the new box really joins, so the
// old box keeps receiving its document until it is replaced (§4.4).
//
// is_active is untouched, and so is the revision: nothing in any document has
// changed yet, and the panel keeps publishing the hop's host throughout
// (ActiveEdgeHost).
func (s *ChainService) ReissueToken(id int) (string, int64, error) {
	token := chain.NewSecret()
	expires, err := s.joinTokenExpiry()
	if err != nil {
		return "", 0, err
	}
	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		hop, err := loadHop(tx, id)
		if err != nil {
			return err
		}
		if err := refuseIfDraining(hop); err != nil {
			return err
		}
		hop.JoinTokenHash = chain.HashSecret(token)
		hop.JoinTokenExpires = expires
		hop.State = chain.StatePending
		if err := tx.Save(hop).Error; err != nil {
			return err
		}
		// Back in pending the hop is no longer probed (§6.1).
		return pruneMonTargetsTx(tx)
	})
	if err != nil {
		return "", 0, err
	}
	return token, expires, nil
}

// MarkJoined is the registry half of a join (§4.3): the hop takes its hop
// secret's hash, spends its join token and becomes part of every document from
// this revision on. The join flow of ticket #81 calls it once it has checked
// the token.
func (s *ChainService) MarkJoined(id int, secretHash, observedAddr string) error {
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		hop, err := loadHop(tx, id)
		if err != nil {
			return err
		}
		return markJoinedTx(tx, hop, secretHash, observedAddr)
	})
}

// markJoinedTx is MarkJoined on a hop already loaded inside a transaction: the
// join flow (chain_join.go) finds its hop by token hash and may have changed
// the address the box reported, and all of that has to land in the same
// transaction as the state, the secret and the revision.
func markJoinedTx(tx *gorm.DB, hop *model.ChainHop, secretHash, observedAddr string) error {
	now := time.Now().UnixMilli()
	hop.State = chain.StateJoined
	if secretHash != "" {
		hop.SecretHash = secretHash
	}
	if observedAddr != "" {
		hop.ObservedAddr = observedAddr
	}
	hop.JoinTokenHash = ""
	hop.JoinTokenExpires = 0
	hop.JoinedAt = now
	hop.LastSeenAt = now
	if err := tx.Save(hop).Error; err != nil {
		return err
	}
	if err := reconcileTopology(tx); err != nil {
		return err
	}
	// A joined hop is probed: the first one takes path proxy away (§6.1).
	if err := pruneMonTargetsTx(tx); err != nil {
		return err
	}
	return bumpRevisionTx(tx)
}

// RecordSeen notes that a hop confirmed a revision. It is the one write that
// happens constantly and must never move the revision itself (§3.4), or the
// chain would chase its own tail.
func (s *ChainService) RecordSeen(id int, revision int64) error {
	return database.GetDB().Model(&model.ChainHop{}).Where("id = ?", id).
		Updates(map[string]any{"last_seen_at": time.Now().UnixMilli(), "last_revision": revision}).Error
}

// ActiveEdgeHost is the address the panel publishes: the host of whichever hop
// carries is_active, whatever state it is in. Only when no hop is active does
// the override fall back to the legacy keys.
//
// This departs deliberately from the wording of §2.3 ("state IN (joined,
// legacy)"). Reissuing the token of the active edge puts it back to pending
// while keeping it active (§4.4), and a state filter here would drop the
// override for the whole re-join window — which does not publish nothing, it
// publishes the real server's address, the one thing the chain exists to hide.
// The host is the one the owner typed and is still the address clients reach,
// so an active pending hop is the right answer, not a reason to fall back.
// Making a pending hop active is still refused (SetActive, invariant 1); this
// is only about a hop that was already active.
func (s *ChainService) ActiveEdgeHost() (string, bool) {
	db := database.GetDB()
	if db == nil {
		return "", false
	}
	var hop model.ChainHop
	err := db.Where("is_active = ?", true).First(&hop).Error
	if err != nil {
		return "", false
	}
	host := strings.TrimSpace(hop.Host)
	return host, host != ""
}

// BumpRevision moves the registry to a new revision, inside tx when one is
// given. Callers that change what a document contains — the port composition
// hooks of §3.4, for instance — use it directly.
func (s *ChainService) BumpRevision(tx *gorm.DB) error {
	if tx == nil {
		tx = database.GetDB()
	}
	return bumpRevisionTx(tx)
}

// MigrateLegacyOverride imports a panel that had a single hand-set
// proxyOverrideHost into the registry as one hop named legacy (§2.3), so the
// address the panel publishes keeps coming from the same place for everyone.
//
// It runs on every start and does nothing at all unless the registry is empty
// and the legacy host is set. is_active mirrors proxyOverrideEnable: an
// override that was off must not switch itself on during an upgrade.
func (s *ChainService) MigrateLegacyOverride() error {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	// The settings are read before the transaction opens: SQLite here runs on
	// a single connection, and a settings read from inside a transaction would
	// wait for a connection the transaction itself is holding.
	host, err := s.settingService.GetProxyOverrideHost()
	if err != nil {
		return err
	}
	if host = strings.TrimSpace(host); host == "" {
		return nil
	}
	enabled, err := s.settingService.GetProxyOverrideEnable()
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var hops int64
		if err := tx.Model(&model.ChainHop{}).Count(&hops).Error; err != nil {
			return err
		}
		if hops > 0 {
			return nil
		}
		now := time.Now().UnixMilli()
		hop := &model.ChainHop{
			Name:      "legacy",
			Host:      host,
			Role:      chain.RoleEdge,
			SubPort:   defaultHopSubPort,
			SubScheme: defaultHopSubScheme,
			State:     chain.StateLegacy,
			IsActive:  enabled,
			JoinedAt:  now,
		}
		if err := tx.Create(hop).Error; err != nil {
			return err
		}
		logger.Infof("chain: imported the legacy host override %s as hop %q", host, hop.Name)
		if err := pruneMonTargetsTx(tx); err != nil {
			return err
		}
		return bumpRevisionTx(tx)
	})
}

// joinTokenExpiry is now plus chainJoinTokenHours, in milliseconds.
func (s *ChainService) joinTokenExpiry() (int64, error) {
	hours, err := s.settingService.GetChainJoinTokenHours()
	if err != nil {
		return 0, err
	}
	if hours <= 0 {
		hours = 24
	}
	return time.Now().Add(time.Duration(hours) * time.Hour).UnixMilli(), nil
}

// reconcileTopology restores invariants 2, 3 and 5 from the current rows: the
// inner fronts form one path ordered by position with no gaps, and every edge
// hangs off the last inner that has actually entered the chain.
//
// Doing it as a sweep after each write, rather than patching next_hop_id at
// every call site, is what keeps "one path, no branches, no cycles" true by
// construction instead of by discipline.
func reconcileTopology(tx *gorm.DB) error {
	inners, err := innerHops(tx)
	if err != nil {
		return err
	}
	var previousId *int
	var lastEnteredId *int
	position := 0
	for index := range inners {
		hop := &inners[index]
		// A draining hop is not in the live path: it is nobody's next hop, it
		// does not shift anyone's position and it does not count as the last
		// inner an edge hangs off. It keeps its own position, next_hop_id and
		// secret untouched until its row goes (§2.7 invariant 7).
		if hop.State == chain.StateDraining {
			continue
		}
		changed := false
		if hop.Position != position {
			hop.Position, changed = position, true
		}
		position++
		if !sameId(hop.NextHopId, previousId) {
			hop.NextHopId, changed = copyId(previousId), true
		}
		if changed {
			if err := tx.Save(hop).Error; err != nil {
				return err
			}
		}
		previousId = &hop.Id
		if hop.State == chain.StateJoined || hop.State == chain.StateLegacy {
			lastEnteredId = &hop.Id
		}
	}

	var edges []model.ChainHop
	if err := tx.Where("role = ? AND state <> ?", chain.RoleEdge, chain.StateDraining).
		Order("id").Find(&edges).Error; err != nil {
		return err
	}
	for index := range edges {
		edge := &edges[index]
		if sameId(edge.NextHopId, lastEnteredId) {
			continue
		}
		edge.NextHopId = copyId(lastEnteredId)
		if err := tx.Save(edge).Error; err != nil {
			return err
		}
	}
	return nil
}

func sameId(a, b *int) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func copyId(id *int) *int {
	if id == nil {
		return nil
	}
	value := *id
	return &value
}

func loadHop(tx *gorm.DB, id int) (*model.ChainHop, error) {
	var hop model.ChainHop
	err := tx.Where("id = ?", id).First(&hop).Error
	if database.IsNotFound(err) {
		return nil, chainErrorf(CodeUnknownHop, "no hop with id %d", id)
	}
	if err != nil {
		return nil, err
	}
	return &hop, nil
}

func innerHops(tx *gorm.DB) ([]model.ChainHop, error) {
	var inners []model.ChainHop
	err := tx.Where("role = ?", chain.RoleInner).Order("position, id").Find(&inners).Error
	return inners, err
}

// orderedHops lists the registry the way it reads: the inner path from the
// real server outward, then the edges.
func orderedHops(tx *gorm.DB) ([]model.ChainHop, error) {
	hops := []model.ChainHop{}
	if tx == nil {
		return hops, nil
	}
	// A draining inner keeps the position it had while the live inner outward
	// of it compacts into that same number, so the tie is broken in favour of
	// the departing hop: it still sits inward of the neighbours it serves, and
	// the truncation of §3.2 depends on that order.
	err := tx.Model(&model.ChainHop{}).
		Order("CASE role WHEN '" + chain.RoleInner + "' THEN 0 ELSE 1 END, position, " +
			"CASE state WHEN '" + chain.StateDraining + "' THEN 0 ELSE 1 END, id").
		Find(&hops).Error
	return hops, err
}

func nameIsFree(tx *gorm.DB, name string, exceptId int) error {
	var taken int64
	if err := tx.Model(&model.ChainHop{}).Where("name = ? AND id <> ?", name, exceptId).
		Count(&taken).Error; err != nil {
		return err
	}
	if taken > 0 {
		return chainErrorf(CodeNameTaken, "a hop named %q already exists", name)
	}
	return nil
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !chain.NameValid(name) {
		return "", chainErrorf(CodeInvalidName, "name %q must match [a-z0-9-]{1,32}", name)
	}
	return name, nil
}

func validHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", chainErrorf(CodeInvalidHost, "host must not be empty")
	}
	if len(host) > 255 {
		return "", chainErrorf(CodeInvalidHost, "host is longer than 255 characters")
	}
	return host, nil
}

func validSubPort(port int) (int, error) {
	if port == 0 {
		return defaultHopSubPort, nil
	}
	if port < 1 || port > 65535 {
		return 0, chainErrorf(CodeInvalidSubPort, "sub port %d is outside 1-65535", port)
	}
	return port, nil
}

func validSubScheme(scheme string) (string, error) {
	switch scheme = strings.TrimSpace(scheme); scheme {
	case "":
		return defaultHopSubScheme, nil
	case "http", "https":
		return scheme, nil
	}
	return "", chainErrorf(CodeInvalidSubScheme, "sub scheme %q must be http or https", scheme)
}

// bumpRevisionTx increments chainRevision inside the transaction that changed
// the registry, so a reader can never see a new registry with an old revision.
func bumpRevisionTx(tx *gorm.DB) error {
	var setting model.Setting
	err := tx.Where("key = ?", chainRevisionKey).First(&setting).Error
	if database.IsNotFound(err) {
		return tx.Create(&model.Setting{Key: chainRevisionKey, Value: "1"}).Error
	}
	if err != nil {
		return err
	}
	current, err := strconv.ParseInt(strings.TrimSpace(setting.Value), 10, 64)
	if err != nil {
		// A revision that cannot be read is worse than one that jumps: start
		// again from 1 rather than leave every box on a stale document.
		logger.Warningf("chain: chainRevision %q is not a number, restarting from 1", setting.Value)
		current = 0
	}
	setting.Value = strconv.FormatInt(current+1, 10)
	return tx.Save(&setting).Error
}

func revisionTx(tx *gorm.DB) (int64, error) {
	var setting model.Setting
	err := tx.Where("key = ?", chainRevisionKey).First(&setting).Error
	if database.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(setting.Value), 10, 64)
}
