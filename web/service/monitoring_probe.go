package service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/util/random"
	"github.com/google/uuid"
)

// Probe accounts (docs/spec/monitoring-panel.md §3). mon-clients connect
// through the same inbounds as users, so each inbound carries one client that
// exists only for that: named probe-<inboundId> (probe-awg for the AmneziaWG
// server), created by MonitoringService.EnsureProbeSet and by nothing else.
// The prefix is reserved: every ordinary path that creates or edits a client
// refuses it, so a user can never be turned into a probe or a probe into a
// user. Deleting a probe is allowed — the next ensure recreates it.

// ProbePrefix marks a probe account's email.
const ProbePrefix = "probe-"

// probeComment is the comment every probe account carries.
const probeComment = "monitoring probe"

// errProbeAccountReserved is what the create/edit paths return for a
// probe-prefixed email.
var errProbeAccountReserved = errors.New("emails starting with \"" + ProbePrefix + "\" are reserved for monitoring probe accounts")

// IsProbeAccount reports whether an email names a probe account. The check is
// case-insensitive so "Probe-12" cannot slip past the guard.
func IsProbeAccount(email string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(email)), ProbePrefix)
}

// ProbeEmail is the email of the probe account of one inbound.
func ProbeEmail(kind string, inboundId int) string {
	if kind == model.MonKindAwg {
		return ProbePrefix + model.MonKindAwg
	}
	return fmt.Sprintf("%s%d", ProbePrefix, inboundId)
}

// refuseProbeAccounts is the guard at the head of the create/edit paths.
func refuseProbeAccounts(emails ...string) error {
	for _, email := range emails {
		if IsProbeAccount(email) {
			return errProbeAccountReserved
		}
	}
	return nil
}

// clientEmails lists the emails of a client batch, for the guard.
func clientEmails(clients []model.Client) []string {
	emails := make([]string, 0, len(clients))
	for i := range clients {
		emails = append(emails, clients[i].Email)
	}
	return emails
}

// withoutProbeAccounts drops probe emails from an online list or a counter's
// input. Traffic is deliberately not touched (§3.4): probes count against the
// inbound's total like any client.
func withoutProbeAccounts(emails []string) []string {
	if len(emails) == 0 {
		return emails
	}
	kept := emails[:0:0]
	for _, email := range emails {
		if !IsProbeAccount(email) {
			kept = append(kept, email)
		}
	}
	return kept
}

// probeXrayClient builds the probe account of one xray inbound with the
// attributes of §3.3: enabled, no limits, no expiry, no Telegram user, the
// probe set's subId, and the flow of the first regular client so the probe
// takes the same route the users take. The credential matches the protocol's
// client identity.
func probeXrayClient(inbound *model.Inbound, existing []model.Client, subId string) model.Client {
	client := model.Client{
		Email:   ProbeEmail(model.MonKindXray, inbound.Id),
		Enable:  true,
		SubID:   subId,
		Comment: probeComment,
	}
	for _, c := range existing {
		if !IsProbeAccount(c.Email) {
			client.Flow = c.Flow
			break
		}
	}
	switch inbound.Protocol {
	case model.Trojan:
		client.Password = random.Seq(16)
	case model.Shadowsocks:
		client.Password = shadowsocksPassword(inboundSettingsMethod(inbound.Settings))
	default:
		client.ID = uuid.NewString()
	}
	return client
}

// probeTunnelClient builds the AmneziaWG probe peer. The tunnel client model
// has no subId of its own (ADR 0002), so the probe is found by its email.
func probeTunnelClient() *model.TunnelClient {
	email := ProbeEmail(model.MonKindAwg, 0)
	return &model.TunnelClient{
		Name:    email,
		Email:   email,
		Enable:  true,
		Comment: probeComment,
	}
}

// shadowsocksPassword returns a password the cipher accepts: the 2022 ciphers
// want a base64 key of the cipher's size, the classic ones any string.
func shadowsocksPassword(method string) string {
	switch {
	case strings.HasPrefix(method, "2022-blake3-aes-128"):
		return base64Key(16)
	case strings.HasPrefix(method, "2022-"):
		return base64Key(32)
	default:
		return random.Seq(16)
	}
}

func base64Key(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// tunnelProbeGrants lets EnsureProbeSet pass a probe peer through the guard
// in TunnelService.AddClient without a second entry point: the grant is
// keyed by the exact client value about to be added and is consumed by the
// one AddClient call that receives it.
var tunnelProbeGrants sync.Map

// grantTunnelProbe marks client as the probe peer EnsureProbeSet is creating.
func grantTunnelProbe(client *model.TunnelClient) {
	tunnelProbeGrants.Store(client, struct{}{})
}

// takeTunnelProbeGrant consumes the grant for client, reporting whether there
// was one.
func takeTunnelProbeGrant(client *model.TunnelClient) bool {
	_, ok := tunnelProbeGrants.LoadAndDelete(client)
	return ok
}

// inboundSettingsMethod reads the cipher of a shadowsocks inbound's settings.
func inboundSettingsMethod(settings string) string {
	var parsed struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal([]byte(settings), &parsed)
	return parsed.Method
}
