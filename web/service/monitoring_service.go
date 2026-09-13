package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
)

// MonitoringService is the panel side of the monitoring contract
// (docs/spec/monitoring-contract.md, docs/spec/monitoring-panel.md §4).
// The panel never calls out: mon-server polls State, asks for the probe set,
// fetches probe configs and pushes events and aggregates. Everything that
// changes lives on mon-server; here is a store, a probe-account factory and a
// cache of the registry snapshot.
type MonitoringService struct {
	settingService SettingService
	inboundService InboundService
	xrayService    XrayService
	awgService     AwgService
}

// MonContract is the contract version carried in the path and the header.
const MonContract = 1

// monLastContactPersistEvery bounds how often an authorized request writes
// monLastContact to SQLite; memory is always current.
const monLastContactPersistEvery = 10 * time.Second

// MonClient is one mon-client of the registry snapshot mon-server sends with
// every POST /probe/ensure. State is what mon-server said; the panel never
// recomputes it.
type MonClient struct {
	Id            string `json:"id"`
	Name          string `json:"name"`
	Region        string `json:"region"`
	State         string `json:"state"`
	LastHeartbeat int64  `json:"lastHeartbeat"`
}

// MonInbound is one monitored inbound as GET /state describes it: sanitised,
// no settings, no keys.
type MonInbound struct {
	Kind      string `json:"kind"`
	InboundId int    `json:"inboundId"`
	Tag       string `json:"tag"`
	Remark    string `json:"remark"`
	Protocol  string `json:"protocol"`
	Port      int    `json:"port"`
	Enable    bool   `json:"enable"`
}

// MonInboundRef names an inbound in a response.
type MonInboundRef struct {
	Kind      string `json:"kind"`
	InboundId int    `json:"inboundId"`
}

// MonOverride is the host override as GET /state reports it.
type MonOverride struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
}

// MonProbeInfo describes the probe set; SubId is null until the first ensure.
type MonProbeInfo struct {
	SubId       *string `json:"subId"`
	LastEnsured int64   `json:"lastEnsured"`
}

// MonStaleInfo carries the STALE threshold.
type MonStaleInfo struct {
	ThresholdMinutes int `json:"thresholdMinutes"`
}

// MonState is the body of GET /state.
type MonState struct {
	Contract     int          `json:"contract"`
	PanelVersion string       `json:"panelVersion"`
	ServerTime   int64        `json:"serverTime"`
	Revision     string       `json:"revision"`
	Override     MonOverride  `json:"override"`
	Probe        MonProbeInfo `json:"probe"`
	Inbounds     []MonInbound `json:"inbounds"`
	Stale        MonStaleInfo `json:"stale"`
}

// MonEnsureResult is the body of POST /probe/ensure.
type MonEnsureResult struct {
	SubId       string          `json:"subId"`
	Revision    string          `json:"revision"`
	LastEnsured int64           `json:"lastEnsured"`
	Created     []MonInboundRef `json:"created"`
	Present     int             `json:"present"`
}

// MonProbeItem is one probe config: a share link for an xray inbound, a
// .conf text for the AmneziaWG server.
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

// MonError is an error the contract has a status and a stable code for.
type MonError struct {
	Status  int
	Code    string
	Message string
}

func (e *MonError) Error() string { return e.Code + ": " + e.Message }

// Errors named by the contract.
var (
	ErrMonOverrideDisabled = &MonError{http.StatusConflict, "override_disabled", "the host override is disabled, there is no proxy path"}
	ErrMonProbeNotEnsured  = &MonError{http.StatusConflict, "probe_not_ensured", "the probe set has not been created yet, call POST /probe/ensure first"}
	ErrMonXrayUnavailable  = &MonError{http.StatusServiceUnavailable, "xray_unavailable", "xray is not running, probe accounts cannot be added"}
	ErrMonAwgUnavailable   = &MonError{http.StatusServiceUnavailable, "awg_unavailable", "the AmneziaWG probe peer could not be created"}
)

// ProbeLinkRenderer renders the share links of the probe accounts of every
// enabled xray inbound, keyed by inbound id. With override set the links
// carry the host override (path proxy); otherwise host is the address (path
// direct). The sub package registers the implementation: it owns link
// rendering and imports this package, so it cannot be imported from here.
type ProbeLinkRenderer func(host string, override bool) (map[int]string, error)

// monRuntime is the in-memory state the contract keeps between requests:
// the registry snapshot cache, the last authorized contact, and the STALE
// flag — the only state the panel derives by itself.
var monRuntime = struct {
	mu sync.Mutex

	snapshot       []MonClient
	snapshotLoaded bool

	lastContact          int64
	lastContactLoaded    bool
	lastContactPersisted int64

	stale      bool
	staleSince int64

	renderLinks ProbeLinkRenderer
	ensureMu    sync.Mutex
}{}

