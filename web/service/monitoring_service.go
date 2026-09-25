package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/shared/ipam"
	"github.com/coinman-dev/3ax-ui/v2/tunnel"
	"github.com/coinman-dev/3ax-ui/v2/util/random"
	"github.com/google/uuid"
)

// MonitoringService is the panel side of the mon-server contract
// (docs/spec/monitoring-contract.md, docs/spec/monitoring-panel.md §4.3). The
// panel is a passive receiver (ADR 0003): it describes its inbounds, keeps one
// probe set for mon-server to dial through, renders that set's configs, and
// stores what mon-server reports. It never decides UP or DOWN itself.
//
// Links renders the xray probe links. It is an interface because the renderer
// lives in package sub, which imports this package (and package web through
// the sub server), so nothing here can import it back; sub registers its
// renderer with SetProbeLinkRenderer at init and a nil Links falls back to
// that. Tests set Links to a fake.
type MonitoringService struct {
	settingService SettingService
	inboundService InboundService
	xrayService    XrayService
	awgService     AwgService

	Links ProbeLinkRenderer
}

// ProbeLinkRenderer renders the subscription link of one client the way /sub
// does. address is the connection address to use when useOverride is false;
// with useOverride the proxy-front host override applies as it does for users.
type ProbeLinkRenderer interface {
	ProbeLink(inbound *model.Inbound, email, address string, useOverride bool) string
}

// MonContractVersion is the X-Mon-Contract the panel speaks. Version 2 gave
// every mon-client its own AmneziaWG probe peers (SBKubric/3ax-ui-monitoring
// #80); version 3 probes through every hop of the chain
// (SBKubric/sane-3x-ui-monitoring#61). mon-server requires exactly this
// version: the two are upgraded together.
const MonContractVersion = 3

// monProbePaths are the paths the panel serves and the AmneziaWG probe peers
// are made for, one per mon-client each. Per-hop probing extends the list.
var monProbePaths = []string{model.MonPathDirect, model.MonPathProxy}

// monProbeSubIdLength matches an ordinary subscription id.
const monProbeSubIdLength = 16

// MonError is an error the contract turns into an HTTP status and a
// {error, message} body (monitoring-contract.md §3).
type MonError struct {
	Status  int
	Code    string
	Message string
}

func (e *MonError) Error() string { return e.Code + ": " + e.Message }

var (
	// ErrOverrideDisabled: GET /probe/configs without host needs the host
	// override, and it is off.
	ErrOverrideDisabled = &MonError{409, "override_disabled", "the proxy-front host override is disabled; pass host= for the direct path"}
	// ErrProbeNotEnsured: the probe set does not exist yet.
	ErrProbeNotEnsured = &MonError{409, "probe_not_ensured", "the probe set has not been created; call POST /probe/ensure first"}
	// ErrLinksNotWired is a wiring mistake, not a runtime condition.
	ErrLinksNotWired = &MonError{500, "internal", "no probe link renderer is wired into MonitoringService"}
	// ErrUnknownHop: GET /probe/configs?hop= names no hop in the registry
	// (proxy-chain.md §6.1). Until per-hop probing lands every name is unknown.
	ErrUnknownHop = &MonError{409, "unknown_hop", "no such hop in the chain registry"}
	// ErrUnknownEdge is ErrUnknownHop under its first-edition code, for a
	// request that named the hop through the ?edge= synonym.
	ErrUnknownEdge = &MonError{409, "unknown_edge", "no such hop in the chain registry"}
)

func errXrayUnavailable(err error) *MonError {
	return &MonError{503, "xray_unavailable", "could not create a probe client: " + err.Error()}
}

// MonInbound is one sanitised inbound of GET /state: no settings, no keys.
type MonInbound struct {
	Kind      string `json:"kind"`
	InboundId int    `json:"inboundId"`
	Tag       string `json:"tag"`
	Remark    string `json:"remark"`
	Protocol  string `json:"protocol"`
	Port      int    `json:"port"`
	Enable    bool   `json:"enable"`
}

// MonInboundRef names an inbound by its monitoring key.
type MonInboundRef struct {
	Kind      string `json:"kind"`
	InboundId int    `json:"inboundId"`
}

