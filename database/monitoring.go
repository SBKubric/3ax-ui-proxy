package database

import (
	"github.com/coinman-dev/3ax-ui/v2/database/model"

	"gorm.io/gorm"
)

// DeleteMonitoringForInbound removes everything monitoring knows about one
// inbound — its target rows, its event feed and both aggregate tables — the
// way the inbound deletion path already drops client_traffics. It is meant to
// run inside that deletion, on the caller's transaction, so an inbound never
// disappears while its monitoring rows stay behind. Events or aggregates that
// arrive for the inbound afterwards are answered with "ignored", not stored
// (docs/spec/monitoring-panel.md §3.6).
func DeleteMonitoringForInbound(tx *gorm.DB, kind string, inboundId int) error {
	for _, table := range []any{
		&model.MonTarget{},
		&model.MonEvent{},
		&model.MonStatsCurrent{},
		&model.MonStatsRollup{},
	} {
		if err := tx.Where("inbound_kind = ? AND inbound_id = ?", kind, inboundId).Delete(table).Error; err != nil {
			return err
		}
	}
	return nil
}
