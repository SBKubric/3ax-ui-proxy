package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coinman-dev/3ax-ui/v2/config"
	"github.com/coinman-dev/3ax-ui/v2/logger"
)

// PanelService provides business logic for panel management operations.
// It handles panel restart, updates, and system-level panel controls.
type PanelService struct{}

// PanelUpdateInfo contains the current and latest available panel versions.
type PanelUpdateInfo struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	UpdateAvailable bool   `json:"updateAvailable"`
}

func (s *PanelService) RestartPanel(delay time.Duration) error {
	p, err := os.FindProcess(syscall.Getpid())
	if err != nil {
		return err
	}
	go func() {
		time.Sleep(delay)
		err := p.Signal(syscall.SIGHUP)
		if err != nil {
			logger.Error("failed to send SIGHUP signal:", err)
		}
	}()
	return nil
}

// GetUpdateInfo checks GitHub for the latest release of this fork on the
// channel of the running version (see latestPanelVersion).
func (s *PanelService) GetUpdateInfo() (*PanelUpdateInfo, error) {
	current := config.GetVersion()
	latest, err := latestPanelVersion(current)
	if err != nil {
		return nil, err
	}
	return &PanelUpdateInfo{
		CurrentVersion:  current,
		LatestVersion:   latest,
		UpdateAvailable: isNewerVersion(latest, current),
	}, nil
}

// StartUpdate starts this fork's updater outside of the current web request.
func (s *PanelService) StartUpdate() error {
	return startPanelUpdate(config.GetVersion())
}

// startPanelUpdate runs update.sh of panelRepo at the release latestPanelVersion
// offers for current, pinned to that tag, and refuses when that release is not
// newer than current: the web update never downgrades (#130).
func startPanelUpdate(current string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("panel web update is supported only on Linux installations")
	}
	tag, err := latestPanelVersion(current)
	if err != nil {
		return fmt.Errorf("failed to find the latest panel release: %w", err)
	}
	if !isNewerVersion(tag, current) {
		return fmt.Errorf("no newer release of %s than %s (latest on this channel: %s)", panelRepo, current, tag)
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("bash is required to run the panel updater: %w", err)
	}
	curl, err := exec.LookPath("curl")
	if err != nil {
		return fmt.Errorf("curl is required to download the panel updater: %w", err)
	}

	mainFolder, serviceFolder := resolveUpdateFolders()
	updateScript := panelUpdateScript(curl, bash, tag)

	if systemdRun, err := exec.LookPath("systemd-run"); err == nil {
		unitName := fmt.Sprintf("x-ui-web-update-%d", time.Now().Unix())
		cmd := exec.Command(systemdRun,
			"--unit", unitName,
			"--setenv", "XUI_MAIN_FOLDER="+mainFolder,
			"--setenv", "XUI_SERVICE="+serviceFolder,
			"--setenv", "XUI_REPO="+panelRepo,
			bash, "-lc", updateScript,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			output := strings.TrimSpace(string(out))
			if !strings.Contains(output, "System has not been booted with systemd") &&
				!strings.Contains(output, "Failed to connect to bus") {
				return fmt.Errorf("failed to start panel update job: %w: %s", err, output)
			}
			logger.Warning("systemd-run is unavailable, falling back to detached update process:", output)
		} else {
			logger.Infof("started panel update job via systemd-run unit %s", unitName)
			return nil
		}
	}

	cmd := exec.Command(bash, "-lc", updateScript)
	cmd.Env = append(os.Environ(),
		"XUI_MAIN_FOLDER="+mainFolder,
		"XUI_SERVICE="+serviceFolder,
		"XUI_REPO="+panelRepo,
	)
	setDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start panel update job: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		logger.Warning("failed to release panel update process:", err)
	}
	logger.Infof("started panel update job with pid %d", cmd.Process.Pid)
	return nil
}

// panelRepo is the GitHub repository the panel looks for new releases in and
// updates from: this fork, not the 3AX-UI upstream it is built on (#130).
const panelRepo = "SBKubric/sane-3x-ui"

// panelGitHubAPI is GitHub's REST API root; tests point it at a fake.
var panelGitHubAPI = "https://api.github.com"

// panelRelease is the part of a GitHub release the update check reads.
type panelRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

var (
	panelVerCacheMu      sync.Mutex
	panelVerCache        string
	panelVerCacheChannel string
	panelVerCacheAt      time.Time
)

func resetPanelVersionCache() {
	panelVerCacheMu.Lock()
	panelVerCache, panelVerCacheChannel, panelVerCacheAt = "", "", time.Time{}
	panelVerCacheMu.Unlock()
}

