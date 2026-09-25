package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		latest  string
		current string
		want    bool
	}{
		{"v2.9.4", "2.9.3", true},
		{"v2.10.0", "2.9.9", true},
		{"v2.9.3", "2.9.3", false},
		{"v2.9.2", "2.9.3", false},
		{"v3.0.0", "2.9.3", true},
		// A beta / git-describe current version compares by its numeric prefix,
		// and the latest stable release must never look like a downgrade.
		{"v1.6.4", "1.6.3-beta", true},
		{"v1.6.4", "1.5.0-beta-16-g1b74ce41-dirty", true},
		{"v1.3.3", "1.6.3-beta", false},
		// Pre-releases of the same X.Y.Z order by their suffix, and a release
		// beats its own pre-releases (#130).
		{"v1.9.0-chain.8", "1.9.0-chain.7", true},
		{"v1.9.0-chain.10", "v1.9.0-chain.9", true},
		{"v1.9.0-chain.7", "v1.9.0-chain.8", false},
		{"v1.9.0-chain.8", "v1.9.0-chain.8", false},
		{"v1.9.0", "v1.9.0-chain.8", true},
		{"v1.9.0-chain.9", "v1.9.0", false},
		// Four-part tags (the fork's v1.8.1.x line) compare on every part.
		{"v1.8.1.5", "v1.8.1.4", true},
		{"v1.8.1.5", "v1.9.0-chain.8", false},
		{"v1.9.0-chain.8", "v1.8.1.5", true},
	}

	for _, tc := range cases {
		if got := isNewerVersion(tc.latest, tc.current); got != tc.want {
			t.Fatalf("isNewerVersion(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestCompareVersionStringsRejectsUnexpectedFormats(t *testing.T) {
	if _, ok := compareVersionStrings("latest", "2.9.3"); ok {
		t.Fatal("expected non-semver latest tag to be rejected")
	}
	if _, ok := compareVersionStrings("v2.9", "2.9.3"); ok {
		t.Fatal("expected short version to be rejected")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/usr/bin/curl"); got != "'/usr/bin/curl'" {
		t.Fatalf("unexpected quote result: %s", got)
	}
	if got := shellQuote("/tmp/a'b"); got != "'/tmp/a'\\''b'" {
		t.Fatalf("unexpected quote result with single quote: %s", got)
	}
}

// fakePanelReleases serves /repos/<panelRepo>/releases and .../releases/latest
// the way GitHub does: the list newest first, drafts included, and latest as
// the newest non-draft non-prerelease.
func fakePanelReleases(t *testing.T, releases []map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+panelRepo+"/releases", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(releases)
	})
	mux.HandleFunc("/repos/"+panelRepo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		for _, rel := range releases {
			if rel["draft"] != true && rel["prerelease"] != true {
				_ = json.NewEncoder(w).Encode(rel)
				return
			}
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldAPI := panelGitHubAPI
	panelGitHubAPI = srv.URL
	t.Cleanup(func() { panelGitHubAPI = oldAPI })
	resetPanelVersionCache()
	t.Cleanup(resetPanelVersionCache)
}

// The fork's releases as of #130: a stable 1.8.1.x line and pre-release chain builds.
var forkReleases = []map[string]any{
	{"tag_name": "v1.9.0-chain.9", "draft": true, "prerelease": true},
	{"tag_name": "v1.9.0-chain.8", "prerelease": true},
	{"tag_name": "v1.9.0-chain.10-rc", "prerelease": true, "draft": false},
	{"tag_name": "v1.9.0-chain.7", "prerelease": true},
	{"tag_name": "v1.8.1.5"},
	{"tag_name": "v1.8.1.4"},
}

func TestLatestPanelVersion_FollowsTheChannelOfTheCurrentVersion(t *testing.T) {
	fakePanelReleases(t, forkReleases)
	cases := []struct{ current, want string }{
		// A pre-release build sees pre-releases too, newest by precedence, never a draft.
		{"v1.9.0-chain.8", "v1.9.0-chain.10-rc"},
		// A stable build sees stable releases only.
		{"v1.8.1.4", "v1.8.1.5"},
	}
	for _, tc := range cases {
		got, err := latestPanelVersion(tc.current)
		if err != nil {
			t.Fatalf("latestPanelVersion(%q): %v", tc.current, err)
		}
		if got != tc.want {
			t.Errorf("latestPanelVersion(%q) = %q, want %q", tc.current, got, tc.want)
		}
	}
}

func TestLatestPanelVersion_StableBuildWithoutStableRelease(t *testing.T) {
	fakePanelReleases(t, []map[string]any{{"tag_name": "v1.9.0-chain.8", "prerelease": true}})
	if got, err := latestPanelVersion("v1.8.1.5"); err == nil {
		t.Fatalf("latestPanelVersion = %q, want an error: the repo has no stable release", got)
	}
}

// The web update never installs a release that is not newer than the running
// one: with the fork's stable latest at v1.8.1.5, the old flow would have taken
// a v1.9.0-chain.8 panel back to 1.8.1.5, or replaced it with upstream (#130).
func TestStartUpdate_RefusesWithoutANewerRelease(t *testing.T) {
	fakePanelReleases(t, forkReleases)
	for _, current := range []string{"v1.9.0-chain.10-rc", "v1.8.1.5"} {
		err := startPanelUpdate(current)
		if err == nil || !strings.Contains(err.Error(), "no newer release") {
			t.Errorf("startPanelUpdate(%q) = %v, want a refusal naming no newer release", current, err)
		}
	}
}

// The updater is this fork's update.sh at the offered tag, told to install
// exactly that tag.
func TestPanelUpdateScript_RunsTheForksUpdaterAtTheTag(t *testing.T) {
	got := panelUpdateScript("/usr/bin/curl", "/bin/bash", "v1.9.0-chain.10-rc")
	want := "set -o pipefail; '/usr/bin/curl' -fLs 'https://raw.githubusercontent.com/SBKubric/sane-3x-ui/v1.9.0-chain.10-rc/update.sh' | '/bin/bash' -s -- 'v1.9.0-chain.10-rc'"
	if got != want {
		t.Fatalf("panelUpdateScript =\n  %s\nwant\n  %s", got, want)
	}
}
