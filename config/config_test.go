package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGetBinFolderPathFallsBackWhenTheExeHasNoBin: `go run .` and `go test`
// compile into a throwaway build-cache directory that never has a bin/
// folder next to it (unlike an installed panel, where install.sh always lays
// the binary and bin/ down as siblings). Resolving against the executable's
// folder in that case would break every dev flow that relies on cwd-relative
// "bin", so the default must fall back to the old bare relative name instead.
func TestGetBinFolderPathFallsBackWhenTheExeHasNoBin(t *testing.T) {
	if os.Getenv("XUI_BIN_FOLDER") != "" {
		t.Skip("XUI_BIN_FOLDER is set; this test exercises the unset default")
	}

	exePath, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	exeRelative := filepath.Join(filepath.Dir(exePath), "bin")
	if _, err := os.Stat(exeRelative); err == nil {
		t.Skipf("the test binary already has a bin/ folder at %q; the fallback is not exercised", exeRelative)
	}

	if got := GetBinFolderPath(); got != "bin" {
		t.Errorf("GetBinFolderPath() = %q, want the cwd-relative fallback %q", got, "bin")
	}
}
