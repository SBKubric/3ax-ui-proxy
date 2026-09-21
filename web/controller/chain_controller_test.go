package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/web/service"
	"github.com/coinman-dev/3ax-ui/v2/web/session"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

// newChainRouter builds the /panel/api group the way APIController.initRouter
// does — guarded by checkAPIAuth, with the chain group hanging off it — over a
// fresh temporary database. NewAPIController would also start the server
// controller's background task, which a route test has no use for.
func newChainRouter(t *testing.T) *gin.Engine {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	gin.SetMode(gin.TestMode)

	r := gin.New()
	store := cookie.NewStore([]byte("chain-ui-test-secret"))
	r.Use(sessions.Sessions("3ax-ui", store))
	r.GET("/test-login", func(c *gin.Context) {
		if err := session.SetLoginUser(c, &model.User{Id: 1, Username: "admin"}); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusOK)
	})

	a := &APIController{}
	api := r.Group("/panel/api")
	api.Use(a.checkAPIAuth)
	NewChainController(api.Group("/chain"))
	return r
}

// chainPost sends a JSON body to a chain route.
func chainPost(r *gin.Engine, path, cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// chainAdd creates a hop through the API and returns the envelope's obj.
func chainAdd(t *testing.T, r *gin.Engine, cookie, body string) struct {
	Hop              model.ChainHop `json:"hop"`
	JoinToken        string         `json:"joinToken"`
	JoinTokenExpires int64          `json:"joinTokenExpires"`
} {
	t.Helper()
	var added struct {
		Hop              model.ChainHop `json:"hop"`
		JoinToken        string         `json:"joinToken"`
		JoinTokenExpires int64          `json:"joinTokenExpires"`
	}
	env := monUIDecode(t, chainPost(r, "/panel/api/chain/add", cookie, body))
	if !env.Success {
		t.Fatalf("add %s: %s", body, env.Msg)
	}
	if err := json.Unmarshal(env.Obj, &added); err != nil {
		t.Fatalf("add obj: %v (%s)", err, env.Obj)
	}
	return added
}

// TestChainRoutesNeedASession: the registry is as invisible without a panel
// session as the rest of /panel/api — a bare 404, never a 401 that would admit
// the route exists.
func TestChainRoutesNeedASession(t *testing.T) {
	r := newChainRouter(t)
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/panel/api/chain/list"},
		{http.MethodGet, "/panel/api/chain/hops/health"},
		{http.MethodGet, "/panel/api/chain/ports"},
		{http.MethodPost, "/panel/api/chain/add"},
		{http.MethodPost, "/panel/api/chain/update/1"},
		{http.MethodPost, "/panel/api/chain/del/1"},
		{http.MethodPost, "/panel/api/chain/setActive/1"},
		{http.MethodPost, "/panel/api/chain/clearActive"},
		{http.MethodPost, "/panel/api/chain/reissueToken/1"},
	} {
		var w *httptest.ResponseRecorder
		if call.method == http.MethodGet {
			w = monUIGet(r, call.path, "")
		} else {
			w = chainPost(r, call.path, "", "{}")
		}
		if w.Code != http.StatusNotFound || w.Body.Len() != 0 {
			t.Errorf("%s %s without a session: status %d body %q, want a bare 404",
				call.method, call.path, w.Code, w.Body.String())
		}
	}
}

