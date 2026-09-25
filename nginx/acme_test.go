package nginx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestACMEFrontConf pins the port-80 server: the ACME challenge path served
// from the webroot acme.sh writes into, and everything else sent to https.
func TestACMEFrontConf(t *testing.T) {
	got := acmeFrontConf(80, "/usr/local/x-ui/acme-webroot")
	check(t, "acme_front.conf", got)
	if !strings.HasPrefix(got, header) {
		t.Error("the port-80 file does not carry the generated-file header")
	}
}

// acmeTree points the package at a temporary nginx tree shaped like the given
// distro and returns its root.
func acmeTree(t *testing.T, httpDir string) string {
	t.Helper()
	root := t.TempDir()
	prevRoot, prevWebroot := ConfRoot, ACMEWebroot
	ConfRoot = root
	ACMEWebroot = filepath.Join(t.TempDir(), "acme-webroot")
	t.Cleanup(func() { ConfRoot, ACMEWebroot = prevRoot, prevWebroot })
	if err := os.MkdirAll(filepath.Join(root, httpDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// debianDefaultSite lays out what the nginx package leaves on Debian and
// Ubuntu: the site in sites-available and a symlink to it in sites-enabled.
func debianDefaultSite(t *testing.T, root, target string) string {
	t.Helper()
	for _, dir := range []string{"sites-available", "sites-enabled"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	site := "server {\n    listen 80 default_server;\n    listen [::]:80 default_server;\n    root /var/www/html;\n}\n"
	if err := os.WriteFile(filepath.Join(root, "sites-available", "default"), []byte(site), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sites-enabled", "default")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// TestStageACMEFrontTakesPort80FromTheDebianDefault: the package's default site
// claims default_server on :80 as well, and two default servers on one port is
// a config nginx refuses. The symlink goes; the site itself stays in
// sites-available, so nothing of the operator's is lost.
func TestStageACMEFrontTakesPort80FromTheDebianDefault(t *testing.T) {
	root := acmeTree(t, "conf.d")
	link := debianDefaultSite(t, root, filepath.Join(root, "sites-available", "default"))

	tx := newTx()
	changed, err := stageACMEFront(tx)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if !changed {
		t.Error("a fresh tree was reported as already in place")
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("the distro's default site is still enabled; nginx would see two default servers on :80")
	}
	if _, err := os.Stat(filepath.Join(root, "sites-available", "default")); err != nil {
		t.Error("the default site itself was deleted, not just disabled")
	}
	if got := read(t, filepath.Join(root, "conf.d", acmeConfName)); got != acmeFrontConf(80, ACMEWebroot) {
		t.Errorf("the port-80 file is not what the renderer produces:\n%s", got)
	}
	if st, err := os.Stat(ACMEWebroot); err != nil || !st.IsDir() {
		t.Errorf("the webroot acme.sh writes into was not created: %v", err)
	}

	// Rolled back — nginx refused the result — the symlink is a symlink again,
	// not a copy of the file it pointed at.
	tx.rollback()
	if target, err := os.Readlink(link); err != nil {
		t.Errorf("the default site did not come back as a symlink: %v", err)
	} else if target != filepath.Join(root, "sites-available", "default") {
		t.Errorf("the restored symlink points at %s", target)
	}
	if _, err := os.Stat(filepath.Join(root, "conf.d", acmeConfName)); !os.IsNotExist(err) {
		t.Error("the port-80 file was left behind after a rollback")
	}
}

// TestStageACMEFrontIsIdempotent: install.sh, update.sh and every certificate
// menu run it; the second run must find nothing to do, so nginx is not
// reloaded for nothing.
func TestStageACMEFrontIsIdempotent(t *testing.T) {
	root := acmeTree(t, "conf.d")
	debianDefaultSite(t, root, "../sites-available/default")

	tx := newTx()
	if _, err := stageACMEFront(tx); err != nil {
		t.Fatalf("first stage: %v", err)
	}
	tx.commit()

	again := newTx()
	changed, err := stageACMEFront(again)
	if err != nil {
		t.Fatalf("second stage: %v", err)
	}
	if changed {
		t.Error("a second run over the same tree reported a change")
	}
}

// TestStageACMEFrontDisablesTheAlpineDefault: Alpine ships its default server
// as http.d/default.conf. It is renamed out of the include glob, not deleted.
func TestStageACMEFrontDisablesTheAlpineDefault(t *testing.T) {
	root := acmeTree(t, "http.d")
	def := filepath.Join(root, "http.d", "default.conf")
	body := "server {\n\tlisten 80 default_server;\n\tlisten [::]:80 default_server;\n\tlocation / { return 404; }\n}\n"
	if err := os.WriteFile(def, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	tx := newTx()
	if _, err := stageACMEFront(tx); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Error("Alpine's default server is still included")
	}
	if got := read(t, def+alpineDisabledSuffix); got != body {
		t.Errorf("the disabled default lost its content: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "http.d", acmeConfName)); err != nil {
		t.Errorf("the port-80 file did not land in http.d: %v", err)
	}
}

// TestStageACMEFrontLeavesForeignSitesAlone: a sites-enabled/default that is
// not the package's symlink is somebody's own site. It is not ours to remove;
// if it clashes, `nginx -t` says so and the caller reports it.
func TestStageACMEFrontLeavesForeignSitesAlone(t *testing.T) {
	root := acmeTree(t, "conf.d")
	link := debianDefaultSite(t, root, "/srv/my-site.conf")

	if _, err := stageACMEFront(newTx()); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != "/srv/my-site.conf" {
		t.Errorf("a site that is not the distro default was touched: %q %v", target, err)
	}
}

// TestStageACMEFrontRefusesAHandWrittenFile: a file of that name without our
// header is somebody's config, and is never overwritten.
func TestStageACMEFrontRefusesAHandWrittenFile(t *testing.T) {
	root := acmeTree(t, "conf.d")
	path := filepath.Join(root, "conf.d", acmeConfName)
	if err := os.WriteFile(path, []byte("server { listen 80; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := stageACMEFront(newTx()); err == nil {
		t.Fatal("a hand-written file was overwritten")
	}
	if got := read(t, path); got != "server { listen 80; }\n" {
		t.Errorf("the hand-written file changed: %q", got)
	}
}

// fakeNginx stands in for the nginx process: the files are real, the process
// is not. served is what "nginx" answers on port 80 — the webroot when it has
// picked the new config up, nothing otherwise.
type fakeNginx struct {
	testErr   error
	running   bool
	reloads   int
	serveRoot bool
}

func useFakeNginx(t *testing.T, f *fakeNginx) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.serveRoot {
			// Whatever else holds port 80 — it knows nothing of the probe.
			http.NotFound(w, r)
			return
		}
		http.FileServer(http.Dir(ACMEWebroot)).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	prevOps, prevProbe := acmeOps, acmeProbeBase
	acmeOps.installed = func() bool { return true }
	acmeOps.test = func() error { return f.testErr }
	acmeOps.running = func() bool { return f.running }
	acmeOps.reload = func() error { f.reloads++; f.running = true; return nil }
	acmeProbeBase = srv.URL
	t.Cleanup(func() { acmeOps, acmeProbeBase = prevOps, prevProbe })
}

func TestEnsureACMEFrontReloadsOnceAndThenLeavesNginxAlone(t *testing.T) {
	root := acmeTree(t, "conf.d")
	debianDefaultSite(t, root, "../sites-available/default")
	f := &fakeNginx{running: true, serveRoot: true}
	useFakeNginx(t, f)

	changed, err := EnsureACMEFront()
	if err != nil {
		t.Fatalf("EnsureACMEFront: %v", err)
	}
	if !changed || f.reloads != 1 {
		t.Errorf("changed=%v after %d reloads, want a change and exactly one reload", changed, f.reloads)
	}

	changed, err = EnsureACMEFront()
	if err != nil {
		t.Fatalf("second EnsureACMEFront: %v", err)
	}
	if changed || f.reloads != 1 {
		t.Errorf("the second run changed=%v and reloaded %d times in total; it should do nothing", changed, f.reloads)
	}
	if entries, _ := os.ReadDir(filepath.Join(ACMEWebroot, ".well-known", "acme-challenge")); len(entries) != 0 {
		t.Errorf("the probe left files in the webroot: %v", entries)
	}
}

// TestEnsureACMEFrontStartsAStoppedNginx: the config is in place but nginx is
// down — after a reboot without the unit enabled, say. Nothing to write, but
// nobody answers the CA either.
func TestEnsureACMEFrontStartsAStoppedNginx(t *testing.T) {
	acmeTree(t, "conf.d")
	f := &fakeNginx{running: true, serveRoot: true}
	useFakeNginx(t, f)
	if _, err := EnsureACMEFront(); err != nil {
		t.Fatalf("first run: %v", err)
	}

	f.running = false
	if _, err := EnsureACMEFront(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if f.reloads != 2 || !f.running {
		t.Errorf("a stopped nginx was not started (reloads=%d, running=%v)", f.reloads, f.running)
	}
}

// TestEnsureACMEFrontPutsEverythingBackWhenNginxRefuses: an operator's own
// default_server on :80 makes nginx -t fail. The box has to be left exactly
// as it was, with the reason spelled out.
func TestEnsureACMEFrontPutsEverythingBackWhenNginxRefuses(t *testing.T) {
	root := acmeTree(t, "conf.d")
	link := debianDefaultSite(t, root, "../sites-available/default")
	f := &fakeNginx{
		running: true, serveRoot: true,
		testErr: errors.New(`nginx -t: nginx: [emerg] a duplicate default server for 0.0.0.0:80 in /etc/nginx/sites-enabled/mine:2`),
	}
	useFakeNginx(t, f)

	_, err := EnsureACMEFront()
	if err == nil {
		t.Fatal("a config nginx refused was reported as applied")
	}
	if !strings.Contains(err.Error(), "duplicate default server") || !strings.Contains(err.Error(), "port 80") {
		t.Errorf("the error does not explain the clash: %v", err)
	}
	if f.reloads != 0 {
		t.Error("nginx was reloaded onto a config it refused")
	}
	if _, err := os.Readlink(link); err != nil {
		t.Error("the default site was not put back")
	}
	if _, err := os.Stat(ACMEConfPath()); !os.IsNotExist(err) {
		t.Error("the port-80 file was left behind")
	}
}

// TestEnsureACMEFrontNoticesSomethingElseOnPort80: nginx reloads cleanly but
// port 80 is answered by another process (a leftover acme.sh standalone
// listener, another web server). The reload is undone and the caller told.
func TestEnsureACMEFrontNoticesSomethingElseOnPort80(t *testing.T) {
	acmeTree(t, "conf.d")
	f := &fakeNginx{running: true, serveRoot: false}
	useFakeNginx(t, f)

	if _, err := EnsureACMEFront(); err == nil {
		t.Fatal("port 80 answered without the webroot and nothing was reported")
	}
	if _, err := os.Stat(ACMEConfPath()); !os.IsNotExist(err) {
		t.Error("the port-80 file was kept although nginx does not serve it")
	}
	if f.reloads != 2 {
		t.Errorf("reloads=%d, want the reload and the one that puts the old config back", f.reloads)
	}
}
