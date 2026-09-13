package sub

import (
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
)

// The monitoring contract hands mon-server the share links of the probe
// accounts (docs/spec/monitoring-panel.md §4.3 ProbeConfigs), rendered by the
// same code that renders /sub so a probe connects exactly the way a user
// does. That code lives here and this package imports web/service, so the
// service cannot call it directly; it is registered at init instead.

func init() {
	service.RegisterProbeLinkRenderer(ProbeLinks)
}

// ProbeLinks renders one share link per enabled xray inbound that carries a
// probe account. With override set the link's address is the host override
// (path proxy); otherwise it is host (path direct). Multi-link inbounds
// (external proxies) contribute their first link: there is one probe.
func ProbeLinks(host string, override bool) (map[int]string, error) {
	settingService := &service.SettingService{}
	remarkModel, err := settingService.GetRemarkModel()
	if err != nil {
		return nil, err
	}
	// showInfo off: a probe's remark carries no quota or expiry.
	s := NewSubService(false, remarkModel, "")
	s.address = host
	s.hiddifyCompat, _ = settingService.GetXrayHiddifyCompat()
	if override {
		s.overrideHost, s.overrideOn = settingService.GetProxyOverride()
	}
	s.datepicker, err = settingService.GetDatepicker()
	if err != nil {
		s.datepicker = "gregorian"
	}

	db := database.GetDB()
	var inbounds []*model.Inbound
	if err := db.Model(model.Inbound{}).
		Where("enable = ? AND protocol IN ?", true, []model.Protocol{model.VLESS, model.VMESS, model.Trojan, model.Shadowsocks}).
		Order("id asc").Find(&inbounds).Error; err != nil {
		return nil, err
	}

	links := make(map[int]string, len(inbounds))
	for _, inbound := range inbounds {
		clients, err := s.inboundService.GetClients(inbound)
		if err != nil || clients == nil {
			continue
		}
		email := service.ProbeEmail(model.MonKindXray, inbound.Id)
		found := false
		for _, client := range clients {
			if strings.EqualFold(client.Email, email) {
				email, found = client.Email, true
				break
			}
		}
		if !found {
			continue
		}
		if len(inbound.Listen) > 0 && inbound.Listen[0] == '@' {
			listen, port, streamSettings, err := s.getFallbackMaster(inbound.Listen, inbound.StreamSettings)
			if err == nil {
				inbound.Listen = listen
				inbound.Port = port
				inbound.StreamSettings = streamSettings
			}
		}
		link := s.getLink(inbound, email)
		if i := strings.IndexByte(link, '\n'); i >= 0 {
			link = link[:i]
		}
		if link != "" {
			links[inbound.Id] = link
		}
	}
	return links, nil
}
