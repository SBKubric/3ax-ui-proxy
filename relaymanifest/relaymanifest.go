// Package relaymanifest builds and checks the relay manifest: the sanitised
// excerpt of the real server's xray config that the proxy front's relay
// reads. It carries, per inbound, only what the relay needs to open a port —
// listen address, port, protocol, tag and the transparent-proxy markers — and
// nothing that could identify or impersonate the real server: no keys,
// passwords, client ids, outbounds or routing.
//
// The real server produces it (`x-ui relay-manifest`, or the button in
// Settings); the proxy front refuses anything else, so a raw bin/config.json
// never has to travel to a disposable box.
package relaymanifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Version is the manifest format version this package writes and accepts.
const Version = 1

// Marker identifies a document as a relay manifest. Its absence is how a raw
// panel config is told apart from a manifest before any field is inspected.
type Marker struct {
	Version      int    `json:"version"`
	GeneratedAt  string `json:"generatedAt,omitempty"`
	PanelVersion string `json:"panelVersion,omitempty"`
}

// Inbound is the whitelisted view of one xray inbound. The field set is the
// contract with the relay (proxy.panelInbound reads exactly these).
type Inbound struct {
	Listen         string          `json:"listen,omitempty"`
	Port           int             `json:"port"`
	Protocol       string          `json:"protocol"`
	Tag            string          `json:"tag,omitempty"`
	Settings       *Settings       `json:"settings,omitempty"`
	StreamSettings *StreamSettings `json:"streamSettings,omitempty"`
}

// Settings keeps the one inbound setting the relay looks at: followRedirect
// marks a dokodemo-door that only receives kernel-redirected traffic.
type Settings struct {
	FollowRedirect bool `json:"followRedirect,omitempty"`
}

// StreamSettings keeps the one stream setting the relay looks at.
type StreamSettings struct {
	Sockopt *Sockopt `json:"sockopt,omitempty"`
}

// Sockopt carries the TPROXY/REDIRECT marker of a transparent-proxy inbound.
type Sockopt struct {
	Tproxy string `json:"tproxy,omitempty"`
}

// Manifest is the whole document.
type Manifest struct {
	RelayManifest Marker    `json:"relayManifest"`
	Inbounds      []Inbound `json:"inbounds"`
}

// Build extracts a manifest from a raw xray config (the panel's
// bin/config.json). Every field outside the whitelist is dropped; unknown
// fields in the source are ignored, since the source is the full config.
func Build(rawXrayConfig []byte, panelVersion string, now time.Time) (*Manifest, error) {
	var src struct {
		Inbounds []struct {
			Listen   string `json:"listen"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
			Tag      string `json:"tag"`
			Settings struct {
				FollowRedirect bool `json:"followRedirect"`
			} `json:"settings"`
			StreamSettings struct {
				Sockopt struct {
					Tproxy string `json:"tproxy"`
				} `json:"sockopt"`
			} `json:"streamSettings"`
		} `json:"inbounds"`
	}
	if err := json.Unmarshal(rawXrayConfig, &src); err != nil {
		return nil, fmt.Errorf("parse xray config: %w", err)
	}

	m := &Manifest{
		RelayManifest: Marker{
			Version:      Version,
			GeneratedAt:  now.UTC().Format(time.RFC3339),
			PanelVersion: panelVersion,
		},
		Inbounds: make([]Inbound, 0, len(src.Inbounds)),
	}
	for _, in := range src.Inbounds {
		out := Inbound{Listen: in.Listen, Port: in.Port, Protocol: in.Protocol, Tag: in.Tag}
		if in.Settings.FollowRedirect {
			out.Settings = &Settings{FollowRedirect: true}
		}
		if in.StreamSettings.Sockopt.Tproxy != "" {
			out.StreamSettings = &StreamSettings{Sockopt: &Sockopt{Tproxy: in.StreamSettings.Sockopt.Tproxy}}
		}
		m.Inbounds = append(m.Inbounds, out)
	}
	return m, nil
}

// JSON renders the manifest indented, ready to paste or save.
func (m *Manifest) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Hint is appended to every rejection so the operator knows where a manifest
// comes from.
const Hint = "generate one on the real server with `x-ui relay-manifest` (or the button in Settings → Subscription) and use that file instead"

// Validate checks that data is a relay manifest and nothing more: the marker
// must be present with a version this package understands, and every object
// may carry only whitelisted fields. A raw bin/config.json, or a manifest
// hand-edited from one, is rejected naming the first offending field.
func Validate(data []byte) (*Manifest, error) {
	// The marker first: a document without it is not a manifest at all, and
	// "unknown field \"log\"" would be a confusing way to say so.
	var probe struct {
		RelayManifest *Marker `json:"relayManifest"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("not a relay manifest (%v); %s", err, Hint)
	}
	if probe.RelayManifest == nil {
		return nil, fmt.Errorf("not a relay manifest: no \"relayManifest\" marker — this looks like a raw xray config; %s", Hint)
	}
	if probe.RelayManifest.Version != Version {
		return nil, fmt.Errorf("relay manifest version %d is not supported (this build reads version %d); %s", probe.RelayManifest.Version, Version, Hint)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("not a relay manifest: %v — it carries more than the relay needs; %s", err, Hint)
	}
	if len(m.Inbounds) == 0 {
		return nil, fmt.Errorf("relay manifest lists no inbounds; %s", Hint)
	}
	return &m, nil
}

// FromFile builds the manifest from the xray config at path (the panel's
// bin/config.json — what xray actually runs) and returns it rendered. This is
// the one producer behind both `x-ui relay-manifest` and the panel button.
func FromFile(path, panelVersion string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read xray config %q: %w", path, err)
	}
	m, err := Build(raw, panelVersion, time.Now())
	if err != nil {
		return nil, fmt.Errorf("xray config %q: %w", path, err)
	}
	return m.JSON()
}
