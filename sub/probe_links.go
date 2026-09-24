package sub

import (
	"encoding/json"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// probeLinks is the renderer MonitoringService falls back to. It is built per
// call from the subscription settings (show-info, remark model, theme), the
// way the sub server builds its own SubService, so it works whether or not
// the sub server is running.
type probeLinks struct{}

func (probeLinks) ProbeLink(inbound *model.Inbound, email, address string, useOverride bool) string {
	settings := &service.SettingService{}
	showInfo, _ := settings.GetSubShowInfo()
	remarkModel, _ := settings.GetRemarkModel()
	theme, _ := settings.GetSubTheme()
	return NewSubService(showInfo, remarkModel, theme).ProbeLink(inbound, email, address, useOverride)
}

// The panel binary links this package for the sub server, which is enough
// for the monitoring controller to render probe links without importing it.
func init() {
	service.SetProbeLinkRenderer(probeLinks{})
}

// ProbeLink renders one client's link for the monitoring probe set
// (docs/spec/monitoring-panel.md §4.3) exactly as /sub would, except that the
// caller decides the connection address: with useOverride the proxy-front host
// override applies as it does for users (path "proxy"); without it the
// inbound's public Listen, if it has one, else address (path "direct"). The
// stream's externalProxy is dropped: it would replace both addresses with its
// own ep.dest and fan the link out into one line per endpoint, and a probe
// wants exactly one link that goes where its path says. Runs on a copy, like
// GetSubs, so the shared service is never written to. Satisfies
// service.ProbeLinkRenderer.
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
	ib.StreamSettings = withoutExternalProxy(ib.StreamSettings)
	return local.getLink(&ib, email)
}

// withoutExternalProxy returns streamSettings with the externalProxy key
// removed, or unchanged when it has none or does not parse.
func withoutExternalProxy(streamSettings string) string {
	var stream map[string]any
	if err := json.Unmarshal([]byte(streamSettings), &stream); err != nil {
		return streamSettings
	}
	if _, ok := stream["externalProxy"]; !ok {
		return streamSettings
	}
	delete(stream, "externalProxy")
	raw, err := json.Marshal(stream)
	if err != nil {
		return streamSettings
	}
	return string(raw)
}
