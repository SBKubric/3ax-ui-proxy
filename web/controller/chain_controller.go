package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// ChainController serves the chain registry to the panel's own editor under
// <webBasePath>panel/api/chain (docs/spec/proxy-chain.md §2.4, §7.3).
//
// It hangs off the /panel/api group and therefore inherits
// APIController.checkAPIAuth: a request without a session gets the same bare
// 404 as every other panel route, so no auth code lives here. Not to be
// confused with /chain/v1/* on the sub server (§3.3, §4.3), which the hops
// themselves speak with a hop secret.
//
// Every handler parses its parameters and calls one ChainService method. The
// service owns the invariants and the revision (§2.7, §3.4); the controller
// owns nothing but the envelope and the wording of a refusal.
type ChainController struct {
	chainService   service.ChainService
	settingService service.SettingService
	portsService   service.ChainPortsService
	health         ChainHealthProvider
}

// ChainHopHealth is one hop's health badge (§7.4): the name the editor keys its
// row by, and one of the mon-state values the Monitoring page already styles.
type ChainHopHealth struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// chainHealthUnknown is the grey badge: no monitoring data for this hop.
const chainHealthUnknown = "UNKNOWN"

// ChainHealthProvider folds the monitoring data into one state per hop. It is
// an interface with a stub behind it because the monitoring half is ticket #87
// (§6.3): the route, the shape and the badge ship now, and the day the probes
// report per hop only the implementation changes.
type ChainHealthProvider interface {
	HopHealth(hops []model.ChainHop) ([]ChainHopHealth, error)
}

// chainHealthStub answers UNKNOWN for every hop in the registry.
type chainHealthStub struct{}

func (chainHealthStub) HopHealth(hops []model.ChainHop) ([]ChainHopHealth, error) {
	health := make([]ChainHopHealth, 0, len(hops))
	for _, hop := range hops {
		health = append(health, ChainHopHealth{Name: hop.Name, State: chainHealthUnknown})
	}
	return health, nil
}

// NewChainController registers the registry routes of §2.4 on g.
func NewChainController(g *gin.RouterGroup) *ChainController {
	a := &ChainController{health: chainHealthStub{}}
	a.initRouter(g)
	return a
}

func (a *ChainController) initRouter(g *gin.RouterGroup) {
	g.GET("/list", a.list)
	g.POST("/add", a.add)
	g.POST("/update/:id", a.update)
	g.POST("/del/:id", a.del)
	g.POST("/setActive/:id", a.setActive)
	g.POST("/clearActive", a.clearActive)
	g.POST("/reissueToken/:id", a.reissueToken)
	g.GET("/hops/health", a.hopsHealth)
	g.GET("/ports", a.ports)
}

// chainListResponse is §2.4's GET list plus the editor's banner.
//
// portsProblem is not part of the registry: it is the last refusal of the port
// composition (§3.8), which the operator can only act on in the chain editor —
// the relayed ports stop moving until the collision is resolved, and nothing
// else in the panel would ever say so.
type chainListResponse struct {
	Revision     int64            `json:"revision"`
	ActiveEdge   string           `json:"activeEdge"`
	PollSeconds  int              `json:"pollSeconds"`
	Hops         []model.ChainHop `json:"hops"`
	PortsProblem *chainProblem    `json:"portsProblem"`
}

// chainProblem is a service refusal in the shape the page reads: the stable
// code to key wording off, and the detail naming the two sources that collide.
type chainProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// chainAddRequest is the body of POST add (§2.4). Position only means something
// for an inner front and is a pointer because "not given" (append at the end)
// and 0 (insert first, right behind the real server) are different requests.
type chainAddRequest struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Role      string `json:"role"`
	SubPort   int    `json:"subPort"`
	SubScheme string `json:"subScheme"`
	Position  *int   `json:"position"`
}

// chainUpdateRequest is the body of POST update/:id: every field is a pointer,
// so the editor can send the one control the operator touched without the rest
// of the form silently rewriting the hop.
//
// Role, position, state, isActive, nextHopId and id are fields of the hop the
// panel knows perfectly well, but update is not how they change: they move
// through add, del and setActive, which are the calls that keep the topology
// invariants. They are named here, pointers like the rest, purely so a request
// that touches one gets field_immutable naming it instead of the decoder's
// generic "unknown field" — which stays for a name update has never heard of.
type chainUpdateRequest struct {
	Name      *string `json:"name"`
	Host      *string `json:"host"`
	SubPort   *int    `json:"subPort"`
	SubScheme *string `json:"subScheme"`

	Role      *string `json:"role"`
	Position  *int    `json:"position"`
	State     *string `json:"state"`
	IsActive  *bool   `json:"isActive"`
	NextHopId *int    `json:"nextHopId"`
	Id        *int    `json:"id"`
}