// TestChainAddHandsTheTokenOutOnceAndListHidesIt: POST add is the only moment
// the join token exists in the clear (§4.1), and GET list — the call the editor
// repeats — carries neither it nor any hash.
func TestChainAddHandsTheTokenOutOnceAndListHidesIt(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)

	added := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)
	if len(added.JoinToken) != 32 {
		t.Fatalf("joinToken %q is %d characters, want 32", added.JoinToken, len(added.JoinToken))
	}
	if added.Hop.State != "pending" || added.Hop.SubPort != 2096 || added.JoinTokenExpires == 0 {
		t.Errorf("hop = %+v expires %d, want a pending hop on 2096 with an expiry", added.Hop, added.JoinTokenExpires)
	}

	env := monUIDecode(t, monUIGet(r, "/panel/api/chain/list", cookie))
	if !env.Success {
		t.Fatalf("list: %s", env.Msg)
	}
	body := string(env.Obj)
	for _, secret := range []string{added.JoinToken, "joinTokenHash", "secretHash", "JoinTokenHash", "SecretHash"} {
		if strings.Contains(body, secret) {
			t.Errorf("list carries %q: %s", secret, body)
		}
	}

	var list struct {
		Revision     int64            `json:"revision"`
		ActiveEdge   string           `json:"activeEdge"`
		PollSeconds  int              `json:"pollSeconds"`
		Hops         []model.ChainHop `json:"hops"`
		PortsProblem *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"portsProblem"`
	}
	if err := json.Unmarshal(env.Obj, &list); err != nil {
		t.Fatalf("list obj: %v (%s)", err, env.Obj)
	}
	if len(list.Hops) != 1 || list.Hops[0].Name != "edge-a" {
		t.Fatalf("hops = %+v, want the one edge", list.Hops)
	}
	if list.PollSeconds != 30 {
		t.Errorf("pollSeconds = %d, want the default 30", list.PollSeconds)
	}
	// Nothing has composed a port list yet, so the editor's banner stays off.
	if list.PortsProblem != nil {
		t.Errorf("portsProblem = %+v, want null on a clean panel", list.PortsProblem)
	}
}

// TestChainAddRefusesABadRequest: validation refusals travel as success:false
// with the service's code in the message, and the status stays 200 — the panel
// envelope has no other way to say no.
func TestChainAddRefusesABadRequest(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)

	for _, call := range []struct{ body, code string }{
		{`{"name":"edge-a","host":"b.example.net","role":"edge"}`, service.CodeNameTaken},
		{`{"name":"Edge B","host":"b.example.net","role":"edge"}`, service.CodeInvalidName},
		{`{"name":"edge-b","host":"","role":"edge"}`, service.CodeInvalidHost},
		{`{"name":"edge-b","host":"b.example.net","role":"middle"}`, service.CodeInvalidRole},
		{`{"name":"inner-9","host":"i.example.net","role":"inner","position":7}`, service.CodeInvalidPosition},
	} {
		env := monUIDecode(t, chainPost(r, "/panel/api/chain/add", cookie, call.body))
		if env.Success {
			t.Errorf("add %s: success=true, want a refusal", call.body)
			continue
		}
		if !strings.Contains(env.Msg, call.code) {
			t.Errorf("add %s: msg %q does not name %q", call.body, env.Msg, call.code)
		}
	}
}

// TestChainDeleteOfTheActiveEdgeIsRefused: the one refusal the editor turns
// into its own wording (chain.deleteActiveRefused) — deleting the active edge
// while another edge could take over would publish the real server's address.
func TestChainDeleteOfTheActiveEdgeIsRefused(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	active := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)
	standby := chainAdd(t, r, cookie, `{"name":"edge-b","host":"b.example.net","role":"edge"}`)

	// A pending hop cannot become active (invariant 1): only a box that has
	// really entered the chain may carry the override.
	env := monUIDecode(t, chainPost(r, "/panel/api/chain/setActive/"+strconv.Itoa(active.Hop.Id), cookie, "{}"))
	if env.Success {
		t.Fatalf("setActive on a pending hop: success=true, want a refusal")
	}
	if !strings.Contains(env.Msg, service.CodeHopNotJoined) {
		t.Errorf("setActive msg %q does not name %q", env.Msg, service.CodeHopNotJoined)
	}

	// Joined through the service, the way the join flow does it, and then made
	// active through the API.
	if err := (&service.ChainService{}).MarkJoined(active.Hop.Id, "hash", "198.51.100.7"); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}
	if env := monUIDecode(t, chainPost(r, "/panel/api/chain/setActive/"+strconv.Itoa(active.Hop.Id), cookie, "{}")); !env.Success {
		t.Fatalf("setActive: %s", env.Msg)
	}
	if err := (&service.ChainService{}).MarkJoined(standby.Hop.Id, "hash-b", ""); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}

	env = monUIDecode(t, chainPost(r, "/panel/api/chain/del/"+strconv.Itoa(active.Hop.Id), cookie, `{"force":false}`))
	if env.Success {
		t.Fatalf("deleting the active edge: success=true, want a refusal")
	}
	if !strings.Contains(env.Msg, service.CodeActiveEdgeInUse) {
		t.Errorf("delete msg %q does not name %q", env.Msg, service.CodeActiveEdgeInUse)
	}

	// The standby is not active, so it goes, and the answer says which hop has
	// to confirm which revision before its box may be powered off (§4.5).
	env = monUIDecode(t, chainPost(r, "/panel/api/chain/del/"+strconv.Itoa(standby.Hop.Id), cookie, `{"force":false}`))
	if !env.Success {
		t.Fatalf("deleting a standby edge: %s", env.Msg)
	}
	var result service.DeleteResult
	if err := json.Unmarshal(env.Obj, &result); err != nil {
		t.Fatalf("delete obj: %v (%s)", err, env.Obj)
	}
	if result.SafeToPowerOffWhen.Revision == 0 {
		t.Errorf("delete = %+v, want the revision to wait for", result)
	}
}

// TestChainReissueTokenGivesANewOne: the old token is dead from this moment,
// and the hop is pending again whatever it was before (§4.1).
func TestChainReissueTokenGivesANewOne(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	added := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)
	if err := (&service.ChainService{}).MarkJoined(added.Hop.Id, "hash", ""); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}

	env := monUIDecode(t, chainPost(r, "/panel/api/chain/reissueToken/"+strconv.Itoa(added.Hop.Id), cookie, "{}"))
	if !env.Success {
		t.Fatalf("reissueToken: %s", env.Msg)
	}
	var reissued struct {
		JoinToken        string `json:"joinToken"`
		JoinTokenExpires int64  `json:"joinTokenExpires"`
	}
	if err := json.Unmarshal(env.Obj, &reissued); err != nil {
		t.Fatalf("reissue obj: %v (%s)", err, env.Obj)
	}
	if len(reissued.JoinToken) != 32 || reissued.JoinToken == added.JoinToken {
		t.Fatalf("reissued token %q, want a fresh 32-character one", reissued.JoinToken)
	}

	state, err := (&service.ChainService{}).List()
	if err != nil {
		t.Fatal(err)
	}
	if state.Hops[0].State != "pending" {
		t.Errorf("hop state = %q after a reissue, want pending", state.Hops[0].State)
	}
}

// TestChainUpdateChangesTheHop: the four fields §2.4 allows, and an unknown id
// refused by code rather than by status.
func TestChainUpdateChangesTheHop(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	added := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)

	env := monUIDecode(t, chainPost(r, "/panel/api/chain/update/"+strconv.Itoa(added.Hop.Id), cookie,
		`{"host":"a2.example.net","subPort":8443,"subScheme":"http"}`))
	if !env.Success {
		t.Fatalf("update: %s", env.Msg)
	}
	state, err := (&service.ChainService{}).List()
	if err != nil {
		t.Fatal(err)
	}
	hop := state.Hops[0]
	if hop.Host != "a2.example.net" || hop.SubPort != 8443 || hop.SubScheme != "http" {
		t.Errorf("hop = %+v, want the updated host, port and scheme", hop)
	}

	env = monUIDecode(t, chainPost(r, "/panel/api/chain/update/4242", cookie, `{"host":"x.example.net"}`))
	if env.Success || !strings.Contains(env.Msg, service.CodeUnknownHop) {
		t.Errorf("update of an unknown hop: success=%v msg %q", env.Success, env.Msg)
	}
	env = monUIDecode(t, chainPost(r, "/panel/api/chain/update/not-a-number", cookie, `{"host":"x.example.net"}`))
	if env.Success {
		t.Errorf("update with a non-numeric id: success=true, want a refusal")
	}
}

// TestChainHopsHealthIsAStub: #87 fills this in from monitoring; until then
// every hop answers UNKNOWN, which is exactly what the badge shows.
func TestChainHopsHealthIsAStub(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)
	chainAdd(t, r, cookie, `{"name":"inner-1","host":"i.example.net","role":"inner"}`)

	env := monUIDecode(t, monUIGet(r, "/panel/api/chain/hops/health", cookie))
	if !env.Success {
		t.Fatalf("hops/health: %s", env.Msg)
	}
	var health []ChainHopHealth
	if err := json.Unmarshal(env.Obj, &health); err != nil {
		t.Fatalf("health obj: %v (%s)", err, env.Obj)
	}
	if len(health) != 2 {
		t.Fatalf("health = %+v, want one entry per hop", health)
	}
	for _, hop := range health {
		if hop.State != chainHealthUnknown || hop.Name == "" {
			t.Errorf("health entry = %+v, want a named hop in state UNKNOWN", hop)
		}
	}
}

// TestChainRefusesAMalformedBody: the registry writes read their JSON the way
// the monitoring contract does — a capped reader and no unknown fields — so a
// client sending the wrong shape is told, rather than having the field it
// misspelled silently ignored.
func TestChainRefusesAMalformedBody(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)

	// An unknown field: 400, and the answer names it.
	w := chainPost(r, "/panel/api/chain/add", cookie,
		`{"name":"edge-a","host":"a.example.net","role":"edge","isActive":true}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("add with an unknown field: status %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "isActive") {
		t.Errorf("add with an unknown field: %s, want the field named", w.Body.String())
	}

	// A body past the cap: 413, and nothing of it is read into the registry.
	big := `{"name":"edge-a","host":"` + strings.Repeat("h", chainMaxBodyBytes) + `","role":"edge"}`
	w = chainPost(r, "/panel/api/chain/add", cookie, big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("add with an oversized body: status %d, want 413 (%s)", w.Code, w.Body.String())
	}

	// Both refusals keep the panel's envelope, so the page reads them the same
	// way as any other no.
	for _, body := range []string{
		`{"name":"edge-a","host":"a.example.net","role":"edge","isActive":true}`,
		big,
		`{"host":"a.example.net"} trailing`,
	} {
		w := chainPost(r, "/panel/api/chain/update/1", cookie, body)
		var env monUIEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("envelope: %v (%s)", err, w.Body.String())
		}
		if env.Success || env.Msg == "" {
			t.Errorf("malformed body answered %+v, want success:false with a reason", env)
		}
	}

	// Nothing was created by any of it.
	state, err := (&service.ChainService{}).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Hops) != 0 {
		t.Errorf("hops = %+v, want a registry no malformed request could touch", state.Hops)
	}
}

