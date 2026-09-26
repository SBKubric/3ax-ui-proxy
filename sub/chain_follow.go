package sub

import (
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// chainUpdateInterval is the Profile-Update-Interval a subscription is served
// with. While any inbound follows the chain, a switch of the active edge
// changes the SNI in its links and the old links stop working (ADR 0005), so
// clients are asked to come back within the hour: min(subUpdates, 1). The
// header carries whole hours, so any setting a client can read comes out as
// 1, the least it can say — and a setting of 0, which some clients read as
// "never", becomes 1 as well, because never refreshing is exactly what breaks.
//
// Without a chain-following inbound the setting goes out untouched, as it
// always has. Behind a chain the fronts pass this header on as they receive it
// (proxy.passthroughHeaders).
func chainUpdateInterval(configured string) string {
	followers, err := (&service.ChainService{}).FollowingInbounds()
	if err != nil {
		logger.Warning("sub: cannot tell whether any inbound follows the chain:", err)
		return configured
	}
	if followers == 0 {
		return configured
	}
	return "1"
}
