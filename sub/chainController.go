package sub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database/model"
	"github.com/coinman-dev/3ax-ui/v2/logger"
	"github.com/coinman-dev/3ax-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// The panel's end of the chain protocol (docs/spec/proxy-chain.md §3.3): the
// same three handles every hop serves on its own sub port, so a box does not
// need to know whether its next hop is another front or the panel itself.
//
// Everything that is not a success is a bare 404 without a body, as
// checkAPIAuth and checkMonAuth answer: a box that could tell "wrong secret"
// from "no such hop" would be an oracle, and a scanner must not learn that the
// endpoint is here at all. The two exceptions are the ones the spec spells
// out, because a box has to act on them: a forwarding loop and a body too
// large to be a join.
const (
	chainPrefix = "/chain/v1"

	// A join body is a token and an address. Anything larger is not one.
	chainJoinMaxBodyBytes = 4 << 10

	// X-Chain-Outer is base64 of a JSON array of acknowledgements; a hop cuts
	// its list at 8 KiB and logs (§3.3), so that is what we are willing to
	// read before ignoring the header.
	chainOuterMaxBytes = 8 << 10

	// A join may be forwarded inward at most this many times before it is
	// treated as a loop (§4.3).
	chainMaxForwarded = 16

	// chainPanelRole is what the panel calls itself in its status: it is not a
	// hop, it is the thing at the end of the chain.
	chainPanelRole = "panel"

	// chainDrainSweepInterval is how often a departure is checked for its
	// deadline (§4.5.4). Confirmations already trigger a sweep where they
	// land; the ticker is for the chain where nobody polls any more, so that a
	// row cannot hang about forever.
	chainDrainSweepInterval = 60 * time.Second
)

// ChainController serves the wave and the join on the panel's sub server.
type ChainController struct {
	chainService    service.ChainService
	documentService service.ChainDocumentService
	joinService     service.ChainJoinService
	waveService     service.ChainWaveService
}

// NewChainController registers the chain endpoints on the sub server's root
// group. The prefix is fixed and unconfigurable: a hop has to find it before
// it has any document to learn it from.
func NewChainController(g *gin.RouterGroup) *ChainController {
	a := &ChainController{}
	a.initRouter(g)
	return a
}

func (a *ChainController) initRouter(g *gin.RouterGroup) {
	chainGroup := g.Group(chainPrefix)
	chainGroup.GET("/document", a.document)
	chainGroup.POST("/join", a.join)
	chainGroup.GET("/status", a.status)
}

// chainRefusal is the body of the two refusals that are not bare 404s.
type chainRefusal struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// chainPanelStatus is the panel's answer to GET /chain/v1/status (§3.6). The
// panel is not a hop: it has no relay, no next hop and no staleness, so its
// status is the registry's revision and the hops the caller may know about.
type chainPanelStatus struct {
	Version  int                   `json:"version"`
	Role     string                `json:"role"`
	Revision int64                 `json:"revision"`
	Hops     []chainPanelStatusHop `json:"hops"`
}

// chainPanelStatusHop is one hop as the registry sees it — no host, no secret
// hash: whoever asks has those in its document already if it is allowed them.
type chainPanelStatusHop struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	State        string `json:"state"`
	LastRevision int64  `json:"lastRevision"`
	LastSeenAt   int64  `json:"lastSeenAt"`
}

