package service

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// The Telegram side of monitoring (monitoring-panel.md §6). The bot is the
// MonEventNotifier of the ingest path and the MonStaleNotifier of the STALE
// job: MonitoringService hands it transitions, this file turns them into
// tgbot.messages.monitoring.* messages, one per event — the panel groups
// nothing, mon-server already decided what is worth telling. On top of that
// sits the daily Monitoring block of SendReport, built from the same
// Summary the Monitoring page reads.
//
// Nothing here talks to Telegram directly: everything goes through
// monitoringSend, so a test only has to set monSend.

// NotifyMonitoringEvents delivers one message per transition and returns the
// ids that are settled, which ApplyEvents then marks notified. A transition
// the operator does not need to see (UNKNOWN, PAUSED) is settled silently —
// it is returned without a message so it is never offered again.
//
// When the bot is not running nothing is returned: the ids stay
// notified=false, the events remain visible as un-notified in the feed, and
// no duplicate is possible either way, since ApplyEvents dedups by id and a
// re-send from mon-server would be dropped as a duplicate rather than
// re-notified.
func (t *Tgbot) NotifyMonitoringEvents(events []model.MonEvent) []string {
	if t.monSend == nil && !t.IsRunning() {
		return nil
	}
	ids := make([]string, 0, len(events))
	for i := range events {
		ev := events[i]
		if msg := t.monitoringEventMessage(&ev); msg != "" {
			t.monitoringSend(msg)
		}
		ids = append(ids, ev.Id)
	}
	return ids
}

// monitoringEventMessage renders one event, or "" when it is a transition the
// spec does not announce.
func (t *Tgbot) monitoringEventMessage(ev *model.MonEvent) string {
	since := "Since==" + time.UnixMilli(ev.Ts).Format("15:04")
	switch ev.Kind {
	case model.MonEventKindTarget:
		inbound := "Inbound==" + t.monInboundRemark(ev.InboundKind, ev.InboundId)
		path := "Path==" + ev.Path
		client := "Client==" + t.monClientLabel(ev.MonClientId)
		reason := "Reason==" + monReason(ev.Reason)
		// DOWN wins over everything, then entering and leaving FLAPPING —
		// leaving it is the news, whatever state follows — and only a plain
		// recovery is an UP message.
		switch {
		case ev.To == model.MonStateDown:
			return t.I18nBot("tgbot.messages.monitoring.down", inbound, path, client, reason, since)
		case ev.To == model.MonStateFlapping:
			return t.I18nBot("tgbot.messages.monitoring.flappingOn", inbound, path, client, reason, since)
		case ev.From == model.MonStateFlapping:
			return t.I18nBot("tgbot.messages.monitoring.flappingOff", inbound, path, client, "State=="+ev.To, since)
		case ev.To == model.MonStateUp:
			if d, ok := monPrecedingDuration(ev, model.MonStateDown); ok {
				return t.I18nBot("tgbot.messages.monitoring.up", inbound, path, client, since, "Duration=="+monHumanDuration(d))
			}
			return t.I18nBot("tgbot.messages.monitoring.upNoDuration", inbound, path, client, since)
		}
		return ""
	case model.MonEventKindMonClient:
		client := "Client==" + t.monClientLabel(ev.MonClientId)
		switch ev.To {
		case "OFFLINE":
			return t.I18nBot("tgbot.messages.monitoring.clientOffline", client, since)
		case "ONLINE":
			if d, ok := monPrecedingDuration(ev, "OFFLINE"); ok {
				return t.I18nBot("tgbot.messages.monitoring.clientOnline", client, "Duration=="+monHumanDuration(d))
			}
			return t.I18nBot("tgbot.messages.monitoring.clientOnlineNoDuration", client)
		}
		return ""
	}
	// kind=panel never reaches here: ApplyEvents stores it notified, because
	// mon-server has already told the operator itself.
	return ""
}

// NotifyMonitoringStale announces that mon-server has gone quiet.
func (t *Tgbot) NotifyMonitoringStale(since time.Time) {
	t.monitoringSend(t.I18nBot("tgbot.messages.monitoring.stale", "Since=="+since.Format("15:04")))
}

// NotifyMonitoringBack announces that it is talking again.
func (t *Tgbot) NotifyMonitoringBack(silentFor time.Duration) {
	minutes := int64(math.Round(silentFor.Minutes()))
	t.monitoringSend(t.I18nBot("tgbot.messages.monitoring.staleBack", "Minutes=="+strconv.FormatInt(minutes, 10)))
}

// monitoringSend is the one way out of this file. monSend is a test seam;
// in production it is nil and the message goes to every admin chat.
func (t *Tgbot) monitoringSend(msg string) {
	if msg == "" {
		return
	}
	if t.monSend != nil {
		t.monSend(msg)
		return
	}
	t.SendMsgToTgbotAdmins(msg)
}

// --- the daily digest --------------------------------------------------------

