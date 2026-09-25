package nginx

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// ACMEWebroot is where acme.sh writes HTTP-01 challenge files for nginx to
// serve on port 80. It sits beside the panel rather than under /var/www so an
// update, which only ever replaces files inside the tarball, leaves it alone,
// and so that uninstalling the panel takes it with it.
var ACMEWebroot = "/usr/local/x-ui/acme-webroot"

// acmeConfName is the port-80 file, next to the http-level 3ax-ui.conf but
// separate from it: that file comes and goes with the front mode, while port
// 80 has to answer the CA whatever the mode — a certificate renews every few
// days on a box where the front may well be off.
const acmeConfName = "3ax-ui-acme.conf"

// acmeFrontConf renders the port-80 server.
//
// Port 80 belongs to nginx on every box (ADR 0005). acme.sh used to take it
// for itself in standalone mode, and lost the race to any nginx already
// listening there — the distro's default site included — so an IP certificate
// could not renew on a box that also ran the front. Now acme.sh only drops a
// file into the webroot and nginx answers the CA.
//
// default_server because the CA asks for the IP certificate by address: the
// Host header is the IP, which no server_name matches. Everything that is not
// a challenge goes to https, where the front answers.
func acmeFrontConf(port int, webroot string) string {
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString("server {\n")
	fmt.Fprintf(&b, "    listen %d default_server;\n", port)
	fmt.Fprintf(&b, "    listen [::]:%d default_server;\n\n", port)
	b.WriteString("    access_log off;\n")
	b.WriteString("    server_tokens off;\n\n")
	// ^~ so that no regex location elsewhere in the http block can take the
	// challenge path over.
	b.WriteString("    location ^~ /.well-known/acme-challenge/ {\n")
	fmt.Fprintf(&b, "        root %s;\n", webroot)
	b.WriteString("        default_type text/plain;\n")
	b.WriteString("    }\n\n")
	b.WriteString("    location / {\n")
	b.WriteString("        return 301 https://$host$request_uri;\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")
	return b.String()
}

// ACMEConfPath is the port-80 file, in the directory this distro includes
// from inside http {}.
func ACMEConfPath() string { return filepath.Join(filepath.Dir(HTTPConfPath()), acmeConfName) }

// alpineDisabledSuffix is appended to Alpine's default server to take it out
// of the http.d/*.conf glob while keeping it on disk.
const alpineDisabledSuffix = ".disabled-by-3ax-ui"

// stageACMEFront writes the port-80 file and takes the distro's own default
// server out of the way, recording every change in tx. changed is false when
// the tree already is what it should be, so the caller can skip the reload.
func stageACMEFront(tx *fileTx) (changed bool, err error) {
	if err := os.MkdirAll(ACMEWebroot, 0o755); err != nil {
		return false, fmt.Errorf("create the ACME webroot: %w", err)
	}

	want := acmeFrontConf(80, ACMEWebroot)
	path := ACMEConfPath()
	current, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return false, fmt.Errorf("read %s: %w", path, err)
	case !strings.HasPrefix(string(current), header):
		return false, fmt.Errorf("%s exists and was not written by 3AX-UI; move it away and try again", path)
	}
	if string(current) != want {
		if err := tx.write(path, want, 0o644); err != nil {
			return false, err
		}
		changed = true
	}

	disabled, err := disableDistroDefault(tx)
	if err != nil {
		return false, err
	}
	return changed || disabled, nil
}

