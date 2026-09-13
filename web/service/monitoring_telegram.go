package service

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// Telegram side of monitoring (docs/spec/monitoring-panel.md §6): one
// message per transition through the panel's bot and its admin list, no
// grouping; and a Monitoring block in the daily report. The panel sends
// nothing for events mon-server already announced (notified=true) and
// nothing for panel events, which only go to the feed.

func init() {
	bot := &Tgbot{}
	SetMonEventNotifier(bot.NotifyMonitoringEvents)
	SetMonStatusHooks(MonStatusHooks{Stale: bot.MonitoringStale, Back: bot.MonitoringBack})
}

// NotifyMonitoringEvents sends one message per announced event.
func (t *Tgbot) NotifyMonitoringEvents(notes []MonNotification) {
	for _, n := range notes {
		if msg := t.MonitoringEventMessage(n); msg != "" {
			t.SendMsgToTgbotAdmins(msg)
		}
	}
}

// MonitoringEventMessage formats one event, or returns "" for transitions
// the panel does not announce (UNKNOWN, PAUSED, panel events).
func (t *Tgbot) MonitoringEventMessage(n MonNotification) string {
	e := n.Event
	monClient := monClientLabel(n.MonClient)
	switch e.Kind {
	case model.MonEventTarget:
		inbound := n.InboundRemark
		if inbound == "" {
			inbound = fmt.Sprintf("%s %d", e.InboundKind, e.InboundId)
		}
		common := []string{"Inbound==" + inbound, "Path==" + e.Path, "MonClient==" + monClient, "Reason==" + e.Reason}
		switch e.ToState {
		case model.MonStateDown:
			return t.I18nBot("tgbot.messages.monitoring.down", append(common, "Since=="+t.monClock(e.Ts))...)
		case model.MonStateFlapping:
			return t.I18nBot("tgbot.messages.monitoring.flappingOn", common...)
		case model.MonStateUp:
			if n.PrevState == model.MonStateFlapping {
				return t.I18nBot("tgbot.messages.monitoring.flappingOff", append(common, "State=="+e.ToState)...)
			}
			downFor := ""
			if n.PrevState == model.MonStateDown && n.PrevSince > 0 && e.Ts > n.PrevSince {
				downFor = monDuration(e.Ts - n.PrevSince)
			}
			return t.I18nBot("tgbot.messages.monitoring.up", append(common, "DownFor=="+downFor)...)
		default:
			if n.PrevState == model.MonStateFlapping {
				return t.I18nBot("tgbot.messages.monitoring.flappingOff", append(common, "State=="+e.ToState)...)
			}
			return ""
		}
	case model.MonEventMonClient:
		switch e.ToState {
		case model.MonClientOffline:
			return t.I18nBot("tgbot.messages.monitoring.clientOffline", "MonClient=="+monClient, "Reason=="+e.Reason)
		case model.MonClientOnline:
			offlineFor := ""
			if since := lastOfflineSince(e.MonClientId, e.Ts); since > 0 {
				offlineFor = monDuration(e.Ts - since)
			}
			return t.I18nBot("tgbot.messages.monitoring.clientOnline", "MonClient=="+monClient, "OfflineFor=="+offlineFor)
		}
	}
	return ""
}

// MonitoringStale announces the panel's STALE.
func (t *Tgbot) MonitoringStale(since int64) {
	t.SendMsgToTgbotAdmins(t.I18nBot("tgbot.messages.monitoring.stale", "Since=="+t.monClock(since)))
}

// MonitoringBack announces the first contact after STALE.
func (t *Tgbot) MonitoringBack(since, now int64) {
	t.SendMsgToTgbotAdmins(t.I18nBot("tgbot.messages.monitoring.staleBack", "SilentFor=="+monDuration(now-since)))
}

