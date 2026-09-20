package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/chain"
	"github.com/coinman-dev/3ax-ui/v2/database"
	"github.com/coinman-dev/3ax-ui/v2/database/model"

	"gorm.io/gorm"
)

// ErrJoinRejected is the only refusal a join ever produces (§4.3). The box on
// the other end is answered with a bare 404 whatever the reason — an unknown
// token, an expired one, a token already spent, a hop that has entered
// already — because a box that could tell those apart would be an oracle for
// probing the registry, and a scanner would learn that the endpoint exists.
//
// The reason survives in the wrapped message, which only ever reaches the
// panel's own log.
var ErrJoinRejected = errors.New("chain: join rejected")

func joinRejected(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrJoinRejected, fmt.Sprintf(format, args...))
}

// JoinRequest is what a box asks with (§4.3). Token is the one the owner
// handed out; Host, SubPort and SubScheme are how the box believes it is
// reachable, and the registry takes them over its own when they are sent.
// ObservedAddr is the address the join really arrived from — the direct
// neighbour's X-Chain-Observed, or the panel's own view of the peer.
type JoinRequest struct {
	Token        string
	Host         string
	SubPort      int
	SubScheme    string
	ObservedAddr string

	// FallbackPanelHost is the address the join arrived at, used as the
	// document's nextHop.host when chainPanelHost is unset. A forwarded join
	// reaches the panel from the first-tier hop, so it is that hop's view of
	// the panel — which is exactly whose document this is.
	FallbackPanelHost string
}

// JoinResponse is the body of a successful POST /chain/v1/join. Secret is the
// hop secret in the clear — the only time it exists outside the box, and the
// registry keeps nothing but its hash.
type JoinResponse struct {
	HopId       int             `json:"hopId"`
	Name        string          `json:"name"`
	Secret      string          `json:"secret"`
	PollSeconds int             `json:"pollSeconds"`
	Document    *chain.Document `json:"document"`
}

// ChainJoinService turns a join token into a hop of the chain (§4.3).
//
// The panel is the only judge of a join: a neighbour can forward the request,
// but only the registry can spend the token, issue the hop secret and move the
// revision — so all of that happens here, in one transaction.
type ChainJoinService struct {
	settingService  SettingService
	documentService ChainDocumentService
}

// Join spends the token and hands the box its place in the chain.
//
// The document is built after the transaction commits, not inside it: building
// reads settings, and this SQLite runs on a single connection, so a read from
// inside the transaction would wait for the connection the transaction holds.
// The box therefore always gets a document at least as new as its own join.
func (s *ChainJoinService) Join(in JoinRequest) (*JoinResponse, error) {
	token := strings.TrimSpace(in.Token)
	if len(token) != chain.SecretLength {
		return nil, joinRejected("the token is %d characters, not %d", len(token), chain.SecretLength)
	}
	// Settings first, outside the transaction, for the same reason.
	pollSeconds, err := s.settingService.GetChainPollSeconds()
	if err != nil {
		return nil, err
	}

	secret := chain.NewSecret()
	tokenHash := chain.HashSecret(token)
	var joined model.ChainHop

	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		var hop model.ChainHop
		err := tx.Where("join_token_hash = ? AND join_token_hash <> ''", tokenHash).First(&hop).Error
		if database.IsNotFound(err) {
			return joinRejected("no hop holds this join token")
		}
		if err != nil {
			return err
		}
		if hop.State != chain.StatePending {
			return joinRejected("hop %q is %s, not %s", hop.Name, hop.State, chain.StatePending)
		}
		if hop.JoinTokenExpires <= time.Now().UnixMilli() {
			return joinRejected("the join token of hop %q expired at %d", hop.Name, hop.JoinTokenExpires)
		}
		if err := applyReportedAddress(&hop, in); err != nil {
			return err
		}
		observed := strings.TrimSpace(in.ObservedAddr)
		if !chain.ObservedAddrValid(observed) {
			// A hint that is not an address is no hint: the hop keeps
			// whatever it had rather than showing the owner nonsense (§4.4).
			observed = ""
		}
		if err := markJoinedTx(tx, &hop, chain.HashSecret(secret), observed); err != nil {
			return err
		}
		joined = hop
		return nil
	})
	if err != nil {
		return nil, err
	}

	document, err := s.documentService.BuildWithPanelHost(joined.Name, in.FallbackPanelHost)
	if err != nil {
		return nil, err
	}
	return &JoinResponse{
		HopId:       joined.Id,
		Name:        joined.Name,
		Secret:      secret,
		PollSeconds: pollSeconds,
		Document:    document,
	}, nil
}

// applyReportedAddress takes over the address the box reports for itself.
//
// The owner types a host when creating the hop, but the box is the one that
// knows which port and scheme its sub server really came up on, so what it
// sends wins over what was typed (§4.4). What it does not send is left alone.
// A malformed value is a refusal rather than a correction: a hop reachable at
// an address nobody meant is worse than a box that retries.
func applyReportedAddress(hop *model.ChainHop, in JoinRequest) error {
	if reported := strings.TrimSpace(in.Host); reported != "" {
		host, err := validHost(reported)
		if err != nil {
			return joinRejected("hop %q reported %v", hop.Name, err)
		}
		hop.Host = host
	}
	if in.SubPort != 0 {
		subPort, err := validSubPort(in.SubPort)
		if err != nil {
			return joinRejected("hop %q reported %v", hop.Name, err)
		}
		hop.SubPort = subPort
	}
	if reported := strings.TrimSpace(in.SubScheme); reported != "" {
		subScheme, err := validSubScheme(reported)
		if err != nil {
			return joinRejected("hop %q reported %v", hop.Name, err)
		}
		hop.SubScheme = subScheme
	}
	return nil
}