// disableDistroDefault takes the site a distro's nginx package enables on
// port 80 out of the build. It claims default_server there too, and nginx
// refuses two default servers on one port.
//
// Only the package's own arrangement is touched. Anything else on port 80 is
// the operator's, and if it clashes `nginx -t` names it and the caller stops.
func disableDistroDefault(tx *fileTx) (bool, error) {
	changed := false

	// Debian and Ubuntu: sites-enabled/default is a symlink to
	// sites-available/default. Removing the symlink disables the site and
	// leaves it where `ln -s` can bring it back.
	link := filepath.Join(ConfRoot, "sites-enabled", "default")
	if target, err := os.Readlink(link); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(link), target)
		}
		if filepath.Clean(target) == filepath.Join(ConfRoot, "sites-available", "default") {
			if err := tx.remove(link); err != nil {
				return false, err
			}
			changed = true
		}
	}

	// Alpine: http.d/default.conf, a 404 on every name. Renamed rather than
	// deleted, so it is still there to compare against after a package
	// upgrade.
	def := filepath.Join(ConfRoot, "http.d", "default.conf")
	if body, err := os.ReadFile(def); err == nil && strings.Contains(string(body), "default_server") {
		if err := tx.write(def+alpineDisabledSuffix, string(body), 0o644); err != nil {
			return false, err
		}
		if err := tx.remove(def); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// acmeOps is the nginx process as EnsureACMEFront drives it. A variable so the
// tests can put a fake in its place; the files it writes are real either way.
var acmeOps = struct {
	installed func() bool
	test      func() error
	reload    func() error
	running   func() bool
}{IsInstalled, Test, Reload, IsRunning}

// acmeProbeBase is where the port-80 server is asked for the probe file.
var acmeProbeBase = "http://127.0.0.1"

// EnsureACMEFront puts nginx on port 80 in front of the ACME webroot and
// confirms it answers there. changed reports whether anything was written; a
// second run over a box already set up writes nothing and reloads nothing.
//
// install.sh, update.sh and the certificate menus of x-ui.sh call it (as
// `x-ui nginx acme-front`) before every issuance, on the panel and on the
// hops alike: acme.sh then only drops the challenge into ACMEWebroot.
//
// On any failure every file is put back and nginx with it.
func EnsureACMEFront() (changed bool, err error) {
	if !acmeOps.installed() {
		return false, errors.New("nginx is not installed")
	}
	tx := newTx()
	defer tx.done()

	changed, err = stageACMEFront(tx)
	if err != nil {
		return false, err
	}
	if !changed {
		tx.commit()
		if !acmeOps.running() {
			if err := acmeOps.reload(); err != nil {
				return false, err
			}
		}
		return false, verifyACMEFront()
	}

	if err := acmeOps.test(); err != nil {
		if strings.Contains(err.Error(), "duplicate default server") {
			return false, fmt.Errorf("%w\nanother server claims default_server on port 80; disable it so nginx can answer the CA there, and run again", err)
		}
		return false, err
	}
	if err := acmeOps.reload(); err != nil {
		tx.rollback()
		_ = acmeOps.reload()
		return false, err
	}
	if err := verifyACMEFront(); err != nil {
		tx.rollback()
		if rerr := acmeOps.reload(); rerr != nil {
			logger.Errorf("nginx: the port-80 config was rolled back but nginx would not reload: %v", rerr)
		}
		return false, err
	}
	tx.commit()
	return true, nil
}

// verifyACMEFront asks port 80 for a file it has just put into the webroot —
// the same request the CA will make.
//
// A reload returns before the new workers have bound anything, and a port 80
// held by some other process answers too, just not from our webroot. Only
// getting the file back says the CA will get its challenge.
func verifyACMEFront() error {
	dir := filepath.Join(ACMEWebroot, ".well-known", "acme-challenge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	name := "3ax-ui-probe-" + hex.EncodeToString(token)
	body := hex.EncodeToString(token)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write the probe: %w", err)
	}
	defer os.Remove(path)

	client := &http.Client{
		Timeout: 2 * time.Second,
		// A redirect is the answer of a server that is not ours.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	url := acmeProbeBase + "/.well-known/acme-challenge/" + name
	for range 10 {
		if resp, err := client.Get(url); err == nil {
			got, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && string(got) == body {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("port 80 does not serve the ACME webroot %s — something other than nginx may hold the port (ss -ltnp 'sport = :80')", ACMEWebroot)
}
