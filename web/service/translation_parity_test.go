package service

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// TestTranslationTgbotCommandsKeyParity is the DoD's TOML key-parity check
// (BRIEF-83.md): every locale must define exactly the same set of
// tgbot.commands keys, or a locale silently falls back to printing the key
// name instead of a translated string (§2.5).
func TestTranslationTgbotCommandsKeyParity(t *testing.T) {
	dir := filepath.Join("..", "translation")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	keysByLocale := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc map[string]any
		if err := toml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		tgbot, ok := doc["tgbot"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no [tgbot] table", path)
		}
		commands, ok := tgbot["commands"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no [tgbot.commands] table", path)
		}
		keys := make([]string, 0, len(commands))
		for k := range commands {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		keysByLocale[e.Name()] = keys
	}

	reference, ok := keysByLocale["translate.en_US.toml"]
	if !ok {
		t.Fatal("translate.en_US.toml not found")
	}
	for locale, keys := range keysByLocale {
		if locale == "translate.en_US.toml" {
			continue
		}
		missing, extra := diffKeys(reference, keys)
		if len(missing) > 0 || len(extra) > 0 {
			t.Errorf("%s: tgbot.commands keys differ from en_US — missing %v, extra %v", locale, missing, extra)
		}
	}
}

// diffKeys returns the sorted-slice keys in want that are absent from got,
// and the keys in got that are not in want.
func diffKeys(want, got []string) (missing, extra []string) {
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	gotSet := make(map[string]bool, len(got))
	for _, k := range got {
		gotSet[k] = true
	}
	for _, k := range want {
		if !gotSet[k] {
			missing = append(missing, k)
		}
	}
	for _, k := range got {
		if !wantSet[k] {
			extra = append(extra, k)
		}
	}
	return missing, extra
}