// TestChainDelTakesAnEmptyBody: del, setActive and reissueToken carry nothing,
// and an empty body means the zero value rather than a refusal.
func TestChainDelTakesAnEmptyBody(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	added := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)

	env := monUIDecode(t, chainPost(r, "/panel/api/chain/del/"+strconv.Itoa(added.Hop.Id), cookie, ""))
	if !env.Success {
		t.Fatalf("del with an empty body: %s", env.Msg)
	}
}

// TestChainInternalFailureSaysNothingAboutTheInsides: a fault is not a refusal.
// A registry refusal explains itself — it names the hop and the rule — but an
// error with no code describes the panel's own machinery, and that text belongs
// in the log rather than in an answer.
func TestChainInternalFailureSaysNothingAboutTheInsides(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	// The one fault a test can stage without a fake: the database is gone.
	database.CloseDB()

	env := monUIDecode(t, monUIGet(r, "/panel/api/chain/list", cookie))
	if env.Success {
		t.Fatalf("list on a closed database: success=true")
	}
	if env.Msg == "" {
		t.Errorf("an internal failure answered with no message at all")
	}
	for _, leak := range []string{"sql:", "database is closed", "gorm", "goroutine"} {
		if strings.Contains(strings.ToLower(env.Msg), strings.ToLower(leak)) {
			t.Errorf("msg %q leaks %q from the panel's insides", env.Msg, leak)
		}
	}
}

