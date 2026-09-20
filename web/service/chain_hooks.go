package service

import (
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"

	"gorm.io/gorm"
)

// chainPortsChanged moves the chain to a new revision after something changed
// the panel's port composition (docs/spec/proxy-chain.md §3.4): an inbound
// added, edited, switched off or deleted, a tunnel server saved. Without it
// the fronts would keep relaying yesterday's ports until some unrelated
// registry edit happened to bump the revision.
//
// It takes the caller's transaction when there is one, and every call site
// must pass one if it still has a transaction open: SQLite here runs on a
// single connection, so a hook that opened a query of its own from inside an
// open transaction would wait forever for the connection that transaction is
// holding. A nil tx is only correct after the caller's write has committed.
//
// A panel with no chain does nothing at all: the registry is empty, no box is
// polling, and a revision counter climbing on a panel that has no fronts is
// noise in the settings table.
//
// The new composition is worked out before the bump, because a list the panel
// cannot make sense of must not become a revision (§3.8): a port claimed by
// two sources has no one right answer, and telling every front to fetch a
// document that will fail to build would take the chain down over a
// mistake the operator can fix in the editor. The refusal is logged naming
// both sources and kept for the banner; the revision stays where it is, so
// the fronts keep relaying the last list that made sense.
//
// Any other failure — an xray config that cannot be read, say — still bumps:
// it is a fault of the panel's own state, likely momentary, and holding the
// revision back would hide every real port change behind it.
//
// A failure to bump is logged, not returned: the write that just happened is
// the operator's, it succeeded, and refusing it after the fact over a revision
// counter would be a worse outcome than a chain that catches up on the next
// change.
func chainPortsChanged(tx *gorm.DB) {
	if tx == nil {
		tx = database.GetDB()
	}
	if tx == nil {
		return
	}
	var hops int64
	if err := tx.Model(&model.ChainHop{}).Count(&hops).Error; err != nil {
		logger.Warning("chain: cannot tell whether the registry has hops:", err)
		return
	}
	if hops == 0 {
		return
	}

	if _, err := (&ChainPortsService{db: tx}).Ports(); err != nil {
		if ChainErrorCode(err) == CodeDuplicatePort {
			logger.Warning("chain: the relayed ports were left at the previous revision:", err)
			return
		}
		logger.Warning("chain: the relayed ports could not be computed:", err)
	}

	if err := (&ChainService{}).BumpRevision(tx); err != nil {
		logger.Warning("chain: the relayed ports changed but the revision did not move:", err)
	}
}