// monXrayRunning is what EnsureProbeSet asks before adding an xray probe
// account; a variable so tests can run without a core.
var monXrayRunning = xrayProcRunning

// RegisterProbeLinkRenderer installs the share-link renderer (see
// ProbeLinkRenderer). Called once from the sub package.
func RegisterProbeLinkRenderer(r ProbeLinkRenderer) {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	monRuntime.renderLinks = r
}

func probeLinkRenderer() ProbeLinkRenderer {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	return monRuntime.renderLinks
}

// monitoredProtocols are the xray inbounds v1 monitors.
var monitoredProtocols = []model.Protocol{model.VLESS, model.VMESS, model.Trojan, model.Shadowsocks}

// monitoredXrayInbounds lists every xray inbound of a monitored protocol,
// disabled ones included (they show as PAUSED), ordered by id.
func (s *MonitoringService) monitoredXrayInbounds() ([]*model.Inbound, error) {
	db := database.GetDB()
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Where("protocol IN ?", monitoredProtocols).Order("id asc").Find(&inbounds).Error
	return inbounds, err
}

// awgInbound returns the AmneziaWG inbound row when the server was created.
func (s *MonitoringService) awgInbound() (*model.Inbound, error) {
	db := database.GetDB()
	var inbound model.Inbound
	err := db.Model(model.Inbound{}).Where("protocol = ?", model.AmneziaWG).First(&inbound).Error
	if database.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inbound, nil
}

// Inbounds is the sanitised inbound list of GET /state.
func (s *MonitoringService) Inbounds() ([]MonInbound, error) {
	xrayInbounds, err := s.monitoredXrayInbounds()
	if err != nil {
		return nil, err
	}
	out := make([]MonInbound, 0, len(xrayInbounds)+1)
	if awg, err := s.awgInbound(); err != nil {
		return nil, err
	} else if awg != nil {
		server, err := s.awgService.GetServer()
		if err != nil {
			return nil, err
		}
		out = append(out, MonInbound{
			Kind: model.MonKindAwg, InboundId: 0, Tag: awg.Tag, Remark: awg.Remark,
			Protocol: model.MonKindAwg, Port: server.ListenPort, Enable: server.Enable,
		})
	}
	for _, ib := range xrayInbounds {
		out = append(out, MonInbound{
			Kind: model.MonKindXray, InboundId: ib.Id, Tag: ib.Tag, Remark: ib.Remark,
			Protocol: string(ib.Protocol), Port: ib.LinkPort(), Enable: ib.Enable,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].InboundId < out[j].InboundId
	})
	return out, nil
}

// override reports the host override as the contract sees it: host is empty
// when the override is off.
func (s *MonitoringService) override() MonOverride {
	host, on := s.settingService.GetProxyOverride()
	return MonOverride{Enabled: on, Host: host}
}

// Revision is the first 16 hex characters of the SHA-256 of the canonical
// JSON of {override, inbounds[{kind,inboundId,protocol,port,enable}],
// probeSubId} (contract §4.2): keys sorted, no whitespace, inbounds ordered
// by (kind, inboundId). Remark and tag are left out so a rename does not move
// every target.
func monRevision(override MonOverride, inbounds []MonInbound, probeSubId string) string {
	canonical := make([]map[string]any, 0, len(inbounds))
	for _, ib := range inbounds {
		canonical = append(canonical, map[string]any{
			"kind": ib.Kind, "inboundId": ib.InboundId, "protocol": ib.Protocol, "port": ib.Port, "enable": ib.Enable,
		})
	}
	raw, err := json.Marshal(map[string]any{
		"override":   map[string]any{"enabled": override.Enabled, "host": override.Host},
		"inbounds":   canonical,
		"probeSubId": probeSubId,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// revision computes the current revision from stored state.
func (s *MonitoringService) revision() (string, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return "", err
	}
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return "", err
	}
	return monRevision(s.override(), inbounds, subId), nil
}

// State is GET /state.
func (s *MonitoringService) State() (*MonState, error) {
	inbounds, err := s.Inbounds()
	if err != nil {
		return nil, err
	}
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return nil, err
	}
	lastEnsured, err := s.settingService.GetMonProbeLastEnsured()
	if err != nil {
		return nil, err
	}
	staleMinutes, err := s.settingService.GetMonStaleMinutes()
	if err != nil {
		return nil, err
	}
	override := s.override()
	state := &MonState{
		Contract:     MonContract,
		PanelVersion: config.GetVersion(),
		ServerTime:   time.Now().UnixMilli(),
		Revision:     monRevision(override, inbounds, subId),
		Override:     override,
		Probe:        MonProbeInfo{LastEnsured: lastEnsured},
		Inbounds:     inbounds,
		Stale:        MonStaleInfo{ThresholdMinutes: staleMinutes},
	}
	if subId != "" {
		state.Probe.SubId = &subId
	}
	return state, nil
}

