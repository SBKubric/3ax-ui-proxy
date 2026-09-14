package sub

import (
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// ProbeLink renders one client's link for the monitoring probe set
// (docs/spec/monitoring-panel.md §4.3) exactly as /sub would, except that the
// caller decides the connection address: with useOverride the proxy-front host
// override applies as it does for users (path "proxy"); without it address is
// used instead (path "direct"). Runs on a copy, like GetSubs, so the shared
// service is never written to. Satisfies service.ProbeLinkRenderer.
func (s *SubService) ProbeLink(inbound *model.Inbound, email, address string, useOverride bool) string {
	local := *s
	local.address = address
	local.hiddifyCompat, _ = local.settingService.GetXrayHiddifyCompat()
	if useOverride {
		local.overrideHost, local.overrideOn = local.settingService.GetProxyOverride()
	} else {
		local.overrideHost, local.overrideOn = "", false
	}
	if local.datepicker == "" {
		local.datepicker = "gregorian"
	}
	if local.remarkModel == "" {
		local.remarkModel = "-ieo"
	}
	ib := *inbound
	if len(ib.Listen) > 0 && ib.Listen[0] == '@' {
		if listen, port, streamSettings, err := local.getFallbackMaster(ib.Listen, ib.StreamSettings); err == nil {
			ib.Listen, ib.Port, ib.StreamSettings = listen, port, streamSettings
		}
	}
	return local.getLink(&ib, email)
}