// latestPanelVersion returns the newest release of panelRepo on the channel of
// the running version: a pre-release build (a tag with a suffix, such as
// v1.9.0-chain.8) is offered every non-draft release, newest by version
// precedence; a stable build only stable releases (GitHub's /releases/latest).
// The result is cached per channel for an hour to avoid hammering the API on
// dashboard refreshes.
func latestPanelVersion(current string) (string, error) {
	channel := "stable"
	if _, pre, ok := parseVersion(current); ok && pre != "" {
		channel = "pre"
	}
	panelVerCacheMu.Lock()
	if panelVerCache != "" && panelVerCacheChannel == channel && time.Since(panelVerCacheAt) < time.Hour {
		v := panelVerCache
		panelVerCacheMu.Unlock()
		return v, nil
	}
	panelVerCacheMu.Unlock()

	var latest string
	var err error
	if channel == "pre" {
		latest, err = newestPanelRelease()
	} else {
		var rel panelRelease
		err = getPanelReleases("/releases/latest", &rel)
		latest = rel.TagName
	}
	if err != nil {
		return "", err
	}
	if latest == "" {
		return "", fmt.Errorf("no %s release of %s found", channel, panelRepo)
	}

	panelVerCacheMu.Lock()
	panelVerCache, panelVerCacheChannel, panelVerCacheAt = latest, channel, time.Now()
	panelVerCacheMu.Unlock()
	return latest, nil
}

// newestPanelRelease picks the highest non-draft tag among the repository's
// recent releases, pre-releases included.
func newestPanelRelease() (string, error) {
	var releases []panelRelease
	if err := getPanelReleases("/releases?per_page=100", &releases); err != nil {
		return "", err
	}
	newest := ""
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		if newest == "" {
			if _, _, ok := parseVersion(rel.TagName); ok {
				newest = rel.TagName
			}
			continue
		}
		if cmp, ok := compareVersionStrings(rel.TagName, newest); ok && cmp > 0 {
			newest = rel.TagName
		}
	}
	return newest, nil
}

func getPanelReleases(path string, out any) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(panelGitHubAPI + "/repos/" + panelRepo + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API returned status %d for %s releases: %s", resp.StatusCode, panelRepo, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func resolveUpdateFolders() (string, string) {
	mainFolder := os.Getenv("XUI_MAIN_FOLDER")
	if mainFolder == "" {
		if exePath, err := os.Executable(); err == nil {
			mainFolder = filepath.Dir(exePath)
		}
	}
	if mainFolder == "" {
		mainFolder = "/usr/local/x-ui"
	}

	serviceFolder := os.Getenv("XUI_SERVICE")
	if serviceFolder == "" {
		serviceFolder = "/etc/systemd/system"
	}
	return mainFolder, serviceFolder
}

func isNewerVersion(latest string, current string) bool {
	cmp, ok := compareVersionStrings(latest, current)
	if !ok {
		return normalizeVersionTag(latest) != normalizeVersionTag(current)
	}
	return cmp > 0
}

// compareVersionStrings orders two release tags: X.Y.Z or X.Y.Z.W (the fork's
// 1.8.1.x line), optionally with a pre-release suffix after the first '-'
// ("1.9.0-chain.8", "1.6.3-beta", a git-describe "1.5.0-beta-16-g1b74ce41").
// Numeric parts compare first (missing ones count as 0); on a tie a release
// beats its own pre-releases, and pre-releases compare by semver precedence of
// their dot-separated identifiers, so "chain.10" > "chain.9" (#130).
func compareVersionStrings(a string, b string) (int, bool) {
	aNums, aPre, okA := parseVersion(a)
	bNums, bPre, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := 0; i < max(len(aNums), len(bNums)); i++ {
		var x, y int
		if i < len(aNums) {
			x = aNums[i]
		}
		if i < len(bNums) {
			y = bNums[i]
		}
		if x != y {
			return cmpInt(x, y), true
		}
	}
	switch {
	case aPre == "" && bPre == "":
		return 0, true
	case aPre == "":
		return 1, true
	case bPre == "":
		return -1, true
	}
	return comparePreRelease(aPre, bPre), true
}

// parseVersion splits a tag into its 3 or 4 numeric parts and the pre-release
// suffix ("" for a release).
func parseVersion(version string) ([]int, string, bool) {
	s := normalizeVersionTag(version)
	pre := ""
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s, pre = s[:i], s[i+1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 3 || len(parts) > 4 {
		return nil, "", false
	}
	nums := make([]int, len(parts))
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, "", false
		}
		nums[i] = n
	}
	return nums, pre, true
}

// comparePreRelease applies semver's pre-release precedence: identifiers
// compare left to right, numeric ones numerically and below alphanumeric ones,
// and a shorter list that is a prefix of the other comes first.
func comparePreRelease(a string, b string) int {
	aIDs, bIDs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < min(len(aIDs), len(bIDs)); i++ {
		x, errX := strconv.Atoi(aIDs[i])
		y, errY := strconv.Atoi(bIDs[i])
		switch {
		case errX == nil && errY == nil:
			if x != y {
				return cmpInt(x, y)
			}
		case errX == nil:
			return -1
		case errY == nil:
			return 1
		default:
			if c := strings.Compare(aIDs[i], bIDs[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(aIDs), len(bIDs))
}

func cmpInt(a int, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	}
	return 0
}

func normalizeVersionTag(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// panelUpdateScript is the shell line of the web update: panelRepo's update.sh
// taken at tag and told to install exactly that tag.
func panelUpdateScript(curl string, bash string, tag string) string {
	url := "https://raw.githubusercontent.com/" + panelRepo + "/" + tag + "/update.sh"
	return fmt.Sprintf("set -o pipefail; %s -fLs %s | %s -s -- %s", shellQuote(curl), shellQuote(url), shellQuote(bash), shellQuote(tag))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