// probeSubId returns the probe set's subId, creating one on first use. A
// probe account that already carries a subId (the settings row was lost, or
// overwritten by a stale settings form) is adopted rather than orphaned.
func (s *MonitoringService) probeSubId(xrayInbounds []*model.Inbound) (string, error) {
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return "", err
	}
	if subId != "" {
		return subId, nil
	}
	for _, ib := range xrayInbounds {
		clients, err := s.inboundService.GetClients(ib)
		if err != nil {
			continue
		}
		if c := findProbeClient(clients, ib.Id); c != nil && c.SubID != "" {
			subId = c.SubID
			break
		}
	}
	if subId == "" {
		subId = strings.ToLower(random.Seq(16))
	}
	return subId, s.settingService.SetMonProbeSubId(subId)
}

// findProbeClient returns the probe account of an inbound, if present.
func findProbeClient(clients []model.Client, inboundId int) *model.Client {
	email := ProbeEmail(model.MonKindXray, inboundId)
	for i := range clients {
		if strings.EqualFold(clients[i].Email, email) {
			return &clients[i]
		}
	}
	return nil
}

// awgProbeClient returns the AmneziaWG probe peer, if present.
func (s *MonitoringService) awgProbeClient() (*model.TunnelClient, error) {
	clients, err := s.awgService.GetClients()
	if err != nil {
		return nil, err
	}
	email := ProbeEmail(model.MonKindAwg, 0)
	for i := range clients {
		if strings.EqualFold(clients[i].Email, email) {
			return &clients[i], nil
		}
	}
	return nil, nil
}

// EnsureProbeSet is POST /probe/ensure (spec §4.3). Idempotent: it creates
// the probe subId on first use, adds the probe account every monitored
// inbound lacks (through the xray API, no restart), records the time, and
// replaces the registry snapshot cache with the body — dropping target rows
// of mon-clients that left the registry. When xray is down and an account is
// missing it stops with ErrMonXrayUnavailable; whatever was created stays and
// the next ensure finishes the set.
func (s *MonitoringService) EnsureProbeSet(snapshot []MonClient) (*MonEnsureResult, error) {
	monRuntime.ensureMu.Lock()
	defer monRuntime.ensureMu.Unlock()

	if err := s.replaceSnapshot(snapshot); err != nil {
		return nil, err
	}

	xrayInbounds, err := s.monitoredXrayInbounds()
	if err != nil {
		return nil, err
	}
	subId, err := s.probeSubId(xrayInbounds)
	if err != nil {
		return nil, err
	}

	created := make([]MonInboundRef, 0)
	present := 0
	needRestart := false
	for _, ib := range xrayInbounds {
		clients, err := s.inboundService.GetClients(ib)
		if err != nil {
			return nil, err
		}
		if findProbeClient(clients, ib.Id) != nil {
			present++
			continue
		}
		if !monXrayRunning() {
			return nil, ErrMonXrayUnavailable
		}
		restart, err := s.inboundService.addProbeClient(ib, probeXrayClient(ib, clients, subId))
		if err != nil {
			return nil, err
		}
		needRestart = needRestart || restart
		created = append(created, MonInboundRef{Kind: model.MonKindXray, InboundId: ib.Id})
		present++
	}
	if needRestart {
		s.xrayService.SetToNeedRestart()
	}

	if awg, err := s.awgInbound(); err != nil {
		return nil, err
	} else if awg != nil {
		peer, err := s.awgProbeClient()
		if err != nil {
			return nil, err
		}
		if peer == nil {
			client := probeTunnelClient()
			grantTunnelProbe(client)
			if err := s.awgService.AddClient(client); err != nil {
				logger.Warning("monitoring: AmneziaWG probe peer not created:", err)
				return nil, ErrMonAwgUnavailable
			}
			created = append(created, MonInboundRef{Kind: model.MonKindAwg, InboundId: 0})
		}
		present++
	}

	now := time.Now().UnixMilli()
	if err := s.settingService.SetMonProbeLastEnsured(now); err != nil {
		return nil, err
	}
	revision, err := s.revision()
	if err != nil {
		return nil, err
	}
	return &MonEnsureResult{SubId: subId, Revision: revision, LastEnsured: now, Created: created, Present: present}, nil
}

