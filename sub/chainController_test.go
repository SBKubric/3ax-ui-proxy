package sub

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// oneInbound is the panel's xray config for these tests: the relayed ports are
// not what they are about, but a document cannot be built without a config.
const oneInbound = `{"inbounds":[{"listen":"0.0.0.0","port":443,"protocol":"vless","tag":"inbound-443"}]}`

// newChainRouter is the sub server's router with nothing on it but the chain
// endpoints, over a registry of its own.
func newChainRouter(t *testing.T) (*gin.Engine, *service.ChainService) {
	t.Helper()
	return newChainRouterWithPanelHost(t, "198.51.100.1")
}

// newChainRouterWithPanelHost is newChainRouter with the owner's chainPanelHost
// spelled out — empty means the panel has not been told its own address, and
// the document falls back to the one the hop dialled.
func newChainRouterWithPanelHost(t *testing.T, panelHost string) (*gin.Engine, *service.ChainService) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(oneInbound), 0o600); err != nil {
		t.Fatalf("write the xray config: %v", err)
	}
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })

	settings := service.SettingService{}
	if err := settings.SetChainPanelHost(panelHost); err != nil {
		t.Fatalf("SetChainPanelHost: %v", err)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewChainController(engine.Group("/"))
	return engine, &service.ChainService{}
}

// enterHop adds a hop and walks it through a join, returning its hop secret.
func enterHop(t *testing.T, registry *service.ChainService, in service.AddHopInput) (*model.ChainHop, string) {
	t.Helper()
	hop, _, _, err := registry.Add(in)
	if err != nil {
		t.Fatalf("Add(%+v): %v", in, err)
	}
	secret := chain.NewSecret()
	if err := registry.MarkJoined(hop.Id, chain.HashSecret(secret), ""); err != nil {
		t.Fatalf("MarkJoined(%s): %v", in.Name, err)
	}
	return hop, secret
}

func hopFromRegistry(t *testing.T, registry *service.ChainService, name string) model.ChainHop {
	t.Helper()
	state, err := registry.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, hop := range state.Hops {
		if hop.Name == name {
			return hop
		}
	}
	t.Fatalf("no hop named %q in the registry", name)
	return model.ChainHop{}
}