// immutableField returns the JSON name of the first foreclosed field the
// request touches, or "" if it stuck to what update may change.
func (r chainUpdateRequest) immutableField() string {
	switch {
	case r.Role != nil:
		return "role"
	case r.Position != nil:
		return "position"
	case r.State != nil:
		return "state"
	case r.IsActive != nil:
		return "isActive"
	case r.NextHopId != nil:
		return "nextHopId"
	case r.Id != nil:
		return "id"
	default:
		return ""
	}
}

// chainDelRequest carries the force flag of §4.5: deleting the last active edge
// publishes the real server's address, so it is never the default.
type chainDelRequest struct {
	Force bool `json:"force"`
}

// chainMaxBodyBytes caps what a registry write may send. The largest body here
// is six short fields; 16 KiB is room to spare and still refuses a request that
// would otherwise be read into memory whole.
const chainMaxBodyBytes = 16 << 10

// readBody decodes a JSON body into dst the way the monitoring contract does
// (monitoring.go): the reader is capped, unknown fields are a refusal rather
// than something quietly dropped, and trailing data is refused too. A field the
// panel does not know is nearly always a client sending the wrong shape, and
// silently ignoring it hides the mistake until the hop behaves unexpectedly.
//
// Unlike the contract's version, the refusal travels in the panel's
// {success,msg,obj} envelope, and an empty body is not an error: del,
// setActive and reissueToken carry nothing, and the zero value is what they
// mean. The decoder's own message is kept — it names the offending field, and
// a malformed request is the caller's own text, not the panel's internals.
func (a *ChainController) readBody(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, chainMaxBodyBytes)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			pureJsonMsg(c, http.StatusRequestEntityTooLarge, false, "chain: body larger than 16 KiB")
			return false
		}
		pureJsonMsg(c, http.StatusBadRequest, false, "chain: invalid body: "+err.Error())
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		pureJsonMsg(c, http.StatusBadRequest, false, "chain: trailing data after the JSON body")
		return false
	}
	return true
}

// GET list — the whole registry, the revision the editor compares each hop's
// against, and the ports banner.
func (a *ChainController) list(c *gin.Context) {
	state, err := a.chainService.List()
	if err != nil {
		a.fail(c, err)
		return
	}
	response := &chainListResponse{
		Revision:    state.Revision,
		ActiveEdge:  state.ActiveEdge,
		PollSeconds: state.PollSeconds,
		Hops:        state.Hops,
	}
	if problem := a.portsService.LastProblem(); problem != nil {
		response.PortsProblem = &chainProblem{Code: problem.Code, Message: problem.Message}
	}
	jsonObj(c, response, nil)
}

// POST add — the join token travels back exactly once (§4.1); the panel keeps
// only its hash, so a lost token is reissued, never recovered.
func (a *ChainController) add(c *gin.Context) {
	var request chainAddRequest
	if !a.readBody(c, &request) {
		return
	}
	hop, token, expires, err := a.chainService.Add(service.AddHopInput{
		Name:      request.Name,
		Host:      request.Host,
		Role:      request.Role,
		SubPort:   request.SubPort,
		SubScheme: request.SubScheme,
		Position:  request.Position,
	})
	if err != nil {
		a.fail(c, err)
		return
	}
	jsonObj(c, gin.H{"hop": hop, "joinToken": token, "joinTokenExpires": expires}, nil)
}

