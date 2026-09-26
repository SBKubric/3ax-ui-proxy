package service

import (
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"

	"gorm.io/gorm"
)

// ChainWaveService is the panel's end of the wave (docs/spec/proxy-chain.md
// §3.3): it says who is allowed to poll the panel, and it writes down how
// fresh every hop is from what the poll carries.
//
// Freshness never moves the revision: it changes constantly, and a chain whose
// revision moved on every poll would spend its life chasing its own tail
// (§3.4). A front report does, and only when it changes something (#140).
type ChainWaveService struct {
	settingService SettingService
}

// AuthenticateHop matches a bearer token against the hop secrets of the hops
// that poll the panel directly — the ones whose next hop is the panel itself,
// next_hop_id NULL. A hop deeper out presents a secret its own neighbour
// checks; to us it is a stranger, and gets the same bare 404 as a scanner.
//
// The comparison is constant-time and runs over every candidate rather than
// stopping at the first match, so the answer takes the same time whether the
// secret is wrong in the first byte or the last.
func (s *ChainWaveService) AuthenticateHop(bearer string) (*model.ChainHop, bool) {
	bearer = strings.TrimSpace(bearer)
	if len(bearer) != chain.SecretLength {
		return nil, false
	}
	db := database.GetDB()
	if db == nil {
		return nil, false
	}
	// draining is in the list on purpose (§3.3, §4.5.3): a departing first-tier
	// hop has to receive at least one more document — the one in which its own
	// self.state became draining — or it would go on naming itself to its
	// neighbours and the stand of #86 would repeat.
	var candidates []model.ChainHop
	err := db.Where("next_hop_id IS NULL AND secret_hash <> '' AND state IN ?",
		[]string{chain.StateJoined, chain.StateLegacy, chain.StateDraining}).Find(&candidates).Error
	if err != nil {
		return nil, false
	}
	presented := []byte(chain.HashSecret(bearer))
	match := -1
	for index := range candidates {
		if subtle.ConstantTimeCompare(presented, []byte(candidates[index].SecretHash)) == 1 {
			match = index
		}
	}
	if match < 0 {
		return nil, false
	}
	return &candidates[match], true
}

