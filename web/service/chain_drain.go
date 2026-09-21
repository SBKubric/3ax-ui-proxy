package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"

	"gorm.io/gorm"
)

// Draining — the two halves of a hop's departure (docs/spec/proxy-chain.md
// §4.5). Deleting a hop that still has outer neighbours is not one event but
// two: the departure starts (the hop leaves the topology, its neighbours are
// re-chained past it) and the departure finishes (the row and the hop secret
// die). In between the hop stays in the registry as draining and keeps serving
// its former neighbours, because it is the only channel through which the very
// revision that re-chains them can reach them.
//
// The stand (#86) is what this exists for: deleting an inner killed its hop
// secret at once, and its outer neighbour — which received its documents only
// through it — never saw the revision that pointed it elsewhere. It froze on
// the previous revision and went on relaying into a hop that was gone.

// CodeHopIsDraining refuses every write on a hop that is on its way out.
// draining is terminal: there is no un-deleting, and a re-chained neighbour
// must not be dragged back (§2.7 invariant 8).
const CodeHopIsDraining = "hop_is_draining"

// The two values of DeleteResult.State: the row is gone, or it is draining.
const (
	DeleteStateDeleted  = "deleted"
	DeleteStateDraining = "draining"
)

// defaultChainDrainMinutes is what a panel whose setting cannot be read falls
// back to, so a missing key can never mean "drain forever" or "do not drain".
const defaultChainDrainMinutes = 10

// DeleteResult is the answer of POST del/:id (§4.5.1). It says which of the two
// deletes happened and, when the hop is draining, what the owner has to wait
// for before powering that box off: every former outer neighbour has to
// confirm the revision the departure started in.
type DeleteResult struct {
	State              string         `json:"state"`
	Hop                string         `json:"hop"`
	DrainRevision      int64          `json:"drainRevision"`
	DrainUntil         int64          `json:"drainUntil"`
	SafeToPowerOffWhen SafeToPowerOff `json:"safeToPowerOffWhen"`
}

// SafeToPowerOff names every hop that has to confirm Revision. It is a list,
// not one name: while a hop drains, all of its former neighbours have to
// re-chain, and waiting for whichever one the panel happened to pick would say
// "safe" while another still relays into the box.
type SafeToPowerOff struct {
	Hops     []string `json:"hops"`
	Revision int64    `json:"revision"`
}

// DrainingHop is one departing hop as the editor shows it (§4.5.4): who it is
// waiting for, and until when.
type DrainingHop struct {
	Name          string   `json:"name"`
	DrainRevision int64    `json:"drainRevision"`
	DrainUntil    int64    `json:"drainUntil"`
	Outer         []string `json:"outer"`
	Waiting       []string `json:"waiting"`
}

// drainMinutes is chainDrainMinutes, never zero or negative: a deadline in the
// past would finish every departure on its first sweep, which is the stand's
// bug with extra steps.
func (s *ChainService) drainMinutes() int {
	minutes, err := s.settingService.GetChainDrainMinutes()
	if err != nil || minutes <= 0 {
		return defaultChainDrainMinutes
	}
	return minutes
}

// drainOuterNames reads the stored JSON list of former outer neighbours. A
// value that does not parse names nobody, which finishes the departure on the
// next sweep — the safe direction: the row goes, the boxes keep relaying.
func drainOuterNames(raw string) []string {
	names := []string{}
	if raw = strings.TrimSpace(raw); raw == "" {
		return names
	}
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		logger.Warningf("chain: drain_outer %q is not a list of hop names: %v", raw, err)
		return []string{}
	}
	return names
}

// liveOuterNeighbours are the rows that hang off id and still have a box to
// serve: everything that entered the chain, a hop that is re-entering (§4.5.7)
// and a hop that is itself draining. A brand-new pending hop is not among them
// — no box of its own exists yet, so there is nobody to keep serving.
func liveOuterNeighbours(tx *gorm.DB, id int) ([]model.ChainHop, error) {
	var neighbours []model.ChainHop
	err := tx.Where("next_hop_id = ?", id).
		Where("state IN ? OR (state = ? AND secret_hash <> '')",
			[]string{chain.StateJoined, chain.StateLegacy, chain.StateDraining}, chain.StatePending).
		Order("CASE role WHEN '" + chain.RoleInner + "' THEN 0 ELSE 1 END, position, id").
		Find(&neighbours).Error
	if err != nil {
		return nil, err
	}
	return neighbours, nil
}

// startDrainingTx is step 4 of §4.5.1: the row stays, its state becomes
// draining, and its host, sub port, next_hop_id, position and secret hash all
// stay exactly as they were — a draining hop keeps dialling the next hop it
// always dialled and keeps answering its neighbours at the address they know.
func startDrainingTx(tx *gorm.DB, hop *model.ChainHop, outer []model.ChainHop, minutes int) (int64, int64, []string, error) {
	names := make([]string, 0, len(outer))
	for _, neighbour := range outer {
		names = append(names, neighbour.Name)
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		return 0, 0, nil, err
	}

	hop.State = chain.StateDraining
	if err := tx.Save(hop).Error; err != nil {
		return 0, 0, nil, err
	}
	// The neighbours are re-chained onto what the departing hop dialled, in
	// the same transaction as the revision: by the time they are told to move,
	// their new upstream already holds their secret hashes.
	for index := range outer {
		neighbour := &outer[index]
		neighbour.NextHopId = copyId(hop.NextHopId)
		if err := tx.Save(neighbour).Error; err != nil {
			return 0, 0, nil, err
		}
	}
	if err := reconcileTopology(tx); err != nil {
		return 0, 0, nil, err
	}
	if err := bumpRevisionTx(tx); err != nil {
		return 0, 0, nil, err
	}
	revision, err := revisionTx(tx)
	if err != nil {
		return 0, 0, nil, err
	}
	until := time.Now().Add(time.Duration(minutes) * time.Minute).UnixMilli()
	err = tx.Model(&model.ChainHop{}).Where("id = ?", hop.Id).
		Updates(map[string]any{
			"drain_revision": revision,
			"drain_until":    until,
			"drain_outer":    string(encoded),
		}).Error
	if err != nil {
		return 0, 0, nil, err
	}
	return revision, until, names, nil
}