// POST update/:id — name, host, sub port and sub scheme (§2.4). Role, position
// and activity are not here: they move through add, del and setActive, which
// are the calls that keep the topology invariants.
func (a *ChainController) update(c *gin.Context) {
	id, err := chainHopId(c)
	if err != nil {
		a.fail(c, err)
		return
	}
	var request chainUpdateRequest
	if !a.readBody(c, &request) {
		return
	}
	if field := request.immutableField(); field != "" {
		a.fail(c, &service.ChainError{
			Code:    service.CodeFieldImmutable,
			Message: strconv.Quote(field) + " cannot be changed through update; use add, del or setActive instead",
		})
		return
	}
	err = a.chainService.Update(id, service.UpdateHopInput{
		Name:      request.Name,
		Host:      request.Host,
		SubPort:   request.SubPort,
		SubScheme: request.SubScheme,
	})
	if err != nil {
		a.fail(c, err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.chain.saved"), nil)
}

// POST del/:id — the answer carries the hop and revision to wait for before
// the box is powered off (§4.5). Deleting the active edge is refused with
// active_edge_in_use, which the editor turns into its own wording.
func (a *ChainController) del(c *gin.Context) {
	id, err := chainHopId(c)
	if err != nil {
		a.fail(c, err)
		return
	}
	var request chainDelRequest
	// An empty body is a plain delete: force is the exception, never a default.
	if !a.readBody(c, &request) {
		return
	}
	result, err := a.chainService.Delete(id, request.Force)
	if err != nil {
		a.fail(c, err)
		return
	}
	jsonObj(c, result, nil)
}

// POST setActive/:id — the single registry write behind the editor's "Make
// active" button and the bot's /proxy <name>. Nothing on any box changes
// (§4.7): only the host the panel publishes.
func (a *ChainController) setActive(c *gin.Context) {
	id, err := chainHopId(c)
	if err != nil {
		a.fail(c, err)
		return
	}
	if err := a.chainService.SetActive(id); err != nil {
		a.fail(c, err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.chain.saved"), nil)
}

// POST clearActive — the panel's own "/proxy off": no edge stays active and
// the panel falls back to publishing the real server's address. It goes
// through SettingService.DisableProxyOverride, the exact call the bot's
// "/proxy off" makes, so the editor's button and the bot command can never
// disagree about what turning the override off means.
func (a *ChainController) clearActive(c *gin.Context) {
	if err := a.settingService.DisableProxyOverride(); err != nil {
		a.fail(c, err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.chain.saved"), nil)
}

// POST reissueToken/:id — a new join token, the old one dead at once. The hop
// goes back to pending; if it was the active edge it stays active, because the
// alternative is publishing the real server's address for the re-join window
// (§2.3, §4.4).
func (a *ChainController) reissueToken(c *gin.Context) {
	id, err := chainHopId(c)
	if err != nil {
		a.fail(c, err)
		return
	}
	token, expires, err := a.chainService.ReissueToken(id)
	if err != nil {
		a.fail(c, err)
		return
	}
	jsonObj(c, gin.H{"joinToken": token, "joinTokenExpires": expires}, nil)
}

// GET hops/health — one badge per hop (§6.4, §7.4).
func (a *ChainController) hopsHealth(c *gin.Context) {
	state, err := a.chainService.List()
	if err != nil {
		a.fail(c, err)
		return
	}
	health, err := a.health.HopHealth(state.Hops)
	jsonObj(c, health, err)
}

// GET ports — the relayed-port list of the chain document (§3.8), the same one
// `x-ui chain ports` prints. The editor shows it instead of the relay manifest
// the fork used to build, which the chain replaced (§5.9).
func (a *ChainController) ports(c *gin.Context) {
	ports, err := a.portsService.Ports()
	if err != nil {
		a.fail(c, err)
		return
	}
	if ports == nil {
		ports = []chain.Port{}
	}
	jsonObj(c, ports, nil)
}

// chainHopId reads the :id of a per-hop route. A non-numeric id is the same
// answer as an id that is not in the registry: the editor never builds one by
// hand, so this is a malformed request rather than something to explain.
//
// It is a real ChainError rather than a plain one so that fail() can key the
// wording off its code, like every other refusal.
func chainHopId(c *gin.Context) (int, error) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		return 0, &service.ChainError{
			Code:    service.CodeUnknownHop,
			Message: "hop id " + strconv.Quote(c.Param("id")) + " is not a hop id",
		}
	}
	return id, nil
}

// fail answers a refusal in the panel envelope: HTTP stays 200 and success goes
// false, as everywhere else in /panel/api.
//
// A registry refusal carries a stable code (§2.4), and the code is what the
// wording hangs off: pages.settings.chain.errors.<code> in every language, with
// the service's own English detail appended by jsonMsgObj — that detail is
// written for the operator and names the hop or the rule that was broken.
//
// An error without a code is not a refusal but a fault: a database that will
// not open, a file the panel cannot read. Its text describes the panel's
// insides and belongs in the log, not in a page anyone with a session can read,
// so the browser gets one generic sentence and the log gets the error.
func (a *ChainController) fail(c *gin.Context, err error) {
	code := service.ChainErrorCode(err)
	if code == "" {
		logger.Warning("chain:", c.Request.Method, c.Request.URL.Path, err)
		message := I18nWeb(c, "pages.settings.chain.errors.internal")
		if message == "" {
			// A panel whose bundle failed to load still has to say something.
			message = "chain: the panel could not complete this request"
		}
		pureJsonMsg(c, http.StatusOK, false, message)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.chain.errors."+code), err)
}
