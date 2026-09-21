package chain

import (
	"encoding/json"
	"reflect"
	"testing"
)

// innerOneExample is the inner-1 document of docs/spec/proxy-chain.md §3.2,
// byte for byte in field names: a decode/encode round trip that loses or
// renames a field is the bug this test exists to catch, because every box in
// the chain parses exactly this JSON.
const innerOneExample = `{"version":1,"revision":42,"generatedAt":1758380000000,
 "self":{"name":"inner-1","role":"inner","host":"10.0.0.7"},
 "nextHop":{"host":"198.51.100.1","subPort":2096,"subScheme":"https",
            "subPath":"/sub/","jsonPath":"/json/","tunPath":"/tun/"},
 "activeEdge":"edge-a",
 "hops":[
   {"name":"inner-1","role":"inner","host":"10.0.0.7","subPort":2096,"secretHash":"3b1f","state":"joined"},
   {"name":"inner-2","role":"inner","host":"203.0.113.9","subPort":2096,"secretHash":"9c4a","state":"joined"},
   {"name":"edge-a","role":"edge","host":"a.example.net","subPort":2096,"secretHash":"77de","state":"joined"},
   {"name":"edge-b","role":"edge","host":"b.example.net","subPort":2096,"secretHash":"01bc","state":"joined"}],
 "ports":[
   {"port":443,"network":"tcp,udp","tag":"inbound-443","source":"xray"},
   {"port":8443,"network":"tcp,udp","tag":"inbound-trojan","source":"xray"},
   {"port":51820,"network":"udp","tag":"awg","source":"awg"},
   {"port":51821,"network":"udp","tag":"wg","source":"wg"},
   {"port":9443,"network":"tcp","tag":"mtproto-17","source":"mtproto"},
   {"port":8080,"network":"tcp","tag":"extra-8080","source":"extra"}]}`

func TestDocumentRoundTripsSpecExample(t *testing.T) {
	var doc Document
	if err := json.Unmarshal([]byte(innerOneExample), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if doc.Version != DocumentVersion || doc.Revision != 42 || doc.GeneratedAt != 1758380000000 {
		t.Fatalf("header: %+v", doc)
	}
	if doc.Self != (Self{Name: "inner-1", Role: RoleInner, Host: "10.0.0.7"}) {
		t.Fatalf("self: %+v", doc.Self)
	}
	want := NextHop{Host: "198.51.100.1", SubPort: 2096, SubScheme: "https",
		SubPath: "/sub/", JsonPath: "/json/", TunPath: "/tun/"}
	if doc.NextHop != want {
		t.Fatalf("nextHop: %+v", doc.NextHop)
	}
	if doc.ActiveEdge != "edge-a" {
		t.Fatalf("activeEdge: %q", doc.ActiveEdge)
	}
	if len(doc.Hops) != 4 || doc.Hops[3] != (Hop{Name: "edge-b", Role: RoleEdge, Host: "b.example.net",
		SubPort: 2096, SecretHash: "01bc", State: StateJoined}) {
		t.Fatalf("hops: %+v", doc.Hops)
	}
	if len(doc.Ports) != 6 || doc.Ports[2] != (Port{Port: 51820, Network: NetworkUDP, Tag: "awg", Source: SourceAwg}) {
		t.Fatalf("ports: %+v", doc.Ports)
	}

	// Re-encoding must produce the same document, field for field.
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got, expected map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal re-encoded: %v", err)
	}
	if err := json.Unmarshal([]byte(innerOneExample), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("round trip changed the document:\n got %s\nwant %s", encoded, innerOneExample)
	}
}

// An edge that is not the active one gets no activeEdge field at all (§3.2),
// so the field has to disappear rather than serialise as "".
func TestDocumentOmitsEmptyActiveEdge(t *testing.T) {
	doc := Document{Version: DocumentVersion, Revision: 7,
		Self:    Self{Name: "edge-b", Role: RoleEdge, Host: "b.example.net"},
		NextHop: NextHop{Host: "203.0.113.9", SubPort: 2096, SubScheme: "https"},
		Hops:    []Hop{{Name: "edge-b", Role: RoleEdge, Host: "b.example.net", SubPort: 2096, State: StateJoined}},
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["activeEdge"]; ok {
		t.Fatalf("activeEdge must be omitted when empty: %s", encoded)
	}
	for _, key := range []string{"version", "revision", "generatedAt", "self", "nextHop", "hops", "ports"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("field %q missing: %s", key, encoded)
		}
	}
}

func TestStatusFieldNames(t *testing.T) {
	const example = `{"version":1,"name":"inner-2","role":"inner","revision":42,
 "lastPoll":1758379990000,"lastOk":1758379990000,"stale":false,
 "relay":{"running":true,"ports":[443,8443],"restartedAt":1758300000000},
 "nextHop":{"host":"10.0.0.7","subPort":2096,"reachable":true},
 "draining":false,"observedHostMismatch":false}`

	var st Status
	if err := json.Unmarshal([]byte(example), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.Version != DocumentVersion || st.Name != "inner-2" || st.Role != RoleInner || st.Revision != 42 {
		t.Fatalf("status header: %+v", st)
	}
	if st.LastPoll != 1758379990000 || st.LastOk != 1758379990000 || st.Stale {
		t.Fatalf("status freshness: %+v", st)
	}
	if !st.Relay.Running || len(st.Relay.Ports) != 2 || st.Relay.RestartedAt != 1758300000000 {
		t.Fatalf("relay: %+v", st.Relay)
	}
	if st.NextHop != (StatusNextHop{Host: "10.0.0.7", SubPort: 2096, Reachable: true}) {
		t.Fatalf("nextHop: %+v", st.NextHop)
	}
	if st.ObservedHostMismatch {
		t.Fatalf("observedHostMismatch: %+v", st)
	}

	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var got, expected map[string]any
	_ = json.Unmarshal(encoded, &got)
	_ = json.Unmarshal([]byte(example), &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("round trip changed the status:\n got %s\nwant %s", encoded, example)
	}
}