func do(engine *gin.Engine, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func documentRequest(secret string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/chain/v1/document", nil)
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	return request
}

func TestChainDocumentNeedsAFirstTierBearer(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, innerSecret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	_, edgeSecret := enterHop(t, registry, service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	for name, secret := range map[string]string{
		"no bearer at all":     "",
		"a secret nobody has":  chain.NewSecret(),
		"a second-tier bearer": edgeSecret,
	} {
		recorder := do(engine, documentRequest(secret))
		if recorder.Code != http.StatusNotFound || recorder.Body.Len() != 0 {
			t.Fatalf("GET /chain/v1/document with %s: %d %q, want a bare 404", name, recorder.Code, recorder.Body.String())
		}
	}

	recorder := do(engine, documentRequest(innerSecret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/document with the first-tier bearer: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestChainDocumentIsTruncatedForTheCaller(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, innerSecret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	enterHop(t, registry, service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})

	recorder := do(engine, documentRequest(innerSecret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/document: %d %q", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type is %q", contentType)
	}
	state, _ := registry.List()
	if etag := recorder.Header().Get("ETag"); etag != service.ETag(state.Revision) {
		t.Fatalf("ETag is %q, want %q", etag, service.ETag(state.Revision))
	}

	var document chain.Document
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the body is not a document: %v", err)
	}
	if document.Self.Name != "inner-1" || document.NextHop.Host != "198.51.100.1" {
		t.Fatalf("self %+v, nextHop %+v", document.Self, document.NextHop)
	}
	if len(document.Hops) != 2 || document.Hops[0].Name != "inner-1" || document.Hops[1].Name != "edge-a" {
		t.Fatalf("hops are %+v, want inner-1 and edge-a", document.Hops)
	}
}

func TestChainDocumentOfADirectEdgeCarriesOnlyItself(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, edgeSecret := enterHop(t, registry, service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	enterHop(t, registry, service.AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})

	recorder := do(engine, documentRequest(edgeSecret))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/document: %d %q", recorder.Code, recorder.Body.String())
	}
	var document chain.Document
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the body is not a document: %v", err)
	}
	if len(document.Hops) != 1 || document.Hops[0].Name != "edge-a" {
		t.Fatalf("a seized edge learned about its neighbour: %+v", document.Hops)
	}
	if document.NextHop.Host != "198.51.100.1" {
		t.Fatalf("nextHop is %+v, want the panel", document.NextHop)
	}
}

func TestChainDocumentAnswers304ToAKnownETag(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, secret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	state, _ := registry.List()

	request := documentRequest(secret)
	request.Header.Set("If-None-Match", service.ETag(state.Revision))
	recorder := do(engine, request)
	if recorder.Code != http.StatusNotModified || recorder.Body.Len() != 0 {
		t.Fatalf("If-None-Match of the current revision: %d %q, want a bodiless 304", recorder.Code, recorder.Body.String())
	}
	if etag := recorder.Header().Get("ETag"); etag != service.ETag(state.Revision) {
		t.Fatalf("the 304 carries ETag %q", etag)
	}

	// A registry write moves the revision, and the same validator stops matching.
	if err := registry.Update(hopFromRegistry(t, registry, "inner-1").Id,
		service.UpdateHopInput{Host: strPtr("10.0.0.8")}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if recorder := do(engine, request); recorder.Code != http.StatusOK {
		t.Fatalf("after a new revision the stale validator got %d", recorder.Code)
	}
}

func strPtr(value string) *string { return &value }

func TestChainDocumentRecordsTheWave(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, secret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	enterHop(t, registry, service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	state, _ := registry.List()

	outer, err := json.Marshal([]chain.OuterAck{{Name: "edge-a", LastRevision: state.Revision, LastSeen: 1}})
	if err != nil {
		t.Fatalf("marshal the acknowledgements: %v", err)
	}
	request := documentRequest(secret)
	request.Header.Set(chain.SeenHeader, strconv.FormatInt(state.Revision, 10))
	request.Header.Set(chain.OuterHeader, base64.StdEncoding.EncodeToString(outer))
	if recorder := do(engine, request); recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/document: %d", recorder.Code)
	}

	if inner := hopFromRegistry(t, registry, "inner-1"); inner.LastRevision != state.Revision || inner.LastSeenAt == 0 {
		t.Fatalf("the caller's poll was not recorded: %+v", inner)
	}
	if edge := hopFromRegistry(t, registry, "edge-a"); edge.LastRevision != state.Revision || edge.LastSeenAt == 0 {
		t.Fatalf("the outer acknowledgement was not recorded: %+v", edge)
	}
}

func TestChainDocumentIgnoresAGarbledWaveHeader(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, secret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})

	request := documentRequest(secret)
	request.Header.Set(chain.SeenHeader, "not a number")
	request.Header.Set(chain.OuterHeader, "not base64 at all!!")
	if recorder := do(engine, request); recorder.Code != http.StatusOK {
		t.Fatalf("a garbled wave header cost the document: %d", recorder.Code)
	}

	request = documentRequest(secret)
	request.Header.Set(chain.OuterHeader, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 8<<10))))
	if recorder := do(engine, request); recorder.Code != http.StatusOK {
		t.Fatalf("an oversized wave header cost the document: %d", recorder.Code)
	}
}

func joinRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal the join body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/chain/v1/join", strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestChainJoinHandsOutTheSecret(t *testing.T) {
	engine, registry := newChainRouter(t)
	hop, token, _, err := registry.Add(service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	request := joinRequest(t, map[string]any{"token": token, "host": "real.example.net", "subPort": 8443, "subScheme": "https"})
	request.Header.Set(chain.ObservedHeader, "198.51.100.44")
	recorder := do(engine, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /chain/v1/join: %d %q", recorder.Code, recorder.Body.String())
	}

	var response service.JoinResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("the body is not a join response: %v", err)
	}
	if response.HopId != hop.Id || response.Name != "edge-a" || len(response.Secret) != chain.SecretLength {
		t.Fatalf("join response %+v", response)
	}
	if response.PollSeconds <= 0 || response.Document == nil || response.Document.Self.Name != "edge-a" {
		t.Fatalf("join response %+v", response)
	}

	stored := hopFromRegistry(t, registry, "edge-a")
	if stored.State != chain.StateJoined || stored.Host != "real.example.net" || stored.ObservedAddr != "198.51.100.44" {
		t.Fatalf("the registry kept %+v", stored)
	}

	// The secret handed out is the one the wave accepts from then on.
	if recorder := do(engine, documentRequest(response.Secret)); recorder.Code != http.StatusOK {
		t.Fatalf("the fresh secret did not open the document: %d", recorder.Code)
	}
}

