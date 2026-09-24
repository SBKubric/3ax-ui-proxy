package service

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/util/common"
)

// Probe accounts (docs/spec/monitoring-panel.md §3). mon-server probes every
// inbound through a client of its own: one xray client per client-facing
// inbound, shared by every mon-client and path, and one AmneziaWG peer per
// mon-client × path (a WireGuard peer has one endpoint and one session, so a
// shared peer is fought over by concurrent probes; SBKubric/3ax-ui-monitoring
// #80). They all share the panel's probe subId and are recognised by name
// alone: the ProbePrefix on the email.
//
// The name is the guard. Every ordinary path that creates or edits a client
// refuses an email carrying the prefix, so a probe can only come from
// MonitoringService.EnsureProbeSet and can never be renamed into a user (a
// stripped prefix would do exactly that). Deleting one is allowed: the next
// ensure recreates it. Online sets, counters and the bot's reports skip probes
// so they are not counted as users; their traffic is left alone.

// ProbePrefix marks a probe account's email.
const ProbePrefix = "probe-"

// ProbeComment is the comment every probe client is created with.
const ProbeComment = "monitoring probe"

// probeTunnelPrefix starts the email of every AmneziaWG probe peer. Native
// WireGuard is out of v1; it would be "probe-wg-".
const probeTunnelPrefix = ProbePrefix + "awg-"

// legacyProbeTunnelEmail is the one shared AmneziaWG probe of contract v1. The
// first ensure of v2 deletes it with every other peer outside the snapshot.
const legacyProbeTunnelEmail = ProbePrefix + "awg"

// monClientIdRule is the grammar of a monClientId in the registry snapshot
// (contract v2 §3): it becomes part of a peer name, so it is kept short and
// free of the separators paths use.
var monClientIdRule = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// ProbeTunnelEmail is the email of the AmneziaWG probe peer of one mon-client
// on one path: probe-awg-<monClientId>-<path>, a ':' in the path (a hop,
// edge:<name>) written as '-'.
func ProbeTunnelEmail(monClientId, path string) string {
	return probeTunnelPrefix + monClientId + "-" + strings.ReplaceAll(path, ":", "-")
}

// IsProbeAccount reports whether an email names a probe account. The check is
// case-insensitive so "Probe-12" cannot slip past the guard.
func IsProbeAccount(email string) bool {
	return len(email) >= len(ProbePrefix) && strings.EqualFold(email[:len(ProbePrefix)], ProbePrefix)
}

// ProbeXrayEmail is the email of the probe client of one xray inbound.
func ProbeXrayEmail(inboundId int) string {
	return ProbePrefix + strconv.Itoa(inboundId)
}

// NewProbeXrayClient builds the probe client of an xray inbound with the
// attributes of §3.3: enabled, unlimited, no Telegram, the shared subId. flow
// is copied from the inbound's first regular client so the probe travels the
// same way users do; pass "" when the inbound has none.
func NewProbeXrayClient(inboundId int, subId, flow string) model.Client {
	return model.Client{
		Email:   ProbeXrayEmail(inboundId),
		Flow:    flow,
		Enable:  true,
		SubID:   subId,
		Comment: ProbeComment,
	}
}

// NewProbeTunnelClient builds the AmneziaWG probe peer of one mon-client on
// one path. The subId is not a column of tunnel clients; EnsureProbeSet binds
// it through the tunnel subscription service after creation.
func NewProbeTunnelClient(monClientId, path string) model.TunnelClient {
	email := ProbeTunnelEmail(monClientId, path)
	return model.TunnelClient{
		Name:    email,
		Email:   email,
		Enable:  true,
		Comment: ProbeComment,
	}
}

// errProbeAccount is what every guarded path returns for a probe email.
func errProbeAccount(email string) error {
	return common.NewError("email is reserved for monitoring probes:", email)
}

// rejectProbeEmails is the guard of the add and update paths: the first email
// that names a probe account fails the whole request. An empty list passes.
func rejectProbeEmails(emails ...string) error {
	for _, email := range emails {
		if IsProbeAccount(email) {
			return errProbeAccount(email)
		}
	}
	return nil
}

// withoutProbeAccounts drops probe emails from a list of emails. Entries that
// are not emails (tunnel client uuids, say) pass through untouched.
func withoutProbeAccounts(emails []string) []string {
	kept := emails[:0:0]
	for _, email := range emails {
		if !IsProbeAccount(email) {
			kept = append(kept, email)
		}
	}
	return kept
}