// SweepDraining finishes every departure that is done: all former neighbours
// have confirmed the revision the departure started in, or the deadline has
// passed (§4.5.4).
//
// It runs where the confirmations arrive (ChainWaveService.RecordSeen) and on
// a 60-second ticker beside the sub server, so a chain in which nobody polls
// any more still times out. It is one SELECT and, at worst, one DELETE — a
// worker of its own would be more machinery than the job.
func (s *ChainService) SweepDraining() error {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	var draining []model.ChainHop
	if err := db.Where("state = ?", chain.StateDraining).Order("id").Find(&draining).Error; err != nil {
		return err
	}
	if len(draining) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	for index := range draining {
		hop := draining[index]
		waiting, err := drainWaiting(db, hop)
		if err != nil {
			return err
		}
		switch {
		case len(waiting) == 0:
			logger.Infof("chain: drain complete for %q — every former neighbour is on revision %d or later",
				hop.Name, hop.DrainRevision)
		case hop.DrainUntil > 0 && now >= hop.DrainUntil:
			logger.Warningf("chain: drain timeout for %s, outer hops still behind: %s",
				hop.Name, strings.Join(waiting, ", "))
		default:
			continue
		}
		if err := s.finishDraining(hop.Id); err != nil {
			return err
		}
	}
	return nil
}

// drainWaiting names the former neighbours that have not confirmed the
// departure's revision yet. A name that is no longer in the registry is not
// waited for: that neighbour left as well, and nothing will ever confirm for
// it (§4.5.4).
func drainWaiting(tx *gorm.DB, hop model.ChainHop) ([]string, error) {
	waiting := []string{}
	for _, name := range drainOuterNames(hop.DrainOuter) {
		var neighbour model.ChainHop
		err := tx.Where("name = ?", name).First(&neighbour).Error
		if database.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if neighbour.LastRevision < hop.DrainRevision {
			waiting = append(waiting, name)
		}
	}
	return waiting, nil
}

// finishDraining is the second half of the departure: the row goes, and with
// it the hop secret — the next poll from that box gets a bare 404, it marks
// itself stale and keeps relaying, because the panel never turns a box off
// (ADR 0003).
//
// The revision moves a second time only when the departing hop's next hop was
// a hop rather than the panel: that hop loses one entry from its hops[], and
// without a new revision it would go on admitting a secret that no longer
// exists. Nothing outward of the departing hop sees any change at all — its
// record lies before their suffix of hops[] (§4.5.6).
func (s *ChainService) finishDraining(id int) error {
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		hop, err := loadHop(tx, id)
		if err != nil {
			return err
		}
		if hop.State != chain.StateDraining {
			return nil
		}
		nextHopIsHop := false
		if hop.NextHopId != nil {
			var next model.ChainHop
			err := tx.Where("id = ?", *hop.NextHopId).First(&next).Error
			if err != nil && !database.IsNotFound(err) {
				return err
			}
			nextHopIsHop = err == nil
		}
		if err := tx.Delete(&model.ChainHop{}, hop.Id).Error; err != nil {
			return err
		}
		if err := reconcileTopology(tx); err != nil {
			return err
		}
		if !nextHopIsHop {
			return nil
		}
		return bumpRevisionTx(tx)
	})
}

// drainingCards is what the editor shows beside a departing hop: who it still
// waits for and how long it has left.
func drainingCards(tx *gorm.DB, hops []model.ChainHop) ([]DrainingHop, error) {
	cards := []DrainingHop{}
	for _, hop := range hops {
		if hop.State != chain.StateDraining {
			continue
		}
		waiting, err := drainWaiting(tx, hop)
		if err != nil {
			return nil, err
		}
		cards = append(cards, DrainingHop{
			Name:          hop.Name,
			DrainRevision: hop.DrainRevision,
			DrainUntil:    hop.DrainUntil,
			Outer:         drainOuterNames(hop.DrainOuter),
			Waiting:       waiting,
		})
	}
	return cards, nil
}

// refuseIfDraining is the guard every write shares (§4.5.5). draining is
// terminal: an owner who changed their mind adds the hop again with a new
// token, which is cheaper than an invariant that lets a departure come back
// and fairer to the neighbours that have already re-chained.
func refuseIfDraining(hop *model.ChainHop) error {
	if hop.State != chain.StateDraining {
		return nil
	}
	return chainErrorf(CodeHopIsDraining,
		"%q is draining and cannot be changed; add it again once its row is gone", hop.Name)
}