// MonProbeRef names a probe account ensure created: an xray probe by its
// inbound, an AmneziaWG probe peer also by the mon-client and path it is for.
type MonProbeRef struct {
	Kind        string `json:"kind"`
	InboundId   int    `json:"inboundId"`
	MonClientId string `json:"monClientId,omitempty"`
	Path        string `json:"path,omitempty"`
}

// MonOverride is the proxy-front host override as GET /state reports it.
type MonOverride struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
}

// MonProbe describes the probe set: SubId is nil until the first ensure.
type MonProbe struct {
	SubId       *string `json:"subId"`
	LastEnsured int64   `json:"lastEnsured"`
}

// MonState is the body of GET /state (monitoring-contract.md §4.1).
type MonState struct {
	Contract     int          `json:"contract"`
	PanelVersion string       `json:"panelVersion"`
	ServerTime   int64        `json:"serverTime"`
	Revision     string       `json:"revision"`
	Override     MonOverride  `json:"override"`
	Chain        *MonChain    `json:"chain,omitempty"`
	Probe        MonProbe     `json:"probe"`
	Inbounds     []MonInbound `json:"inbounds"`
	Stale        struct {
		ThresholdMinutes int `json:"thresholdMinutes"`
	} `json:"stale"`
}

// MonClient is one entry of the mon-client registry snapshot mon-server sends
// with POST /probe/ensure. The panel caches it for the UI and never
// recomputes State.
type MonClient struct {
	Id            string `json:"id"`
	Name          string `json:"name"`
	Region        string `json:"region"`
	State         string `json:"state"`
	LastHeartbeat int64  `json:"lastHeartbeat"`
}

// MonEnsureResult is the body of a successful POST /probe/ensure.
// Unallocated lists the mon-clients left without an AmneziaWG probe peer on
// some path because the tunnel's address pool is full; their items are
// missing from /probe/configs.
type MonEnsureResult struct {
	SubId       string        `json:"subId"`
	Revision    string        `json:"revision"`
	LastEnsured int64         `json:"lastEnsured"`
	Created     []MonProbeRef `json:"created"`
	Present     int           `json:"present"`
	Unallocated []string      `json:"unallocated"`
}

