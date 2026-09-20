package service

import (
	"crypto/subtle"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"

	"gorm.io/gorm"
)

// ChainWaveService is the panel's end of the wave (docs/spec/proxy-chain.md
// §3.3): it says who is allowed to poll the panel, and it writes down how
// fresh every hop is from what the poll carries.
//
// It never moves the revision. Freshness is the one thing that changes
// constantly, and a chain whose revision moved on every poll would spend its
// life chasing its own tail (§3.4).
type ChainWaveService struct{}

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
	var candidates []model.ChainHop
	err := db.Where("next_hop_id IS NULL AND secret_hash <> '' AND state IN ?",
		[]string{chain.StateJoined, chain.StateLegacy}).Find(&candidates).Error
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
// Neither field is ever wound back. Acknowledgements travel inward hop by hop,
// so a stale one can arrive behind a fresher one, and a hop that appeared to
// go backwards would read as an outage that never happened. A timestamp from
// a box with a running-fast clock is clamped to now for the same reason.
func (s *ChainWaveService) RecordSeen(hopName string, seenRevision int64, outer []chain.OuterAck) error {
	db := database.GetDB()
	if db == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	return db.Transaction(func(tx *gorm.DB) error {
		if err := recordSeenTx(tx, hopName, seenRevision, now); err != nil {
			return err
		}
		for _, ack := range outer {
			name := strings.TrimSpace(ack.Name)
			if name == "" || name == hopName {
				continue
			}
			seen := ack.LastSeen
			if seen > now {
				seen = now
			}
			if err := recordSeenTx(tx, name, ack.LastRevision, seen); err != nil {
				return err
			}
		}
		return nil
	})
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