// MonitoringDigest is the Monitoring block of the daily report: the last 24
// hours per inbound, the worst target, the mon-clients OFFLINE now and the
// STALE time — from the same Summary the UI shows. Empty when monitoring is
// off or nothing has ever been probed.
func (t *Tgbot) MonitoringDigest() string {
	if enabled, err := t.settingService.GetMonEnable(); err != nil || !enabled {
		return ""
	}
	m := &MonitoringService{}
	summary, err := m.Summary(time.Now(), 24*time.Hour)
	if err != nil || summary == nil {
		return ""
	}
	hasData := false
	for _, ib := range summary.Inbounds {
		if len(ib.Paths) > 0 {
			hasData = true
			break
		}
	}
	if !hasData && m.LastContact() == 0 {
		return ""
	}
	msg := t.I18nBot("tgbot.messages.monitoring.digestTitle")
	for _, ib := range summary.Inbounds {
		if len(ib.Paths) == 0 {
			continue
		}
		parts := make([]string, 0, len(ib.Paths))
		for _, p := range ib.Paths {
			part := p.Path + " "
			if p.Uptime == nil {
				part += "—"
			} else {
				part += monPercent(*p.Uptime)
			}
			if p.Coverage < 0.995 {
				part += " (cov " + monPercent(p.Coverage) + ")"
			}
			parts = append(parts, part)
		}
		msg += t.I18nBot("tgbot.messages.monitoring.digestLine", "Inbound=="+ib.Remark,
			"Paths=="+strings.Join(parts, " · "), "Incidents=="+strconv.Itoa(ib.Incidents))
	}
	if !hasData {
		msg += t.I18nBot("tgbot.messages.monitoring.digestNoData")
	}
	if w := summary.Worst; w != nil {
		msg += t.I18nBot("tgbot.messages.monitoring.digestWorst", "Inbound=="+w.Remark, "Path=="+w.Path,
			"MonClient=="+monClientLabel(MonClient{Id: w.MonClientId, Name: w.MonClientName, Region: w.Region}),
			"Uptime=="+monPercent(w.Uptime), "Coverage=="+monPercent(w.Coverage))
	}
	if len(summary.OfflineMonClients) > 0 {
		names := make([]string, 0, len(summary.OfflineMonClients))
		for _, c := range summary.OfflineMonClients {
			names = append(names, monClientLabel(c))
		}
		msg += t.I18nBot("tgbot.messages.monitoring.digestOffline", "MonClients=="+strings.Join(names, ", "))
	}
	if summary.StaleMs > 0 {
		msg += t.I18nBot("tgbot.messages.monitoring.digestStale", "StaleFor=="+monDuration(summary.StaleMs))
	}
	return msg
}

// monClientLabel is "<name> (<region>)", falling back to the id.
func monClientLabel(c MonClient) string {
	name := c.Name
	if name == "" {
		name = c.Id
	}
	if c.Region != "" {
		return name + " (" + c.Region + ")"
	}
	return name
}

// monClock formats an epoch millisecond as HH:MM in the panel's time zone.
func (t *Tgbot) monClock(ms int64) string {
	loc, err := t.settingService.GetTimeLocation()
	if err != nil || loc == nil {
		loc = time.Local
	}
	return time.UnixMilli(ms).In(loc).Format("15:04")
}

// monDuration renders a millisecond duration as 45s, 7m, 2h 5m or 3d 4h.
func monDuration(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		if h := int(d.Hours()) % 24; h > 0 {
			return fmt.Sprintf("%dd %dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
	}
}

// monPercent renders a 0..1 ratio as "99.8%".
func monPercent(ratio float64) string {
	return strconv.FormatFloat(ratio*100, 'f', 1, 64) + "%"
}

// lastOfflineSince finds when a mon-client went OFFLINE before ts, from the
// feed; zero when the feed has no such event.
func lastOfflineSince(monClientId string, ts int64) int64 {
	var since int64
	database.GetDB().Model(&model.MonEvent{}).
		Where("kind = ? AND mon_client_id = ? AND to_state = ? AND ts < ?", model.MonEventMonClient, monClientId, model.MonClientOffline, ts).
		Order("ts desc").Limit(1).Pluck("ts", &since)
	return since
}
