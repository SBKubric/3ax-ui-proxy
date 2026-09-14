package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
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
// lives in package sub, which imports this package; the controller wires
// sub.SubService in (it satisfies ProbeLinkRenderer). Without it ProbeConfigs
// cannot serve xray items.
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

// MonContractVersion is the X-Mon-Contract the panel speaks.
const MonContractVersion = 1

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
type MonEnsureResult struct {
	SubId       string          `json:"subId"`
	Revision    string          `json:"revision"`
	LastEnsured int64           `json:"lastEnsured"`
	Created     []MonInboundRef `json:"created"`
	Present     int             `json:"present"`
}

// MonProbeItem is one config of GET /probe/configs: a link for an xray
// inbound, a .conf for the AmneziaWG server.
type MonProbeItem struct {
	Kind      string `json:"kind"`
	InboundId int    `json:"inboundId"`
	Link      string `json:"link,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Conf      string `json:"conf,omitempty"`
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

// revision is the first 16 hex characters of SHA-256 over the canonical JSON
// of {override, inbounds[{kind,inboundId,protocol,port,enable}], probeSubId}
// (monitoring-contract.md §4.2). Keys sorted, no whitespace, inbounds already
// sorted by (kind, inboundId); tag and remark are left out so a rename does
// not rebuild targets.
func revision(override MonOverride, inbounds []MonInbound, probeSubId string) string {
	type hashedInbound struct {
		Enable    bool   `json:"enable"`
		InboundId int    `json:"inboundId"`
		Kind      string `json:"kind"`
		Port      int    `json:"port"`
		Protocol  string `json:"protocol"`
	}
	hashed := make([]hashedInbound, 0, len(inbounds))
	for _, ib := range inbounds {
		hashed = append(hashed, hashedInbound{ib.Enable, ib.InboundId, ib.Kind, ib.Port, ib.Protocol})
	}
	// encoding/json writes struct fields in declaration order and map keys
	// sorted; both are alphabetical here, which is the canonical form.
	canonical, _ := json.Marshal(map[string]any{
		"inbounds":   hashed,
		"override":   map[string]any{"enabled": override.Enabled, "host": override.Host},
		"probeSubId": probeSubId,
	})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])[:16]
}

// Revision is the current revision string, as GET /state reports it.
func (s *MonitoringService) Revision() (string, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return "", err
	}
	subId, _ := s.settingService.GetMonProbeSubId()
	return revision(s.override(), inbounds, subId), nil
}

// State is GET /state: the sanitised inbounds, the override, the probe set
// and the revision. No side effects.
func (s *MonitoringService) State() (*MonState, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	override := s.override()
	subId, _ := s.settingService.GetMonProbeSubId()
	lastEnsured, _ := s.settingService.GetMonProbeLastEnsured()
	stale, err := s.settingService.GetMonStaleMinutes()
	if err != nil {
		stale = 15
	}
	st := &MonState{
		Contract:     MonContractVersion,
		PanelVersion: config.GetVersion(),
		ServerTime:   time.Now().UnixMilli(),
		Revision:     revision(override, inbounds, subId),
		Override:     override,
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

// ensureTunnelProbe creates the AmneziaWG probe client unless it exists.
//
// The shared subId is not bound to it: tunnel clients carry no subId until the
// tunnel subscription feature (docs/spec/tunnel-subscription.md) lands; its
// TunnelSubscriptionService.Set is the hook to call here when it does.
func (s *MonitoringService) ensureTunnelProbe() (bool, error) {
	clients, err := s.awgService.GetClients()
	if err != nil {
		return false, err
	}
	for _, c := range clients {
		if IsProbeAccount(c.Email) {
			return false, nil
		}
	}
	probe := NewProbeTunnelClient()
	if err := s.awgService.addClient(&probe, true); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureProbeSet is POST /probe/ensure. Idempotent: it mints the probe subId
// on first call, creates whichever probe clients are missing (a deleted probe
// comes back with a fresh identity and the same subId), stamps
// monProbeLastEnsured, replaces the mon-client snapshot with the body, and
// drops mon_targets of mon-clients that left the registry. A failure to
// create a client is reported as 503 xray_unavailable; whatever was created
// stays, and the next ensure finishes the set.
func (s *MonitoringService) EnsureProbeSet(snapshot []MonClient) (*MonEnsureResult, error) {
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
	created := []MonInboundRef{}
	present := 0
	for _, ib := range inbounds {
		switch {
		case monXrayProtocols[ib.Protocol]:
			made, err := s.ensureXrayProbe(ib, subId)
			if err != nil {
				return nil, errXrayUnavailable(err)
			}
			if made {
				created = append(created, MonInboundRef{model.MonInboundKindXray, ib.Id})
			}
			present++
		case ib.Protocol == model.AmneziaWG:
			made, err := s.ensureTunnelProbe()
			if err != nil {
				return nil, errXrayUnavailable(err)
			}
			if made {
				created = append(created, MonInboundRef{model.MonInboundKindAwg, 0})
			}
			present++
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
	return &MonEnsureResult{SubId: subId, Revision: rev, LastEnsured: now, Created: created, Present: present}, nil
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
func (s *MonitoringService) ProbeConfigs(host string) (*MonProbeConfigs, error) {
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
			if s.Links == nil {
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
			link := s.Links.ProbeLink(ib, probe.Email, host, useOverride)
			if link == "" {
				continue
			}
			items = append(items, MonProbeItem{Kind: model.MonInboundKindXray, InboundId: ib.Id, Link: link})
		case ib.Protocol == model.AmneziaWG:
			clients, err := s.awgService.GetClients()
			if err != nil {
				return nil, err
			}
			for i := range clients {
				if !IsProbeAccount(clients[i].Email) {
					continue
				}
				conf, err := s.tunnelProbeConf(&clients[i], host, useOverride)
				if err != nil {
					return nil, err
				}
				items = append(items, MonProbeItem{Kind: model.MonInboundKindAwg, InboundId: 0, Filename: clients[i].Email, Conf: conf})
				break
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].InboundId < items[j].InboundId
	})

	rev, err := s.Revision()
	if err != nil {
		return nil, err
	}
	return &MonProbeConfigs{Revision: rev, Path: path, Items: items}, nil
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
