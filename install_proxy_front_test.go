package main

import (
	"encoding/json"
	"strings"
	"testing"
)

var proxyFrontFunctions = append([]string{"proxy_config_json"}, promptProxyModeFunctions...)

// TestInstallProxyFrontOption (#140): PROXY_FRONT picks the box's front at
// install, off unless asked for, and lands in proxy.json where `x-ui proxy`
// reads it; anything else is refused before the box is touched.
func TestInstallProxyFrontOption(t *testing.T) {
	cases := []struct {
		name, front, want string
	}{
		{name: "default", front: "", want: "off"},
		{name: "off", front: "off", want: "off"},
		{name: "only443", front: "only443", want: "only443"},
		{name: "loosely spelled", front: "ONLY443", want: "only443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := append(proxyModeEnv("none", "", ""), "PROXY_FRONT="+tc.front)
			out, err := runInstallShell(t, proxyFrontFunctions, "prompt_proxy_mode >/dev/null\nproxy_config_json\n", env...)
			if err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			var cfg struct {
				Version int `json:"version"`
				Front   struct {
					Mode string `json:"mode"`
				} `json:"front"`
				NextHop struct {
					Host string `json:"host"`
				} `json:"nextHop"`
			}
			if err := json.Unmarshal([]byte(out), &cfg); err != nil {
				t.Fatalf("proxy.json is not JSON: %v\n%s", err, out)
			}
			if cfg.Front.Mode != tc.want || cfg.Version != 2 || cfg.NextHop.Host != "10.0.0.7" {
				t.Errorf("proxy.json = %+v, want front %q", cfg, tc.want)
			}
		})
	}

	out, err := runInstallShell(t, proxyFrontFunctions, "prompt_proxy_mode\necho reached\n",
		append(proxyModeEnv("none", "", ""), "PROXY_FRONT=shared")...)
	if err == nil || strings.Contains(out, "reached") {
		t.Errorf("PROXY_FRONT=shared was accepted:\n%s", out)
	}
}
