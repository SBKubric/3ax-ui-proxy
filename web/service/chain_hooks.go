package service

import (
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"

	"gorm.io/gorm"
)

// chainPortsChanged moves the chain to a new revision after something changed
// the panel's port composition (docs/spec/proxy-chain.md §3.4): an inbound
// added, edited or deleted, a tunnel server saved. Without it the fronts would
// keep relaying yesterday's ports until some unrelated registry edit happened
// to bump the revision.
//
// It takes the caller's transaction when there is one. SQLite here runs on a
// single connection, so a hook that opened a query of its own from inside an
// open transaction would wait for the connection that transaction is holding.
//
// A panel with no chain does nothing at all: the registry is empty, no box is
// polling, and a revision counter climbing on a panel that has no fronts is
// noise in the settings table.
//
// A failure is logged, not returned: the write that just happened is the
// operator's, it succeeded, and refusing it after the fact over a revision
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
	if err := (&ChainService{}).BumpRevision(tx); err != nil {
		logger.Warning("chain: the relayed ports changed but the revision did not move:", err)
	}
}