// TestChainClearActiveDisablesTheOverride: the editor's "Turn override off"
// button, and the panel's own "/proxy off" — clearActive goes through
// SettingService.DisableProxyOverride, the exact call the bot's "/proxy off"
// makes (tgbot.go), so the two can never disagree about what turning the
// override off means. It disables both halves: the registry's active edge and
// the legacy proxyOverrideEnable flag DisableProxyOverride also turns off.
func TestChainClearActiveDisablesTheOverride(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	added := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)
	if err := (&service.ChainService{}).MarkJoined(added.Hop.Id, "hash", ""); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}
	if env := monUIDecode(t, chainPost(r, "/panel/api/chain/setActive/"+strconv.Itoa(added.Hop.Id), cookie, "{}")); !env.Success {
		t.Fatalf("setActive: %s", env.Msg)
	}
	if _, ok := (&service.ChainService{}).ActiveEdgeHost(); !ok {
		t.Fatal("setup: the edge should be active before clearActive is asked to turn it off")
	}

	env := monUIDecode(t, chainPost(r, "/panel/api/chain/clearActive", cookie, "{}"))
	if !env.Success {
		t.Fatalf("clearActive: %s", env.Msg)
	}
	if _, ok := (&service.ChainService{}).ActiveEdgeHost(); ok {
		t.Error("clearActive left an edge active; want no active edge, the panel publishing the real server")
	}
	state, err := (&service.ChainService{}).List()
	if err != nil {
		t.Fatal(err)
	}
	if state.ActiveEdge != "" {
		t.Errorf("activeEdge = %q after clearActive, want none", state.ActiveEdge)
	}

	// Calling it again with nothing active is not an error: it is exactly
	// what "/proxy off" already means on a panel with no override.
	env = monUIDecode(t, chainPost(r, "/panel/api/chain/clearActive", cookie, "{}"))
	if !env.Success {
		t.Fatalf("clearActive with nothing active: %s", env.Msg)
	}
}

