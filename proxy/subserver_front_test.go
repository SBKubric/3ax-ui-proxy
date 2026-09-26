package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func answers(addr string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/nothing-here")
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return true
}

// TestSubServerMovesBehindTheFront (#140): behind the front the sub server
// answers on a loopback address for nginx, and its public port closes once the
// outer neighbours have moved — and opens again if the front goes away.
func TestSubServerMovesBehindTheFront(t *testing.T) {
	public := freeLoopbackAddr(t)
	host, portText, _ := net.SplitHostPort(public)
	port, _ := strconv.Atoi(portText)
	cfg := &Config{SubListen: host, SubPort: port, NextHop: NextHop{Host: "10.0.0.7", SubPort: DefaultSubPort, SubScheme: "https"}}
	s := testSubServer(t, cfg, NewState())
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { s.Stop() })

	loopback := freeLoopbackAddr(t)
	if err := s.ServeLoopback(loopback); err != nil {
		t.Fatalf("ServeLoopback: %v", err)
	}
	if !answers(loopback) || !answers(public) {
		t.Fatal("both listeners should answer while the neighbours move")
	}
	// Serving the same address again is a no-op, not a second bind.
	if err := s.ServeLoopback(loopback); err != nil {
		t.Errorf("ServeLoopback twice: %v", err)
	}

	if err := s.ClosePublic(); err != nil {
		t.Fatalf("ClosePublic: %v", err)
	}
	if answers(public) {
		t.Error("the public sub port still answers after it was closed")
	}
	if !answers(loopback) {
		t.Error("closing the public port took the loopback down too")
	}

	if err := s.OpenPublic(); err != nil {
		t.Fatalf("OpenPublic: %v", err)
	}
	s.StopLoopback()
	if !answers(public) {
		t.Error("the public sub port did not come back")
	}
	if answers(loopback) {
		t.Error("the loopback listener outlived the front")
	}
}

// TestBehindTheFrontTheClientIsTheOneNginxSaw: through the front every request
// reaches the sub server from 127.0.0.1. The address a join came from is
// evidence the owner reads (§4.4), so on the loopback listener — and only
// there — the X-Real-IP nginx sets is the remote address.
func TestBehindTheFrontTheClientIsTheOneNginxSaw(t *testing.T) {
	var seen string
	h := trustFront(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = remoteHost(r.RemoteAddr) }))

	req := httptest.NewRequest(http.MethodGet, "/chain/v1/join", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Real-IP", "198.51.100.77")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "198.51.100.77" {
		t.Errorf("remote = %q, want the client nginx saw", seen)
	}

	req = httptest.NewRequest(http.MethodGet, "/chain/v1/join", nil)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Real-IP", "not an address")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "127.0.0.1" {
		t.Errorf("remote with a junk X-Real-IP = %q, want the peer as it is", seen)
	}
}
