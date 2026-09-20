package service

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
)

// chainHealth reports one hop's health for the /proxy listing (§6.3, §7.4 of
// docs/spec/proxy-chain.md). Ticket #87 plugs monitoring in by replacing
// chainHealthLookup; until then every hop reports UNKNOWN.
type chainHealth func(name string) string

// defaultChainHealth is the stub every hop reports before #87.
func defaultChainHealth(name string) string { return "UNKNOWN" }

// chainHealthLookup is the seam #87 replaces and tests override; it is a
// package variable rather than a Tgbot field so this ticket does not have to
// touch the Tgbot struct in tgbot.go (ADR 0002 additive-only).
var chainHealthLookup chainHealth = defaultChainHealth

// chainProxyCommand is the body of /proxy once the chain registry exists
// (§2.5, §7.6): "/proxy" lists every hop, "/proxy <name>" switches the active
// edge, "/proxy off" clears it. It never touches Telegram itself, so it is
// plain to unit test.
func (t *Tgbot) chainProxyCommand(args []string) string {
	switch {
	case len(args) == 0:
		return t.chainListing("")
	case strings.EqualFold(args[0], "off"):
		return t.chainOff()
	default:
		return t.chainSwitch(strings.TrimSpace(args[0]))
	}
}

// chainOff clears the active edge — the chain-registry meaning of "/proxy
// off" (§2.5), still delegated to DisableProxyOverride so the legacy keys
// come along with it.
func (t *Tgbot) chainOff() string {
	if err := t.settingService.DisableProxyOverride(); err != nil {
		return t.I18nBot("tgbot.commands.proxyError", "Error=="+err.Error())
	}
	return t.I18nBot("tgbot.commands.chainOff")
}

// chainSwitch makes the named edge active. An unknown name, or a name that is
// not a joined/legacy edge (an inner front or a pending hop), falls back to
// the same listing an unadorned /proxy gives, with a warning on top (§7.6).
func (t *Tgbot) chainSwitch(name string) string {
	chainSvc := &ChainService{}
	state, err := chainSvc.List()
	if err != nil {
		return t.I18nBot("tgbot.commands.proxyError", "Error=="+err.Error())
	}
	hop := findChainHop(state, name)
	if hop == nil {
		return t.chainUnknownListing(name, state)
	}
	if err := chainSvc.SetActive(hop.Id); err != nil {
		switch ChainErrorCode(err) {
		case CodeNotAnEdge, CodeHopNotJoined:
			return t.chainUnknownListing(name, state)
		default:
			return t.I18nBot("tgbot.commands.proxyError", "Error=="+err.Error())
		}
	}
	msg := t.I18nBot("tgbot.commands.chainSwitched", "Name=="+html.EscapeString(name))
	msg += "\r\n" + t.I18nBot("tgbot.commands.chainSwitchNote")
	return msg
}

// chainListing renders a plain "/proxy" — the chain, with the usage hint
// underneath.
func (t *Tgbot) chainListing(warning string) string {
	chainSvc := &ChainService{}
	state, err := chainSvc.List()
	if err != nil {
		return t.I18nBot("tgbot.commands.proxyError", "Error=="+err.Error())
	}
	return t.renderChainListing(warning, state)
}

// chainUnknownListing is the same rendering with the chainUnknown warning on
// top, for a name that did not resolve to a switchable edge.
func (t *Tgbot) chainUnknownListing(name string, state *ChainState) string {
	warning := t.I18nBot("tgbot.commands.chainUnknown", "Name=="+html.EscapeString(name))
	return t.renderChainListing(warning, state)
}

// renderChainListing is the shared body of every listing: an optional warning,
// the header with the revision, one line per hop (or chainEmpty when the
// registry has none — the legacy host override migrates into a "legacy" hop
// at startup, so an empty registry means the override is off), and the usage
// hint.
func (t *Tgbot) renderChainListing(warning string, state *ChainState) string {
	var b strings.Builder
	if warning != "" {
		b.WriteString(warning)
		b.WriteString("\r\n\r\n")
	}
	b.WriteString(t.I18nBot("tgbot.commands.chainHeader", "Rev=="+strconv.FormatInt(state.Revision, 10)))
	b.WriteString("\r\n\r\n")
	if len(state.Hops) == 0 {
		b.WriteString(t.I18nBot("tgbot.commands.chainEmpty"))
	} else {
		lines := make([]string, len(state.Hops))
		for i, hop := range state.Hops {
			lines[i] = t.chainHopLine(hop, state.Revision)
		}
		b.WriteString(strings.Join(lines, "\r\n"))
	}
	b.WriteString("\r\n\r\n")
	b.WriteString(t.I18nBot("tgbot.commands.chainUsage"))
	return b.String()
}

// chainHopLine is one row of the listing: marker, name, role and state as the
// registry has them (technical tokens, not localised — like "path" on the
// monitoring page), freshness against the current revision (§7.4), and health
// from the chainHealthLookup seam.
func (t *Tgbot) chainHopLine(hop model.ChainHop, revision int64) string {
	marker := "·"
	if hop.IsActive {
		marker = "★"
	}
	freshness := t.I18nBot("tgbot.commands.chainFresh")
	if hop.LastRevision != revision {
		freshness = t.I18nBot("tgbot.commands.chainRev", "Rev=="+strconv.FormatInt(hop.LastRevision, 10))
	}
	health := chainHealthLookup(hop.Name)
	return fmt.Sprintf("%s <code>%s</code> · %s · %s · %s · %s",
		marker, html.EscapeString(hop.Name), hop.Role, hop.State, freshness, health)
}

// findChainHop looks a hop up by name in an already-fetched ChainState, so
// chainSwitch does not need a second registry read.
func findChainHop(state *ChainState, name string) *model.ChainHop {
	for i := range state.Hops {
		if state.Hops[i].Name == name {
			return &state.Hops[i]
		}
	}
	return nil
}