// MonProbeItem is one config of GET /probe/configs: a link for an xray
// inbound, shared by every mon-client; a .conf for the AmneziaWG server, one
// per mon-client, which MonClientId names.
type MonProbeItem struct {
	Kind        string `json:"kind"`
	InboundId   int    `json:"inboundId"`
	MonClientId string `json:"monClientId,omitempty"`
	Link        string `json:"link,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Conf        string `json:"conf,omitempty"`
}

// MonProbeConfigs is the body of GET /probe/configs.
type MonProbeConfigs struct {
	Revision string         `json:"revision"`
	Path     string         `json:"path"`
	Items    []MonProbeItem `json:"items"`
}

// monXrayProtocols are the client-facing xray protocols the contract covers
// in v1; each of these inbounds gets a probe client.
var monXrayProtocols = map[model.Protocol]bool{
	model.VLESS: true, model.VMESS: true, model.Trojan: true, model.Shadowsocks: true,
}

// --- registry snapshot cache -------------------------------------------------

// monRegistry is the in-memory copy of the mon-client snapshot. Services are
// instantiated ad hoc all over the panel, so the cache is package state; it is
// loaded from monClientsSnapshot on first use to survive a restart.
var monRegistry struct {
	sync.RWMutex
	loaded  bool
	clients []MonClient
}

// RegistrySnapshot returns the last mon-client snapshot mon-server sent.
func (s *MonitoringService) RegistrySnapshot() []MonClient {
	monRegistry.RLock()
	if monRegistry.loaded {
		out := append([]MonClient(nil), monRegistry.clients...)
		monRegistry.RUnlock()
		return out
	}
	monRegistry.RUnlock()

	monRegistry.Lock()
	defer monRegistry.Unlock()
	if !monRegistry.loaded {
		raw, err := s.settingService.GetMonClientsSnapshot()
		var clients []MonClient
		if err == nil && strings.TrimSpace(raw) != "" {
			if err := json.Unmarshal([]byte(raw), &clients); err != nil {
				logger.Warning("monitoring: stored mon-client snapshot is not valid JSON:", err)
			}
		}
		monRegistry.clients = clients
		monRegistry.loaded = true
	}
	return append([]MonClient(nil), monRegistry.clients...)
}

func (s *MonitoringService) setRegistrySnapshot(clients []MonClient) error {
	if clients == nil {
		clients = []MonClient{}
	}
	raw, err := json.Marshal(clients)
	if err != nil {
		return err
	}
	if err := s.settingService.SetMonClientsSnapshot(string(raw)); err != nil {
		return err
	}
	monRegistry.Lock()
	monRegistry.clients = append([]MonClient(nil), clients...)
	monRegistry.loaded = true
	monRegistry.Unlock()
	return nil
}

// --- state -------------------------------------------------------------------

// Inbounds lists what mon-server may probe: every client-facing xray inbound
// (disabled ones included, so mon-server can show them PAUSED) and the
// AmneziaWG server as ("awg", 0) when it exists. Sorted by (kind, inboundId).
func (s *MonitoringService) Inbounds() ([]MonInbound, error) {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	out := make([]MonInbound, 0, len(inbounds))
	for _, ib := range inbounds {
		switch {
		case monXrayProtocols[ib.Protocol]:
			out = append(out, MonInbound{Kind: model.MonInboundKindXray, InboundId: ib.Id, Tag: ib.Tag, Remark: ib.Remark,
				Protocol: string(ib.Protocol), Port: ib.LinkPort(), Enable: ib.Enable})
		case ib.Protocol == model.AmneziaWG:
			out = append(out, MonInbound{Kind: model.MonInboundKindAwg, InboundId: 0, Tag: ib.Tag, Remark: ib.Remark,
				Protocol: model.MonInboundKindAwg, Port: ib.LinkPort(), Enable: ib.Enable})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].InboundId < out[j].InboundId
	})
	return out, nil
}

// override reports the proxy-front host override as the contract shows it:
// host is empty when the override is off.
func (s *MonitoringService) override() MonOverride {
	host, on := s.settingService.GetProxyOverride()
	if !on {
		return MonOverride{}
	}
	return MonOverride{Enabled: true, Host: host}
}

// revisionEndpointHost stands in for the endpoint host when the revision
// renders a tunnel probe .conf. The host is not probe material the panel owns:
// path "proxy" takes the override host (hashed on its own) and path "direct"
// the host mon-server passes, so only the port and the rest of the .conf count.
const revisionEndpointHost = "probe.invalid"

// Revision is the first 16 hex characters of SHA-256 over the canonical JSON
// of everything that goes into the probe material (monitoring-contract.md
// §4.2): the override, the chain's active edge and probed hops (absent with an
// empty registry), the probe subId, the link setting that shapes xray links,
// and per inbound, sorted by (kind, inboundId), its target fields plus
// its probe material — for an xray inbound listen, streamSettings and settings
// with clients cut down to its probe client; for the AmneziaWG server the
// probe peers, each as its rendered .conf. Tag and remark stay out so a
// rename does not rebuild targets. Nothing in it depends on time or on map
// order, so it is the same across restarts; there is no counter.
func (s *MonitoringService) Revision() (string, error) {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return "", err
	}
	entries := make([]map[string]any, 0, len(inbounds))
	for _, ib := range inbounds {
		switch {
		case monXrayProtocols[ib.Protocol]:
			entries = append(entries, map[string]any{
				"kind": model.MonInboundKindXray, "inboundId": ib.Id, "protocol": string(ib.Protocol),
				"port": ib.LinkPort(), "enable": ib.Enable,
				"listen":   ib.Listen,
				"stream":   probeStream(ib.StreamSettings),
				"settings": probeSettings(ib),
			})
		case ib.Protocol == model.AmneziaWG:
			peers, err := s.tunnelProbeMaterial()
			if err != nil {
				return "", err
			}
			entries = append(entries, map[string]any{
				"kind": model.MonInboundKindAwg, "inboundId": 0, "protocol": model.MonInboundKindAwg,
				"port": ib.LinkPort(), "enable": ib.Enable,
				"peers": peers,
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ki, kj := entries[i]["kind"].(string), entries[j]["kind"].(string)
		if ki != kj {
			return ki < kj
		}
		return entries[i]["inboundId"].(int) < entries[j]["inboundId"].(int)
	})
	subId, _ := s.settingService.GetMonProbeSubId()
	hiddify, _ := s.settingService.GetXrayHiddifyCompat()
	override := s.override()
	material := map[string]any{
		"hiddifyCompat": hiddify,
		"inbounds":      entries,
		"override":      map[string]any{"enabled": override.Enabled, "host": override.Host},
		"probeSubId":    subId,
	}
	chain, err := monChainTx(nil)
	if err != nil {
		return "", err
	}
	if chain.probed() {
		material["chain"] = chain.revisionMaterial()
	}
	// encoding/json writes map keys sorted and no whitespace: the canonical
	// form. Parsed settings and streams are maps too, so their key order in
	// the database does not matter either.
	canonical, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:16], nil
}

// canonicalJSON parses a stored JSON column for the revision. Numbers keep
// their literal text; a column that does not parse is hashed as the string it
// is.
func canonicalJSON(raw string) any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	return v
}

// probeStream is an xray inbound's streamSettings as the revision sees them:
// all of it but externalProxy, which probe links drop (§4.4).
func probeStream(raw string) any {
	stream := canonicalJSON(raw)
	if m, ok := stream.(map[string]any); ok {
		delete(m, "externalProxy")
	}
	return stream
}

// probeSettings is an xray inbound's settings as the revision sees them: every
// protocol-level key (a shadowsocks method, vless decryption, fallbacks...)
// but, of the clients, only the inbound's probe, so adding or editing a user
// does not move the revision.
func probeSettings(ib *model.Inbound) any {
	settings, ok := canonicalJSON(ib.Settings).(map[string]any)
	if !ok {
		return ib.Settings
	}
	clients, _ := settings["clients"].([]any)
	probes := []any{}
	for _, c := range clients {
		client, _ := c.(map[string]any)
		if email, _ := client["email"].(string); strings.EqualFold(email, ProbeXrayEmail(ib.Id)) {
			probes = append(probes, client)
		}
	}
	settings["clients"] = probes
	return settings
}

// tunnelProbeMaterial lists the AmneziaWG probe peers for the revision, by
// name, each with its .conf rendered as GET /probe/configs renders it but with
// revisionEndpointHost for the host. The .conf carries the server's public
// parameters (key, port, MTU, DNS, obfuscation) and the peer's own keys and
// addresses, so a change to any of them moves the revision.
func (s *MonitoringService) tunnelProbeMaterial() ([]map[string]any, error) {
	clients, err := s.awgService.GetClients()
	if err != nil {
		return nil, err
	}
	peers := []map[string]any{}
	for i := range clients {
		if !IsProbeAccount(clients[i].Email) {
			continue
		}
		conf, err := s.tunnelProbeConf(&clients[i], revisionEndpointHost, false)
		if err != nil {
			return nil, err
		}
		peers = append(peers, map[string]any{"name": clients[i].Email, "conf": conf})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i]["name"].(string) < peers[j]["name"].(string) })
	return peers, nil
}

// State is GET /state: the sanitised inbounds, the override, the chain, the
// probe set and the revision. No side effects.
func (s *MonitoringService) State() (*MonState, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	rev, err := s.Revision()
	if err != nil {
		return nil, err
	}
	subId, _ := s.settingService.GetMonProbeSubId()
	lastEnsured, _ := s.settingService.GetMonProbeLastEnsured()
	stale, err := s.settingService.GetMonStaleMinutes()
	if err != nil {
		stale = 15
	}
	chain, err := monChainTx(nil)
	if err != nil {
		return nil, err
	}
	st := &MonState{
		Contract:     MonContractVersion,
		Chain:        chain,
		PanelVersion: config.GetVersion(),
		ServerTime:   time.Now().UnixMilli(),
		Revision:     rev,
		Override:     s.override(),
		Probe:        MonProbe{LastEnsured: lastEnsured},
		Inbounds:     inbounds,
	}
	if subId != "" {
		st.Probe.SubId = &subId
	}
	st.Stale.ThresholdMinutes = stale
	return st, nil
}

// --- probe set ---------------------------------------------------------------

// findClient returns the client with the email, if any.
func findClient(clients []model.Client, email string) *model.Client {
	for i := range clients {
		if strings.EqualFold(clients[i].Email, email) {
			return &clients[i]
		}
	}
	return nil
}

// probeSecret is the per-protocol identity of a new probe client: a uuid for
// vless/vmess, a password for trojan and shadowsocks (a base64 key of the
// method's size for shadowsocks-2022 ciphers, as the panel's client form makes
// them).
func probeSecret(protocol model.Protocol, method string) (id, password string) {
	switch protocol {
	case model.VLESS, model.VMESS:
		return uuid.New().String(), ""
	case model.Shadowsocks:
		size := 0
		switch {
		case strings.HasPrefix(method, "2022-blake3-aes-128"):
			size = 16
		case strings.HasPrefix(method, "2022-"):
			size = 32
		}
		if size > 0 {
			key := make([]byte, size)
			if _, err := rand.Read(key); err == nil {
				return "", base64.StdEncoding.EncodeToString(key)
			}
		}
		return "", random.Seq(10)
	default:
		return "", random.Seq(10)
	}
}

// ensureXrayProbe creates the probe client of one xray inbound unless it is
// there already. It reports whether a client was created.
func (s *MonitoringService) ensureXrayProbe(ib *model.Inbound, subId string) (bool, error) {
	clients, err := s.inboundService.GetClients(ib)
	if err != nil {
		return false, err
	}
	email := ProbeXrayEmail(ib.Id)
	if findClient(clients, email) != nil {
		return false, nil
	}
	flow := ""
	for _, c := range clients {
		if !IsProbeAccount(c.Email) {
			flow = c.Flow
			break
		}
	}
	probe := NewProbeXrayClient(ib.Id, subId, flow)
	var settings map[string]any
	_ = json.Unmarshal([]byte(ib.Settings), &settings)
	method, _ := settings["method"].(string)
	probe.ID, probe.Password = probeSecret(ib.Protocol, method)
	if ib.Protocol == model.VMESS {
		probe.Security = "auto"
	}
	payload, err := json.Marshal(map[string]any{"clients": []model.Client{probe}})
	if err != nil {
		return false, err
	}
	needRestart, err := s.inboundService.addInboundClient(&model.Inbound{Id: ib.Id, Settings: string(payload)}, true)
	if err != nil {
		return false, err
	}
	if needRestart && ib.Enable {
		// The client is stored but not live in xray: the panel's own restart
		// cycle picks it up, as it does for every other client added this way.
		s.xrayService.SetToNeedRestart()
	}
	return true, nil
}

// ensureTunnelProbes reconciles the AmneziaWG probe peers to the registry
// snapshot: one peer per mon-client × path (monProbePaths), whatever the
// mon-client's state; every other probe peer goes — those of mon-clients that
// left the registry and the shared probe-awg of contract v1. Stale peers are
// deleted before new ones are made so their addresses are free again. A peer
// the address pool has no room for is skipped with a warning and its
// mon-client reported in unallocated; any other failure stops the ensure.
//
// The shared subId is not bound to the peers: tunnel clients carry no subId
// until the tunnel subscription feature (docs/spec/tunnel-subscription.md)
// lands; its TunnelSubscriptionService.Set is the hook to call here when it
// does.
func (s *MonitoringService) ensureTunnelProbes(snapshot []MonClient) (created []MonProbeRef, present int, unallocated []string, err error) {
	clients, err := s.awgService.GetClients()
	if err != nil {
		return nil, 0, nil, err
	}
	want := map[string]bool{}
	for _, mc := range snapshot {
		for _, path := range monProbePaths {
			want[ProbeTunnelEmail(mc.Id, path)] = true
		}
	}
	have := map[string]bool{}
	for _, c := range clients {
		if !IsProbeAccount(c.Email) {
			continue
		}
		if !want[c.Email] || have[c.Email] {
			if err := s.awgService.DeleteClient(c.Id); err != nil {
				return nil, 0, nil, err
			}
			continue
		}
		have[c.Email] = true
	}

	created = []MonProbeRef{}
	unallocated = []string{}
	for _, mc := range sortedMonClients(snapshot) {
		missed := false
		for _, path := range monProbePaths {
			email := ProbeTunnelEmail(mc.Id, path)
			if have[email] {
				present++
				continue
			}
			peer := NewProbeTunnelClient(mc.Id, path)
			if err := s.awgService.addClient(&peer, true); err != nil {
				if errors.Is(err, ipam.ErrPoolExhausted) {
					logger.Warningf("monitoring: no AmneziaWG probe peer for mon-client %s on %s: %v", mc.Id, path, err)
					missed = true
					continue
				}
				return nil, 0, nil, err
			}
			have[email] = true
			present++
			created = append(created, MonProbeRef{Kind: model.MonInboundKindAwg, InboundId: 0, MonClientId: mc.Id, Path: path})
		}
		if missed {
			unallocated = append(unallocated, mc.Id)
		}
	}
	return created, present, unallocated, nil
}

// sortedMonClients is the snapshot sorted by id with duplicates dropped, so
// peers are made, and addresses taken, in the same order on every ensure.
func sortedMonClients(snapshot []MonClient) []MonClient {
	seen := map[string]bool{}
	out := make([]MonClient, 0, len(snapshot))
	for _, mc := range snapshot {
		if !seen[mc.Id] {
			seen[mc.Id] = true
			out = append(out, mc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// validateSnapshot refuses a registry snapshot with a monClientId outside
// the grammar of contract v2 §3; it names a peer, so it is checked before
// anything is touched.
func validateSnapshot(snapshot []MonClient) error {
	for i, mc := range snapshot {
		if !monClientIdRule.MatchString(mc.Id) {
			return &MonError{400, "invalid_body", fmt.Sprintf("monClients[%d].id: %q is not 1 to 32 characters of [A-Za-z0-9_-]", i, mc.Id)}
		}
	}
	return nil
}

// EnsureProbeSet is POST /probe/ensure. Idempotent: it mints the probe subId
// on first call, creates whichever xray probe clients are missing (a deleted
// probe comes back with a fresh identity and the same subId), reconciles the
// AmneziaWG probe peers to the snapshot (ensureTunnelProbes), stamps
// monProbeLastEnsured, replaces the mon-client snapshot with the body, and
// drops mon_targets of mon-clients that left the registry. A monClientId
// outside the v2 grammar is 400 before anything changes. A full tunnel
// address pool is not an error: the result lists the mon-clients it left
// without a peer. Any other failure to create a client is reported as 503
// xray_unavailable; whatever was created stays, and the next ensure finishes
// the set.
func (s *MonitoringService) EnsureProbeSet(snapshot []MonClient) (*MonEnsureResult, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return nil, err
	}
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return nil, err
	}
	if subId == "" {
		subId = random.Seq(monProbeSubIdLength)
		if err := s.settingService.SetMonProbeSubId(subId); err != nil {
			return nil, err
		}
	}

	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	created := []MonProbeRef{}
	present := 0
	unallocated := []string{}
	for _, ib := range inbounds {
		switch {
		case monXrayProtocols[ib.Protocol]:
			made, err := s.ensureXrayProbe(ib, subId)
			if err != nil {
				return nil, errXrayUnavailable(err)
			}
			if made {
				created = append(created, MonProbeRef{Kind: model.MonInboundKindXray, InboundId: ib.Id})
			}
			present++
		case ib.Protocol == model.AmneziaWG:
			made, peers, missed, err := s.ensureTunnelProbes(snapshot)
			if err != nil {
				return nil, errXrayUnavailable(err)
			}
			created = append(created, made...)
			present += peers
			unallocated = missed
		}
	}

	now := time.Now().UnixMilli()
	if err := s.settingService.SetMonProbeLastEnsured(now); err != nil {
		return nil, err
	}
	if err := s.setRegistrySnapshot(snapshot); err != nil {
		return nil, err
	}
	if err := s.dropTargetsOutside(snapshot); err != nil {
		return nil, err
	}

	rev, err := s.Revision()
	if err != nil {
		return nil, err
	}
	return &MonEnsureResult{SubId: subId, Revision: rev, LastEnsured: now, Created: created, Present: present, Unallocated: unallocated}, nil
}

// dropTargetsOutside removes mon_targets rows whose mon-client is not in the
// snapshot. Events and stats stay until retention (§2.1).
func (s *MonitoringService) dropTargetsOutside(snapshot []MonClient) error {
	db := database.GetDB()
	if len(snapshot) == 0 {
		return db.Where("1 = 1").Delete(&model.MonTarget{}).Error
	}
	ids := make([]string, 0, len(snapshot))
	for _, c := range snapshot {
		ids = append(ids, c.Id)
	}
	return db.Where("mon_client_id NOT IN ?", ids).Delete(&model.MonTarget{}).Error
}

// tunnelProbeConf renders the AmneziaWG probe .conf the way the panel renders
// every client config, with the endpoint host chosen by the caller: the
// override host for the proxy path, the given host for the direct path.
func (s *MonitoringService) tunnelProbeConf(client *model.TunnelClient, host string, useOverride bool) (string, error) {
	server, err := s.awgService.GetServer()
	if err != nil {
		return "", err
	}
	if useOverride {
		overrideHost, on := s.settingService.GetProxyOverride()
		return tunnel.GenerateClientConfig(tunnel.AWG, withProxyOverride(server, overrideHost, on), *client), nil
	}
	direct := *server
	direct.Endpoint = tunnel.ReplaceEndpointHost(server.Endpoint, host)
	return tunnel.GenerateClientConfig(tunnel.AWG, &direct, *client), nil
}

// ProbeConfigs is GET /probe/configs: the probe set's material for one path.
// With an empty host the links carry the host override (path "proxy"); with a
// host they carry that host instead (path "direct"). Disabled inbounds are
// left out, as in a subscription, so mon-server sees them PAUSED.
//
// hop and edge are ?hop= and its synonym ?edge= (proxy-chain.md §6.1). Until
// per-hop probing lands no hop is known, so any name is 409 unknown_hop
// (unknown_edge when asked through ?edge= alone) rather than the proxy path.
func (s *MonitoringService) ProbeConfigs(host, hop, edge string) (*MonProbeConfigs, error) {
	if err := unknownHop(hop, edge); err != nil {
		return nil, err
	}
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return nil, err
	}
	if subId == "" {
		return nil, ErrProbeNotEnsured
	}
	host = strings.TrimSpace(host)
	useOverride := host == ""
	path := model.MonPathDirect
	if useOverride {
		if _, on := s.settingService.GetProxyOverride(); !on {
			return nil, ErrOverrideDisabled
		}
		path = model.MonPathProxy
	}

	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return nil, err
	}
	items := []MonProbeItem{}
	for _, ib := range inbounds {
		if !ib.Enable {
			continue
		}
		switch {
		case monXrayProtocols[ib.Protocol]:
			links := s.links()
			if links == nil {
				return nil, ErrLinksNotWired
			}
			clients, err := s.inboundService.GetClients(ib)
			if err != nil {
				return nil, err
			}
			probe := findClient(clients, ProbeXrayEmail(ib.Id))
			if probe == nil {
				continue
			}
			// One link per inbound: the first, should a renderer hand back a
			// multi-link inbound (§4.3).
			link, _, _ := strings.Cut(links.ProbeLink(ib, probe.Email, host, useOverride), "\n")
			if link == "" {
				continue
			}
			items = append(items, MonProbeItem{Kind: model.MonInboundKindXray, InboundId: ib.Id, Link: link})
		case ib.Protocol == model.AmneziaWG:
			peers, err := s.tunnelProbeItems(path, host, useOverride)
			if err != nil {
				return nil, err
			}
			items = append(items, peers...)
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		if items[i].InboundId != items[j].InboundId {
			return items[i].InboundId < items[j].InboundId
		}
		return items[i].MonClientId < items[j].MonClientId
	})

	rev, err := s.Revision()
	if err != nil {
		return nil, err
	}
	return &MonProbeConfigs{Revision: rev, Path: path, Items: items}, nil
}

// tunnelProbeItems renders the AmneziaWG items of one path: for each
// mon-client of the registry snapshot, its peer for that path, named by
// monClientId. A mon-client without a peer (the pool was full, or ensure has
// not run since it joined) has no item.
func (s *MonitoringService) tunnelProbeItems(path, host string, useOverride bool) ([]MonProbeItem, error) {
	clients, err := s.awgService.GetClients()
	if err != nil {
		return nil, err
	}
	byEmail := map[string]*model.TunnelClient{}
	for i := range clients {
		if IsProbeAccount(clients[i].Email) {
			byEmail[clients[i].Email] = &clients[i]
		}
	}
	items := []MonProbeItem{}
	for _, mc := range sortedMonClients(s.RegistrySnapshot()) {
		peer := byEmail[ProbeTunnelEmail(mc.Id, path)]
		if peer == nil {
			continue
		}
		conf, err := s.tunnelProbeConf(peer, host, useOverride)
		if err != nil {
			return nil, err
		}
		items = append(items, MonProbeItem{Kind: model.MonInboundKindAwg, InboundId: 0, MonClientId: mc.Id, Filename: peer.Email, Conf: conf})
	}
	return items, nil
}

// unknownHop answers the ?hop= / ?edge= mode of GET /probe/configs before
// the chain registry is consulted for it: blank parameters are absent, any
// name is unknown. Two different names are refused as unknown_hop as well.
func unknownHop(hop, edge string) error {
	hop, edge = strings.TrimSpace(hop), strings.TrimSpace(edge)
	switch {
	case hop == "" && edge == "":
		return nil
	case hop == "":
		return ErrUnknownEdge
	default:
		return ErrUnknownHop
	}
}

// removeXrayProbe deletes the probe client of an inbound, with its traffic
// row. DelInboundClientByEmail refuses to leave an inbound with no clients,
// so an inbound that holds only the probe is emptied by hand.
func (s *MonitoringService) removeXrayProbe(ib *model.Inbound) error {
	email := ProbeXrayEmail(ib.Id)
	clients, err := s.inboundService.GetClients(ib)
	if err != nil {
		return err
	}
	if findClient(clients, email) == nil {
		return nil
	}
	if len(clients) > 1 {
		_, err := s.inboundService.DelInboundClientByEmail(ib.Id, email)
		return err
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(ib.Settings), &settings); err != nil {
		return err
	}
	settings["clients"] = []any{}
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	db := database.GetDB()
	if err := s.inboundService.DelClientIPs(db, email); err != nil {
		return err
	}
	if err := s.inboundService.DelClientStat(db, email); err != nil && !database.IsNotFound(err) {
		return err
	}
	if ib.Enable {
		s.xrayService.SetToNeedRestart()
	}
	return db.Model(&model.Inbound{}).Where("id = ?", ib.Id).Update("settings", string(raw)).Error
}

// DeleteProbeSet is DELETE /probe: every probe account goes (xray and
// AmneziaWG, with their traffic rows), and the panel forgets the subId, the
// last ensure and the mon-client snapshot. The hourly job calls it too when
// the set outlives monProbeTtlHours without an ensure.
func (s *MonitoringService) DeleteProbeSet() error {
	inbounds, err := s.inboundService.GetAllInbounds()
	if err != nil {
		return err
	}
	for _, ib := range inbounds {
		if monXrayProtocols[ib.Protocol] {
			if err := s.removeXrayProbe(ib); err != nil {
				return fmt.Errorf("inbound %d: %w", ib.Id, err)
			}
		}
	}
	tunnelClients, err := s.awgService.GetClients()
	if err != nil {
		return err
	}
	for _, c := range tunnelClients {
		if IsProbeAccount(c.Email) {
			if err := s.awgService.DeleteClient(c.Id); err != nil {
				return err
			}
		}
	}
	if err := s.settingService.SetMonProbeSubId(""); err != nil {
		return err
	}
	if err := s.settingService.SetMonProbeLastEnsured(0); err != nil {
		return err
	}
	return s.setRegistrySnapshot(nil)
}