// RecordSeen writes down what a poll told us: the caller has applied
// seenRevision, and each entry of outer is a neighbour further out that the
// caller has heard from since its own last poll (§3.3).
//
// A hop may only speak for the hops outward of it — the ones in its own
// truncated document. Everything else is dropped: the acknowledgements are
// the one thing a box can put in the registry, and a seized front must not be
// able to report a hop it cannot even see as fresh, nor pull an inward
// neighbour's freshness about.
//
// Nothing is ever wound back and nothing may run ahead. Acknowledgements
// travel inward hop by hop, so a stale one can arrive behind a fresher one,
// and a hop that appeared to go backwards would read as an outage that never
// happened; a revision beyond the registry's own or a timestamp from a box
// with a fast clock is clamped, so a box cannot claim to be ahead of the
// panel.
func (s *ChainWaveService) RecordSeen(hopName string, seenRevision int64, outer []chain.OuterAck) error {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	// The settings read comes first, outside the transaction: this SQLite runs
	// on a single connection.
	revision, err := s.settingService.GetChainRevision()
	if err != nil {
		return err
	}
	outward, err := outwardOf(db, hopName)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	clamp := func(value, ceiling int64) int64 {
		if value > ceiling {
			return ceiling
		}
		return value
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := recordSeenTx(tx, hopName, clamp(seenRevision, revision), now); err != nil {
			return err
		}
		for _, ack := range outer {
			name := strings.TrimSpace(ack.Name)
			if _, allowed := outward[name]; !allowed {
				continue
			}
			if err := recordSeenTx(tx, name, clamp(ack.LastRevision, revision), clamp(ack.LastSeen, now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Confirmations are exactly what a departure waits for, so the sweep runs
	// where they land (§4.5.4). A sweep that fails is logged and not returned:
	// the poll that carried the acknowledgement has done its job either way.
	if err := (&ChainService{}).SweepDraining(); err != nil {
		logger.Warning("chain: sweeping draining hops after a poll:", err)
	}
	return nil
}

// outwardOf names the hops a caller may speak for: exactly the ones in its own
// document besides itself (§3.2). The truncation is the document's — the inner
// path outward of the caller plus the edges for an inner, nothing at all for
// an edge, whose neighbour is beside it rather than outward — and it is worked
// out from the registry here rather than by building a document, which would
// need the panel host the caller does not have to have given us.
func outwardOf(db *gorm.DB, hopName string) (map[string]struct{}, error) {
	outward := map[string]struct{}{}
	hops, err := orderedHops(db)
	if err != nil {
		return nil, err
	}
	entered := make([]model.ChainHop, 0, len(hops))
	for _, hop := range hops {
		if chainHopVisible(hop) {
			entered = append(entered, hop)
		}
	}
	for index, hop := range entered {
		if hop.Name != hopName {
			continue
		}
		if hop.Role == chain.RoleEdge {
			return outward, nil
		}
		for _, further := range entered[index+1:] {
			outward[further.Name] = struct{}{}
		}
		return outward, nil
	}
	return outward, nil
}

// recordSeenTx moves one hop's freshness forward, and only forward. A name
// that is not in the registry updates nothing: the wave carries whatever a box
// has heard, and a hop deleted meanwhile must not reappear.
func recordSeenTx(tx *gorm.DB, name string, revision, seenAt int64) error {
	updates := map[string]any{}
	if revision > 0 {
		updates["last_revision"] = gorm.Expr("MAX(last_revision, ?)", revision)
	}
	if seenAt > 0 {
		updates["last_seen_at"] = gorm.Expr("MAX(last_seen_at, ?)", seenAt)
	}
	if len(updates) == 0 {
		return nil
	}
	return tx.Model(&model.ChainHop{}).Where("name = ?", name).Updates(updates).Error
}

// RecordFront writes down what the polling hop, and the hops outward of it
// through their acknowledgements, said about their fronts (#140).
//
// A report that changes where a hop's sub server answers moves the hop's
// sub port and scheme in the registry and bumps the revision in the same
// transaction: the new revision is what makes the hop's outer neighbours fetch
// the document that sends them to 443, and what the box itself waits for
// before it closes the port they used to poll (proxy.OldSubPortNeeded). A
// report that changes nothing writes nothing.
//
// As with freshness, a hop speaks only for itself and the hops outward of it,
// and a report the registry could not hold is ignored.
func (s *ChainWaveService) RecordFront(hopName string, own *chain.FrontReport, outer []chain.OuterAck) error {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	reports := map[string]chain.FrontReport{}
	if own != nil && own.Valid() {
		reports[hopName] = *own
	}
	if len(outer) > 0 {
		outward, err := outwardOf(db, hopName)
		if err != nil {
			return err
		}
		for _, ack := range outer {
			name := strings.TrimSpace(ack.Name)
			if _, allowed := outward[name]; !allowed || ack.Front == nil || !ack.Front.Valid() {
				continue
			}
			reports[name] = *ack.Front
		}
	}
	if len(reports) == 0 {
		return nil
	}

	var moved []string
	err := db.Transaction(func(tx *gorm.DB) error {
		changed := false
		for name, report := range reports {
			var hop model.ChainHop
			if err := tx.Where("name = ?", name).First(&hop).Error; err != nil {
				if database.IsNotFound(err) {
					continue
				}
				return err
			}
			if hop.FrontMode == report.Mode && hop.SubPort == report.SubPort && hop.SubScheme == report.SubScheme {
				continue
			}
			if err := tx.Model(&model.ChainHop{}).Where("id = ?", hop.Id).Updates(map[string]any{
				"front_mode": report.Mode,
				"sub_port":   report.SubPort,
				"sub_scheme": report.SubScheme,
			}).Error; err != nil {
				return err
			}
			// Only the address matters to the documents; a box that merely
			// starts reporting what the registry already holds needs no
			// new revision.
			if hop.SubPort != report.SubPort || hop.SubScheme != report.SubScheme {
				changed = true
			}
			moved = append(moved, fmt.Sprintf("%s front %s on %s:%d", name, report.Mode, report.SubScheme, report.SubPort))
		}
		if changed {
			return bumpRevisionTx(tx)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, line := range moved {
		logger.Info("chain:", line)
	}
	return nil
}