// TestChainAddReturnsTheComputedNextHopId: add's own answer must carry the
// same next_hop_id list already computes, not the pre-reconcile nil the row
// held the instant it was created.
func TestChainAddReturnsTheComputedNextHopId(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)

	first := chainAdd(t, r, cookie, `{"name":"inner-1","host":"i1.example.net","role":"inner"}`)
	if first.Hop.NextHopId != nil {
		t.Fatalf("first inner's nextHopId = %v, want nil (chained onto the real server)", first.Hop.NextHopId)
	}

	second := chainAdd(t, r, cookie, `{"name":"inner-2","host":"i2.example.net","role":"inner","position":1}`)
	if second.Hop.NextHopId == nil || *second.Hop.NextHopId != first.Hop.Id {
		t.Fatalf("second inner's nextHopId in the add response = %v, want %d (inner-1)",
			second.Hop.NextHopId, first.Hop.Id)
	}

	// list, which already computes it correctly, must agree.
	state, err := (&service.ChainService{}).List()
	if err != nil {
		t.Fatal(err)
	}
	for _, hop := range state.Hops {
		if hop.Id == second.Hop.Id && (hop.NextHopId == nil || *hop.NextHopId != first.Hop.Id) {
			t.Errorf("list's nextHopId = %v, want %d to match add's own answer", hop.NextHopId, first.Hop.Id)
		}
	}
}

// TestChainUpdateRefusesAnImmutableField: role (and the other fields that move
// through add, del and setActive) get a field_immutable refusal that names the
// field, distinct from the "unknown field" a name update has never heard of at
// all.
func TestChainUpdateRefusesAnImmutableField(t *testing.T) {
	r := newChainRouter(t)
	cookie := monUILogin(t, r)
	added := chainAdd(t, r, cookie, `{"name":"edge-a","host":"a.example.net","role":"edge"}`)

	for _, body := range []string{
		`{"role":"inner"}`,
		`{"position":0}`,
		`{"state":"joined"}`,
		`{"isActive":true}`,
		`{"nextHopId":1}`,
		`{"id":999}`,
	} {
		env := monUIDecode(t, chainPost(r, "/panel/api/chain/update/"+strconv.Itoa(added.Hop.Id), cookie, body))
		if env.Success {
			t.Errorf("update %s: success=true, want a field_immutable refusal", body)
			continue
		}
		if !strings.Contains(env.Msg, service.CodeFieldImmutable) {
			t.Errorf("update %s: msg %q does not name %q", body, env.Msg, service.CodeFieldImmutable)
		}
	}

	// A name update has never heard of at all is still the decoder's own
	// refusal, not field_immutable.
	w := chainPost(r, "/panel/api/chain/update/"+strconv.Itoa(added.Hop.Id), cookie, `{"bogus":true}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("update with a truly unknown field: status %d, want 400 (%s)", w.Code, w.Body.String())
	}

	// None of the refused requests touched the hop.
	state, err := (&service.ChainService{}).List()
	if err != nil {
		t.Fatal(err)
	}
	if state.Hops[0].Role != "edge" {
		t.Errorf("role = %q after refused updates, want unchanged", state.Hops[0].Role)
	}
}