// document serves the truncated document of the calling hop (§3.3).
func (a *ChainController) document(c *gin.Context) {
	hop, ok := a.authenticate(c)
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	document, err := a.documentService.BuildWithPanelHost(hop.Name, requestHost(c))
	if err != nil {
		// A chain that cannot be described is the panel's problem, not the
		// box's, and it says so here rather than in a body the box would have
		// to interpret.
		logger.Warning("chain: cannot build the document for", hop.Name, err)
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	a.recordWave(c, hop.Name)

	etag := service.ETag(document.Revision)
	c.Header("ETag", etag)
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.JSON(http.StatusOK, document)
}

// join lets a box into the chain (§4.3). It is the one unauthenticated handle
// of the protocol: the token in the body is the credential.
func (a *ChainController) join(c *gin.Context) {
	if forwarded := headerInt(c.GetHeader(chain.ForwardedHeader)); forwarded > chainMaxForwarded {
		c.AbortWithStatusJSON(http.StatusBadRequest, chainRefusal{
			Error:   "join_loop",
			Message: "the join was forwarded " + strconv.FormatInt(forwarded, 10) + " times; the chain is looping",
		})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, chainJoinMaxBodyBytes)
	var body struct {
		Token     string `json:"token"`
		Host      string `json:"host"`
		SubPort   int    `json:"subPort"`
		SubScheme string `json:"subScheme"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, chainRefusal{
				Error:   "body_too_large",
				Message: "a join body is a token and an address",
			})
			return
		}
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// The direct neighbour stamps where the join really came from; when the
	// panel is the direct neighbour, that is the peer we are talking to. The
	// header comes from another box, so it is checked before it is believed —
	// and a header that is not an address is worth less than the peer we can
	// see for ourselves.
	observed := strings.TrimSpace(c.GetHeader(chain.ObservedHeader))
	if !chain.ObservedAddrValid(observed) {
		observed = strings.TrimSpace(c.ClientIP())
	}
	if !chain.ObservedAddrValid(observed) {
		observed = ""
	}

	response, err := a.joinService.Join(service.JoinRequest{
		Token:             body.Token,
		Host:              body.Host,
		SubPort:           body.SubPort,
		SubScheme:         body.SubScheme,
		ObservedAddr:      observed,
		FallbackPanelHost: requestHost(c),
	})
	if err != nil {
		if !errors.Is(err, service.ErrJoinRejected) {
			logger.Warning("chain: join failed:", err)
		}
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	logger.Infof("chain: hop %q joined from %s", response.Name, observed)
	c.JSON(http.StatusOK, response)
}

// status is the panel's health view for the hop that polls it (§3.6).
//
// The hop list is truncated exactly as the caller's document is: the status
// must not become a second, laxer way to read the chain, which for a standby
// edge would mean learning that its neighbour exists at all (§3.2).
func (a *ChainController) status(c *gin.Context) {
	hop, ok := a.authenticate(c)
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	document, err := a.documentService.BuildWithPanelHost(hop.Name, requestHost(c))
	if err != nil {
		logger.Warning("chain: cannot build the status for", hop.Name, err)
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	state, err := a.chainService.List()
	if err != nil {
		logger.Warning("chain: cannot read the registry:", err)
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	freshness := make(map[string]chainPanelStatusHop, len(state.Hops))
	for _, registered := range state.Hops {
		freshness[registered.Name] = chainPanelStatusHop{
			Name:         registered.Name,
			Role:         registered.Role,
			State:        registered.State,
			LastRevision: registered.LastRevision,
			LastSeenAt:   registered.LastSeenAt,
		}
	}

	status := chainPanelStatus{
		Version:  chain.DocumentVersion,
		Role:     chainPanelRole,
		Revision: document.Revision,
		Hops:     make([]chainPanelStatusHop, 0, len(document.Hops)),
	}
	for _, visible := range document.Hops {
		status.Hops = append(status.Hops, freshness[visible.Name])
	}
	c.JSON(http.StatusOK, status)
}

// authenticate resolves the bearer to the hop that presented it, or to
// nothing at all. Only a hop whose next hop is the panel gets this far
// (ChainWaveService.AuthenticateHop).
func (a *ChainController) authenticate(c *gin.Context) (*model.ChainHop, bool) {
	presented, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
	if !ok {
		return nil, false
	}
	return a.waveService.AuthenticateHop(strings.TrimSpace(presented))
}

// recordWave writes down what the poll told us about the chain's freshness. A
// garbled header costs the caller nothing: the document is what it came for,
// and the wave is a report, not a request.
func (a *ChainController) recordWave(c *gin.Context, hopName string) {
	seen := headerInt(c.GetHeader(chain.SeenHeader))
	outer := parseOuterAcks(c.GetHeader(chain.OuterHeader))
	if err := a.waveService.RecordSeen(hopName, seen, outer); err != nil {
		logger.Warning("chain: cannot record the poll of", hopName, err)
	}
	// Where the caller and the hops outward of it now answer (#140). A box
	// older than the report sends none, and its registry entry stays as it
	// is.
	var front *chain.FrontReport
	if report, ok := chain.ParseFrontReport(c.GetHeader(chain.FrontHeader)); ok {
		front = &report
	}
	if err := a.waveService.RecordFront(hopName, front, outer); err != nil {
		logger.Warning("chain: cannot record the front report of", hopName, err)
	}
}

// parseOuterAcks decodes X-Chain-Outer. Anything that does not decode, does
// not parse or is too long to be a list of acknowledgements is ignored: the
// panel hears every hop of the chain through this header, and one box with a
// broken encoder must not be able to fail everyone's poll.
func parseOuterAcks(header string) []chain.OuterAck {
	header = strings.TrimSpace(header)
	if header == "" || len(header) > chainOuterMaxBytes {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		if decoded, err = base64.RawStdEncoding.DecodeString(header); err != nil {
			return nil
		}
	}
	var acks []chain.OuterAck
	if err := json.Unmarshal(decoded, &acks); err != nil {
		return nil
	}
	return acks
}

// headerInt reads a header that should hold a number, and returns 0 when it
// does not.
func headerInt(value string) int64 {
	number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return number
}

// requestHost is the address the caller reached the panel at, without the
// port: the document's nextHop.host when the owner has not stated one.
func requestHost(c *gin.Context) string {
	host := strings.TrimSpace(c.Request.Host)
	if host == "" {
		return ""
	}
	if stripped, _, err := net.SplitHostPort(host); err == nil {
		return stripped
	}
	return host
}

// startChainDrainSweep runs SweepDraining once a minute until ctx is done
// (§4.5.4). It rides with the sub server because that is where the chain's own
// traffic arrives: a panel whose subscription server is off serves no
// documents, so no departure it could finish is under way either.
func startChainDrainSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(chainDrainSweepInterval)
		defer ticker.Stop()
		chainService := &service.ChainService{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := chainService.SweepDraining(); err != nil {
					logger.Warning("chain: sweeping draining hops:", err)
				}
			}
		}
	}()
}

// warnChainNeedsSubServer is the one thing the panel can do about a chain
// whose only door is shut: the endpoints of §3.3 live on the sub server, so a
// registry with hops in it and subEnable off is a chain that will never hear
// another revision. It is a warning rather than a refusal — the sub server is
// the owner's switch, not ours.
func warnChainNeedsSubServer() {
	state, err := (&service.ChainService{}).List()
	if err != nil || len(state.Hops) == 0 {
		return
	}
	logger.Warningf("chain: the registry has %d hop(s) but the subscription server is off; "+
		"/chain/v1/* is unreachable and no hop can poll or join until subEnable is on", len(state.Hops))
}