// rejectAddedProbeClients is the guard of AddInbound (old is nil) and
// UpdateInbound: a probe email in updated's settings.clients passes only if
// old already held exactly that email. The probe an inbound has survives an
// ordinary edit and may be dropped (the next ensure recreates it); a new one,
// or a renamed one, is refused.
func (s *InboundService) rejectAddedProbeClients(old, updated *model.Inbound) error {
	clients, _ := s.GetClients(updated)
	had := map[string]bool{}
	if old != nil {
		oldClients, _ := s.GetClients(old)
		for _, c := range oldClients {
			had[c.Email] = true
		}
	}
	for _, c := range clients {
		if IsProbeAccount(c.Email) && !had[c.Email] {
			return errProbeAccount(c.Email)
		}
	}
	return s.rejectProbeLookalikes(old, clients)
}

// rejectNewProbeClients is the guard of AddInboundClient: no probe email, and
// no probe identity under another name (rejectProbeLookalikes) against the
// probe the target inbound holds.
func (s *InboundService) rejectNewProbeClients(inboundId int, clients []model.Client) error {
	if err := rejectProbeEmails(clientEmails(clients)...); err != nil {
		return err
	}
	inbound, err := s.GetInbound(inboundId)
	if err != nil {
		return err
	}
	return s.rejectProbeLookalikes(inbound, clients)
}

// rejectProbeLookalikes closes the gap the email guard leaves (#115): a probe
// renamed into a user ("probe-1" → "carol") reads as a drop plus an add, and
// the new client would inherit the probe's identity — its credential and its
// subId, that is the probe set's links. A client that is not a probe by name
// is refused if it carries the credential or subId of a probe client of old,
// or the panel's monProbeSubId. Probe clients themselves are left to the email
// guards. old may be nil (a new inbound): then only monProbeSubId is checked.
func (s *InboundService) rejectProbeLookalikes(old *model.Inbound, clients []model.Client) error {
	secrets := map[string]bool{}
	subIds := map[string]bool{}
	monSubId, err := (&SettingService{}).GetMonProbeSubId()
	if err != nil {
		return err
	}
	if monSubId != "" {
		subIds[monSubId] = true
	}
	if old != nil {
		oldClients, _ := s.GetClients(old)
		for _, c := range oldClients {
			if !IsProbeAccount(c.Email) {
				continue
			}
			for _, secret := range clientSecrets(c) {
				secrets[secret] = true
			}
			if c.SubID != "" {
				subIds[c.SubID] = true
			}
		}
	}
	for _, c := range clients {
		if IsProbeAccount(c.Email) {
			continue
		}
		if subIds[c.SubID] {
			return errProbeIdentity(c.Email)
		}
		for _, secret := range clientSecrets(c) {
			if secrets[secret] {
				return errProbeIdentity(c.Email)
			}
		}
	}
	return nil
}

// clientSecrets lists the credentials a client may connect with: the uuid of
// vless/vmess, the password of trojan/shadowsocks, the auth of hysteria. All
// of them rather than the protocol's one, so an edit that also changes the
// inbound's protocol cannot move a probe secret into another field unseen.
func clientSecrets(c model.Client) []string {
	secrets := make([]string, 0, 3)
	for _, secret := range []string{c.ID, c.Password, c.Auth} {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	return secrets
}

// errProbeIdentity is what the guards return for a user carrying a probe's
// credential or subId.
func errProbeIdentity(email string) error {
	return common.NewError("client carries the identity of a monitoring probe:", email)
}

// clientEmails lists the emails of a batch of clients, in order.
func clientEmails(clients []model.Client) []string {
	emails := make([]string, 0, len(clients))
	for _, c := range clients {
		emails = append(emails, c.Email)
	}
	return emails
}

// monitoringKey is the (inbound_kind, inbound_id) the monitoring tables use
// for an inbound: the AmneziaWG server is ("awg", 0), everything else is an
// xray inbound under its own id (monitoring-contract.md, identifiers).
func monitoringKey(inbound *model.Inbound) (string, int) {
	if inbound.Protocol == model.AmneziaWG {
		return model.MonInboundKindAwg, 0
	}
	return model.MonInboundKindXray, inbound.Id
}
