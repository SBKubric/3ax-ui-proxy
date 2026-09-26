package sub

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
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

// Subscriptions of a panel whose Reality inbounds follow the chain (#139,
// ADR 0005): the links carry the active edge's neighbour as their SNI, and
// clients are told to come back often enough to see the next switch.

const followerClient = `{"id":"a5899a9e-dc44-4ec4-bba1-72b6e29bd2d1","email":"follower","subId":"sub-follow","enable":true,"flow":""}`

const followerStream = `{"network":"tcp","security":"reality","tcpSettings":{},"realitySettings":{` +
	`"show":false,"xver":0,"target":"www.original.example:443",` +
	`"serverNames":["www.original.example","original.example"],` +
	`"privateKey":"priv","shortIds":["ab12"],` +
	`"settings":{"publicKey":"pub","fingerprint":"chrome","serverName":"","spiderX":"/"}}}`

// subRouter serves /sub/ and /json/ with a 12-hour update interval, the
// panel's default.
func subRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewSUBController(engine.Group("/"), "/sub/", "/json/", "/clash/", true, false, false, false,
		"-ieo", "12", "", "", "", "", "", "", "", "", false, "", "")
	return engine
}

func seedSubInbound(t *testing.T, port int, follow bool) {
	t.Helper()
	inbound := &model.Inbound{
		UserId: 1, Remark: "reality", Enable: true, Port: port,
		Protocol: model.VLESS, Tag: "inbound-" + strconv.Itoa(port),
		Settings:       `{"clients":[` + followerClient + `],"decryption":"none"}`,
		StreamSettings: followerStream, Sniffing: `{"enabled":false}`,
		FollowChain: follow,
	}
	if err := database.GetDB().Create(inbound).Error; err != nil {
		t.Fatalf("create inbound: %v", err)
	}
}

func get(engine *gin.Engine, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "panel.example.net"
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestChainFollowingSubscriptions(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { database.CloseDB() })
	resetInboundsCache()
	engine := subRouter(t)

	// No inbound follows the chain: the interval is the panel's own.
	seedSubInbound(t, 24443, false)
	if got := get(engine, "/sub/sub-follow").Header().Get("Profile-Update-Interval"); got != "12" {
		t.Fatalf("Profile-Update-Interval without followers = %q, want the setting's 12", got)
	}

	// One does, and the chain switches onto edge-b: every link names edge-b's
	// neighbour, the only name the inbound still accepts.
	registry := &service.ChainService{}
	edge, _, _, err := registry.Add(service.AddHopInput{Name: "edge-b", Host: "b.example.net", Role: chain.RoleEdge,
		RealityTarget: "198.51.100.20:443", RealityServerName: "www.neighbour-b.example"})
	if err != nil {
		t.Fatalf("Add(edge-b): %v", err)
	}
	if err := registry.MarkJoined(edge.Id, chain.HashSecret("secret"), ""); err != nil {
		t.Fatalf("MarkJoined: %v", err)
	}
	seedSubInbound(t, 24444, true)
	if err := registry.SetActive(edge.Id); err != nil {
		t.Fatalf("SetActive(edge-b): %v", err)
	}

	sub := get(engine, "/sub/sub-follow")
	if sub.Code != http.StatusOK {
		t.Fatalf("/sub: status %d: %s", sub.Code, sub.Body.String())
	}
	// A client polls at most hourly while inbounds follow the chain: the
	// header carries whole hours, and 1 is the least it can say.
	if got := sub.Header().Get("Profile-Update-Interval"); got != "1" {
		t.Errorf("Profile-Update-Interval with a follower = %q, want 1", got)
	}
	body := sub.Body.String()
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body)); err == nil {
		body = string(decoded)
	}
	var followerLink, plainLink string
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		switch {
		case strings.Contains(line, ":24444"):
			followerLink = line
		case strings.Contains(line, ":24443"):
			plainLink = line
		}
	}
	if !strings.Contains(followerLink, "sni=www.neighbour-b.example") {
		t.Errorf("the follower's link does not name edge-b's neighbour: %q", followerLink)
	}
	if !strings.Contains(followerLink, "@b.example.net:") {
		t.Errorf("the follower's link does not point at the active edge: %q", followerLink)
	}
	if strings.Contains(plainLink, "neighbour-b") {
		t.Errorf("an inbound that does not follow the chain took its neighbour: %q", plainLink)
	}

	json := get(engine, "/json/sub-follow")
	if json.Code != http.StatusOK {
		t.Fatalf("/json: status %d: %s", json.Code, json.Body.String())
	}
	if got := json.Header().Get("Profile-Update-Interval"); got != "1" {
		t.Errorf("JSON Profile-Update-Interval with a follower = %q, want 1", got)
	}
	if !strings.Contains(json.Body.String(), `"serverName": "www.neighbour-b.example"`) {
		t.Errorf("the JSON subscription does not name edge-b's neighbour:\n%s", json.Body.String())
	}
}