func TestChainJoinFallsBackToTheCallersAddress(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, token, _, err := registry.Add(service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	request := joinRequest(t, map[string]any{"token": token})
	request.RemoteAddr = "203.0.113.9:50000"
	if recorder := do(engine, request); recorder.Code != http.StatusOK {
		t.Fatalf("POST /chain/v1/join: %d %q", recorder.Code, recorder.Body.String())
	}
	if stored := hopFromRegistry(t, registry, "edge-a"); stored.ObservedAddr != "203.0.113.9" {
		t.Fatalf("observedAddr is %q, want the peer address", stored.ObservedAddr)
	}
}

func TestChainJoinRefusalsAreBare404s(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, token, _, err := registry.Add(service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if recorder := do(engine, joinRequest(t, map[string]any{"token": token})); recorder.Code != http.StatusOK {
		t.Fatalf("the first join: %d", recorder.Code)
	}

	cases := map[string]any{
		"a spent token":   map[string]any{"token": token},
		"an alien token":  map[string]any{"token": chain.NewSecret()},
		"no token at all": map[string]any{},
	}
	for name, body := range cases {
		recorder := do(engine, joinRequest(t, body))
		if recorder.Code != http.StatusNotFound || recorder.Body.Len() != 0 {
			t.Fatalf("join with %s: %d %q, want a bare 404", name, recorder.Code, recorder.Body.String())
		}
	}

	// Anything that is not JSON at all is the same non-answer.
	request := httptest.NewRequest(http.MethodPost, "/chain/v1/join", strings.NewReader("not json"))
	if recorder := do(engine, request); recorder.Code != http.StatusNotFound || recorder.Body.Len() != 0 {
		t.Fatalf("join with a broken body: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestChainJoinRefusesALoop(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, token, _, err := registry.Add(service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	request := joinRequest(t, map[string]any{"token": token})
	request.Header.Set(chain.ForwardedHeader, "17")
	recorder := do(engine, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a join forwarded 17 times: %d %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "join_loop") {
		t.Fatalf("the refusal does not name join_loop: %q", recorder.Body.String())
	}
	if hopFromRegistry(t, registry, "edge-a").State != chain.StatePending {
		t.Fatal("a looping join still entered the hop")
	}
}

func TestChainJoinRefusesAnOversizedBody(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, token, _, err := registry.Add(service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	request := joinRequest(t, map[string]any{"token": token, "host": strings.Repeat("h", 8<<10)})
	recorder := do(engine, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an 8 KiB join body: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestChainStatusIsThePanelsView(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, secret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	enterHop(t, registry, service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	state, _ := registry.List()

	request := httptest.NewRequest(http.MethodGet, "/chain/v1/status", nil)
	if recorder := do(engine, request); recorder.Code != http.StatusNotFound || recorder.Body.Len() != 0 {
		t.Fatalf("GET /chain/v1/status without a bearer: %d %q", recorder.Code, recorder.Body.String())
	}

	request.Header.Set("Authorization", "Bearer "+secret)
	recorder := do(engine, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/status: %d %q", recorder.Code, recorder.Body.String())
	}
	var status struct {
		Version  int    `json:"version"`
		Role     string `json:"role"`
		Revision int64  `json:"revision"`
		Hops     []struct {
			Name         string `json:"name"`
			Role         string `json:"role"`
			State        string `json:"state"`
			LastRevision int64  `json:"lastRevision"`
			LastSeenAt   int64  `json:"lastSeenAt"`
		} `json:"hops"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatalf("the body is not a status: %v", err)
	}
	if status.Version != chain.DocumentVersion || status.Role != "panel" || status.Revision != state.Revision {
		t.Fatalf("status %+v", status)
	}
	if len(status.Hops) != 2 || status.Hops[0].Name != "inner-1" || status.Hops[1].Name != "edge-a" {
		t.Fatalf("status hops %+v", status.Hops)
	}
	if status.Hops[0].State != chain.StateJoined || status.Hops[0].Role != chain.RoleInner {
		t.Fatalf("status hop %+v", status.Hops[0])
	}
}

func TestChainStatusIsTruncatedLikeTheDocument(t *testing.T) {
	engine, registry := newChainRouter(t)
	_, edgeSecret := enterHop(t, registry, service.AddHopInput{Name: "edge-a", Host: "a.example.net", Role: chain.RoleEdge})
	enterHop(t, registry, service.AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge})

	request := httptest.NewRequest(http.MethodGet, "/chain/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+edgeSecret)
	recorder := do(engine, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/status: %d %q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "edge-b") {
		t.Fatalf("the status told a standby edge about its neighbour: %q", recorder.Body.String())
	}
}

func TestChainDocumentFallsBackToTheAddressTheHopDialled(t *testing.T) {
	engine, registry := newChainRouterWithPanelHost(t, "")
	_, secret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})

	request := documentRequest(secret)
	request.Host = "198.51.100.1:2096"
	recorder := do(engine, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/document with no chainPanelHost: %d %q", recorder.Code, recorder.Body.String())
	}
	var document chain.Document
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the body is not a document: %v", err)
	}
	if document.NextHop.Host != "198.51.100.1" {
		t.Fatalf("nextHop.host is %q, want the address the hop dialled without its port", document.NextHop.Host)
	}
}

func TestChainPanelHostOverridesTheAddressTheHopDialled(t *testing.T) {
	engine, registry := newChainRouterWithPanelHost(t, "panel.example.net")
	_, secret := enterHop(t, registry, service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})

	request := documentRequest(secret)
	request.Host = "198.51.100.1:2096"
	recorder := do(engine, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /chain/v1/document: %d %q", recorder.Code, recorder.Body.String())
	}
	var document chain.Document
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("the body is not a document: %v", err)
	}
	if document.NextHop.Host != "panel.example.net" {
		t.Fatalf("nextHop.host is %q, want the setting", document.NextHop.Host)
	}
}

func TestChainJoinDocumentTakesTheAddressTheBoxDialled(t *testing.T) {
	engine, registry := newChainRouterWithPanelHost(t, "")
	_, token, _, err := registry.Add(service.AddHopInput{Name: "inner-1", Host: "10.0.0.7", Role: chain.RoleInner})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	request := joinRequest(t, map[string]any{"token": token})
	request.Host = "198.51.100.1:2096"
	recorder := do(engine, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /chain/v1/join: %d %q", recorder.Code, recorder.Body.String())
	}
	var response service.JoinResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("the body is not a join response: %v", err)
	}
	if response.Document == nil || response.Document.NextHop.Host != "198.51.100.1" {
		t.Fatalf("the join document points at %+v", response.Document)
	}
}