// ProbeConfigs is GET /probe/configs[?host=] (spec §4.4). Without host the
// links carry the host override (path proxy; ErrMonOverrideDisabled when it
// is off); with host they carry that address (path direct). Disabled
// inbounds are left out, as in the subscription, so mon-server sees PAUSED.
func (s *MonitoringService) ProbeConfigs(host string) (*MonProbeConfigs, error) {
	subId, err := s.settingService.GetMonProbeSubId()
	if err != nil {
		return nil, err
	}
	if subId == "" {
		return nil, ErrMonProbeNotEnsured
	}
	path := model.MonPathDirect
	override := false
	endpointHost := host
	if host == "" {
		overrideHost, on := s.settingService.GetProxyOverride()
		if !on {
			return nil, ErrMonOverrideDisabled
		}
		path, override, endpointHost = model.MonPathProxy, true, overrideHost
	}

	items := make([]MonProbeItem, 0)
	if awg, err := s.awgInbound(); err != nil {
		return nil, err
	} else if awg != nil {
		server, err := s.awgService.GetServer()
		if err != nil {
			return nil, err
		}
		peer, err := s.awgProbeClient()
		if err != nil {
			return nil, err
		}
		if server.Enable && peer != nil && peer.Enable {
			conf := tunnel.GenerateClientConfig(tunnel.AWG, withProxyOverride(server, endpointHost, true), *peer)
			items = append(items, MonProbeItem{Kind: model.MonKindAwg, InboundId: 0, Filename: peer.Email, Conf: conf})
		}
	}

	render := probeLinkRenderer()
	if render == nil {
		return nil, errors.New("monitoring: no probe link renderer registered")
	}
	links, err := render(host, override)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(links))
	for id := range links {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		items = append(items, MonProbeItem{Kind: model.MonKindXray, InboundId: id, Link: links[id]})
	}

	revision, err := s.revision()
	if err != nil {
		return nil, err
	}
	return &MonProbeConfigs{Revision: revision, Path: path, Items: items}, nil
}

// DeleteProbeSet is DELETE /probe and the hourly TTL sweep (spec §4.5): every
// probe account goes, with its traffic row; the subId, the ensure time and the
// registry snapshot are forgotten, and with the snapshot every target row.
func (s *MonitoringService) DeleteProbeSet() error {
	monRuntime.ensureMu.Lock()
	defer monRuntime.ensureMu.Unlock()

	xrayInbounds, err := s.monitoredXrayInbounds()
	if err != nil {
		return err
	}
	needRestart := false
	for _, ib := range xrayInbounds {
		_, restart, err := s.inboundService.removeProbeClient(ib)
		if err != nil {
			return fmt.Errorf("inbound %d: %w", ib.Id, err)
		}
		needRestart = needRestart || restart
	}
	if needRestart {
		s.xrayService.SetToNeedRestart()
	}
	if peer, err := s.awgProbeClient(); err != nil {
		return err
	} else if peer != nil {
		if err := s.awgService.DeleteClient(peer.Id); err != nil {
			return err
		}
	}
	if err := s.settingService.SetMonProbeSubId(""); err != nil {
		return err
	}
	if err := s.settingService.SetMonProbeLastEnsured(0); err != nil {
		return err
	}
	if err := s.replaceSnapshot(nil); err != nil {
		return err
	}
	return nil
}

// ProbeAccountCount counts the probe accounts present, for the settings page.
func (s *MonitoringService) ProbeAccountCount() (int, error) {
	xrayInbounds, err := s.monitoredXrayInbounds()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, ib := range xrayInbounds {
		clients, err := s.inboundService.GetClients(ib)
		if err != nil {
			return 0, err
		}
		if findProbeClient(clients, ib.Id) != nil {
			n++
		}
	}
	if peer, err := s.awgProbeClient(); err != nil {
		return 0, err
	} else if peer != nil {
		n++
	}
	return n, nil
}

// Snapshot returns the cached registry snapshot, loading it from the settings
// after a restart.
func (s *MonitoringService) Snapshot() []MonClient {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	if !monRuntime.snapshotLoaded {
		raw, err := s.settingService.GetMonClientsSnapshot()
		if err == nil {
			var clients []MonClient
			if json.Unmarshal([]byte(raw), &clients) == nil {
				monRuntime.snapshot = clients
			}
		}
		monRuntime.snapshotLoaded = true
	}
	return slices.Clone(monRuntime.snapshot)
}