// monitoringDigest is the Monitoring block of SendReport (§6): the last 24
// hours of every enabled inbound, the worst target, the mon-clients that are
// offline, those still awaiting their first heartbeat, and how long
// monitoring itself was silent. Empty — so SendReport
// sends nothing — when monitoring is off or the summary cannot be built.
func (t *Tgbot) monitoringDigest() string {
	enabled, err := t.settingService.GetMonEnable()
	if err != nil || !enabled {
		return ""
	}
	summary, err := t.monitoringService.Summary(time.Now(), 24*time.Hour)
	if err != nil {
		logger.Warning("monitoring: could not build the Telegram digest:", err)
		return ""
	}

	msg := t.I18nBot("tgbot.messages.monitoring.digestTitle")
	for _, ib := range summary.Inbounds {
		remark := "Remark==" + monInboundLabel(ib.Kind, ib.InboundId, ib.Remark)
		if len(ib.Paths) == 0 {
			msg += t.I18nBot("tgbot.messages.monitoring.digestNoData", remark)
			continue
		}
		parts := make([]string, 0, len(ib.Paths))
		for _, p := range ib.Paths {
			parts = append(parts, p.Path+" "+monUptimePercent(p.Uptime))
		}
		msg += t.I18nBot("tgbot.messages.monitoring.digestLine", remark,
			"Paths=="+strings.Join(parts, " · "),
			"Coverage=="+monCoveragePercent(ib.Coverage),
			"Incidents=="+strconv.Itoa(ib.Incidents))
	}
	if w := summary.Worst; w != nil {
		uptime := w.Uptime
		msg += t.I18nBot("tgbot.messages.monitoring.digestWorst",
			"Remark=="+t.monInboundRemark(w.InboundKind, w.InboundId),
			"Path=="+w.Path,
			"Client=="+monClientLabel(w.MonClientName, w.Region),
			"Uptime=="+monUptimePercent(&uptime),
			"Coverage=="+monCoveragePercent(w.Coverage))
	}
	if len(summary.OfflineClients) > 0 {
		names := make([]string, 0, len(summary.OfflineClients))
		for _, c := range summary.OfflineClients {
			names = append(names, monClientLabel(monFirstNotEmpty(c.Name, c.Id), c.Region))
		}
		msg += t.I18nBot("tgbot.messages.monitoring.digestOffline", "Clients=="+strings.Join(names, ", "))
	}
	if len(summary.AwaitingClients) > 0 {
		names := make([]string, 0, len(summary.AwaitingClients))
		for _, c := range summary.AwaitingClients {
			names = append(names, monClientLabel(monFirstNotEmpty(c.Name, c.Id), c.Region))
		}
		msg += t.I18nBot("tgbot.messages.monitoring.digestAwaiting", "Clients=="+strings.Join(names, ", "))
	}
	if summary.StaleMs > 0 || summary.StaleNow {
		minutes := int64(math.Round(float64(summary.StaleMs) / 60000))
		msg += t.I18nBot("tgbot.messages.monitoring.digestStale", "Minutes=="+strconv.FormatInt(minutes, 10))
	}
	return msg
}

// --- formatting helpers ------------------------------------------------------

// monPrecedingDuration is how long the target (or mon-client) sat in the
// state it is leaving: the distance back to the last event that put it there.
// Reports false when that event is no longer in the feed, in which case the
// message is sent without a duration rather than with a made-up one.
func monPrecedingDuration(ev *model.MonEvent, toState string) (time.Duration, bool) {
	q := database.GetDB().Model(&model.MonEvent{}).
		Where("kind = ? AND mon_client_id = ? AND to_state = ? AND ts < ?", ev.Kind, ev.MonClientId, toState, ev.Ts)
	if ev.Kind == model.MonEventKindTarget {
		q = q.Where("inbound_kind = ? AND inbound_id = ? AND path = ?", ev.InboundKind, ev.InboundId, ev.Path)
	}
	var prev model.MonEvent
	if err := q.Order("ts desc").First(&prev).Error; err != nil {
		if !database.IsNotFound(err) {
			logger.Warning("monitoring: could not look up the preceding event:", err)
		}
		return 0, false
	}
	return time.Duration(ev.Ts-prev.Ts) * time.Millisecond, true
}

// monInboundRemark names an inbound the way the operator knows it, falling
// back to its monitoring key when it is gone from the panel.
func (t *Tgbot) monInboundRemark(kind string, id int) string {
	inbounds, err := t.monitoringService.Inbounds()
	if err != nil {
		logger.Warning("monitoring: could not list inbounds for a Telegram message:", err)
		return monInboundLabel(kind, id, "")
	}
	for _, ib := range inbounds {
		if ib.Kind == kind && ib.InboundId == id {
			return monInboundLabel(kind, id, ib.Remark)
		}
	}
	return monInboundLabel(kind, id, "")
}

// monInboundLabel is the remark, or "<kind>#<id>" when there is none.
func monInboundLabel(kind string, id int, remark string) string {
	if remark != "" {
		return remark
	}
	return fmt.Sprintf("%s#%d", kind, id)
}

// monClientLabel renders one mon-client of the registry snapshot; an id the
// snapshot no longer knows is printed as it came.
func (t *Tgbot) monClientLabel(id string) string {
	for _, c := range t.monitoringService.RegistrySnapshot() {
		if c.Id == id {
			return monClientLabel(monFirstNotEmpty(c.Name, c.Id), c.Region)
		}
	}
	return id
}

func monClientLabel(name, region string) string {
	if region == "" {
		return name
	}
	return name + " (" + region + ")"
}

func monFirstNotEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// monReason keeps the "reason:" part of a message from ending in nothing.
func monReason(reason string) string {
	if reason == "" {
		return "unknown"
	}
	return reason
}

// monUptimePercent is one decimal, or a dash when nothing was probed.
func monUptimePercent(uptime *float64) string {
	if uptime == nil {
		return "—"
	}
	return strconv.FormatFloat(*uptime*100, 'f', 1, 64) + " %"
}

// monCoveragePercent is a whole percent.
func monCoveragePercent(coverage float64) string {
	return strconv.Itoa(int(math.Round(coverage * 100)))
}

// monHumanDuration is the short form the alerts use: 45s, 7m, 1h 12m, 2d 3h.
func monHumanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		if h == 0 {
			return strconv.Itoa(days) + "d"
		}
		return strconv.Itoa(days) + "d " + strconv.Itoa(h) + "h"
	}
}
