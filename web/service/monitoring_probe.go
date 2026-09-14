package service

import (
	"strconv"
	"strings"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/util/common"
)

// Probe accounts (docs/spec/monitoring-panel.md §3). mon-server probes every
// inbound through a client of its own — one xray client per client-facing
// inbound and one AmneziaWG client — all sharing the panel's probe subId. They
// are recognised by name alone: the ProbePrefix on the email.
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

// ProbeTunnelEmail is the email of the AmneziaWG probe client. Native
// WireGuard is out of v1; it would be "probe-wg".
const ProbeTunnelEmail = ProbePrefix + "awg"

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

// NewProbeTunnelClient builds the AmneziaWG probe client. The subId is not a
// column of tunnel clients; EnsureProbeSet binds it through the tunnel
// subscription service after creation.
func NewProbeTunnelClient() model.TunnelClient {
	return model.TunnelClient{
		Name:    ProbeTunnelEmail,
		Email:   ProbeTunnelEmail,
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