// SnapshotIds is the set of mon-client ids in the snapshot; a target row whose
// mon-client is not in it is not live.
func (s *MonitoringService) SnapshotIds() map[string]struct{} {
	snapshot := s.Snapshot()
	ids := make(map[string]struct{}, len(snapshot))
	for _, c := range snapshot {
		ids[c.Id] = struct{}{}
	}
	return ids
}

// replaceSnapshot stores a new registry snapshot (memory and setting) and
// drops the target rows of mon-clients that are no longer in it. Events and
// aggregates stay until retention.
func (s *MonitoringService) replaceSnapshot(snapshot []MonClient) error {
	if snapshot == nil {
		snapshot = []MonClient{}
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := s.settingService.SetMonClientsSnapshot(string(raw)); err != nil {
		return err
	}
	monRuntime.mu.Lock()
	monRuntime.snapshot = slices.Clone(snapshot)
	monRuntime.snapshotLoaded = true
	monRuntime.mu.Unlock()

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

// TouchLastContact records an authorized mon-server request at now: always in
// memory, in the settings at most every monLastContactPersistEvery. It
// reports whether the panel was STALE before this contact, so the caller can
// announce the recovery.
func (s *MonitoringService) TouchLastContact(now time.Time) (wasStale bool, staleSince int64) {
	ms := now.UnixMilli()
	monRuntime.mu.Lock()
	monRuntime.lastContact = ms
	monRuntime.lastContactLoaded = true
	persist := ms-monRuntime.lastContactPersisted >= monLastContactPersistEvery.Milliseconds()
	if persist {
		monRuntime.lastContactPersisted = ms
	}
	wasStale, staleSince = monRuntime.stale, monRuntime.staleSince
	monRuntime.stale, monRuntime.staleSince = false, 0
	monRuntime.mu.Unlock()
	if persist {
		if err := s.settingService.SetMonLastContact(ms); err != nil {
			logger.Warning("monitoring: could not store monLastContact:", err)
		}
	}
	return wasStale, staleSince
}

// LastContact is the time of the last authorized request, ms; zero while
// mon-server has never called. The memory copy wins over the setting, which
// lags by up to monLastContactPersistEvery and can be rolled back by a stale
// settings form.
func (s *MonitoringService) LastContact() int64 {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	if !monRuntime.lastContactLoaded {
		if v, err := s.settingService.GetMonLastContact(); err == nil {
			monRuntime.lastContact = v
		}
		monRuntime.lastContactLoaded = true
	}
	return monRuntime.lastContact
}

// Stale reports the panel's STALE flag and when the silence started.
func (s *MonitoringService) Stale() (bool, int64) {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	return monRuntime.stale, monRuntime.staleSince
}

// setStale raises the STALE flag once; it reports whether this call raised it.
func (s *MonitoringService) setStale(since int64) bool {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	if monRuntime.stale {
		return false
	}
	monRuntime.stale, monRuntime.staleSince = true, since
	return true
}

// resetMonRuntime forgets the in-memory state; tests call it between
// databases.
func resetMonRuntime() {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	monRuntime.snapshot, monRuntime.snapshotLoaded = nil, false
	monRuntime.lastContact, monRuntime.lastContactLoaded, monRuntime.lastContactPersisted = 0, false, 0
	monRuntime.stale, monRuntime.staleSince = false, 0
}

// MonStatusHooks hear the panel's own transitions: Stale when the silence
// crosses the threshold, Back on the first authorized request after it. The
// Telegram side installs them; nil hooks are skipped.
type MonStatusHooks struct {
	Stale func(since int64)
	Back  func(since, now int64)
}

var monStatusHooks MonStatusHooks

// SetMonStatusHooks installs the STALE/back hooks.
func SetMonStatusHooks(h MonStatusHooks) {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	monStatusHooks = h
}

func currentMonStatusHooks() MonStatusHooks {
	monRuntime.mu.Lock()
	defer monRuntime.mu.Unlock()
	return monStatusHooks
}

// RecordContact is what an authorized mon-server request does: it touches
// the last-contact time and, when the panel was STALE, announces the return.
func (s *MonitoringService) RecordContact(now time.Time) {
	wasStale, since := s.TouchLastContact(now)
	if wasStale {
		if hooks := currentMonStatusHooks(); hooks.Back != nil {
			hooks.Back(since, now.UnixMilli())
		}
	}
}

// ResetMonRuntime forgets the in-memory monitoring state (snapshot cache,
// last contact, STALE flag). Tests call it between databases.
func ResetMonRuntime() {
	resetMonRuntime()
}
