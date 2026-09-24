package tunnel

import "fmt"

// The tunnel MTU has to leave room for everything the tunnel wraps around an
// inner packet, or full-size packets (a TLS certificate flight, for one) exceed
// the 1500-byte link and are lost while the handshake and small packets get
// through — TCP connects and TLS stalls.
//
// An encapsulated data packet on the wire is
//
//	outer IP header   20 (IPv4) or 40 (IPv6)
//	UDP header         8
//	S4 padding        S4 random bytes in front of the message (AmneziaWG 2.0+)
//	message header    16 (type, receiver index, counter)
//	inner packet      <= MTU, rounded up to 16 but never past the MTU
//	auth tag          16 (Poly1305)
//
// Nothing else in the AmneziaWG parameter set grows a data packet: S1–S3 pad
// the handshake and cookie messages, H1–H4 only change the value of the 4-byte
// type field, Jc/Jmin/Jmax and I1–I5 are separate datagrams sent around the
// handshake, and HeaderProtectionKey carries its nonce inside the S4 padding.
// The 3.x ContentPaddingAddition and RandomTrailers do add bytes to data
// packets, but only up to the peer's "UDP window" — the largest data packet
// seen in either direction — so they never make a packet larger than a
// full-size one already is (amneziawg-go device/send.go randomPaddingAddition
// and randomTrailer, kernel module send.c encrypt_packet).
const (
	// LinkMTU is the underlay MTU the defaults are computed for: plain
	// Ethernet, which is what a VPS uplink is.
	LinkMTU = 1500

	// LegacyDefaultMTU is WireGuard's own default (1500 - 80) and what the
	// panel stored for every server before the S4 padding was accounted for.
	// A record carrying it never had its MTU chosen deliberately.
	LegacyDefaultMTU = 1420

	udpHeaderLen       = 8
	dataHeaderLen      = 16
	authTagLen         = 16
	ipv4HeaderLen      = 20
	ipv6HeaderLen      = 40
	dataPacketOverhead = udpHeaderLen + dataHeaderLen + authTagLen // 40
)

// transportPadding is the S4 padding a flavour actually puts on data packets:
// none for native WireGuard, whatever the record says.
func transportPadding(k Kind, server *Server) int {
	if !k.Obfuscation || server.S4 < 0 {
		return 0
	}
	return server.S4
}

// DefaultMTU is the MTU the panel gives a tunnel whose data packets carry s4
// bytes of padding: the largest inner packet that still fits a 1500-byte link
// when the outer header is IPv6, i.e. 1500 - 40 - 8 - 32 - s4 = 1420 - s4.
//
// IPv6 is assumed because the panel cannot know which family a client reaches
// the server over — the endpoint may be a hostname with an AAAA record, and the
// server may be dual-stack — and it is the same assumption WireGuard's own 1420
// makes. Over IPv4 this leaves 20 bytes unused, which costs nothing noticeable;
// guessing IPv4 and being wrong loses every full-size packet.
func DefaultMTU(s4 int) int {
	return LinkMTU - ipv6HeaderLen - dataPacketOverhead - max(s4, 0)
}

// MaxMTU is the largest MTU that can work at all with s4 bytes of padding: a
// full-size packet over IPv4 exactly fills a 1500-byte link, 1440 - s4. Anything
// above it is lost on every path, so the panel refuses to save it.
func MaxMTU(s4 int) int {
	return LinkMTU - ipv4HeaderLen - dataPacketOverhead - max(s4, 0)
}

// FollowDefaultMTU returns the MTU a server should carry once its padding
// changes from prevS4 to s4. A value the panel picked — the legacy 1420, the
// default for the previous padding, or none at all while padding is on —
// follows the padding to DefaultMTU(s4). A value the operator chose is kept.
//
// "None at all" matters because awg-quick fills an unset MTU with the route's
// MTU minus 80, which knows nothing about S4. Without padding that is exactly
// right, so an unset MTU stays unset there.
func FollowDefaultMTU(mtu, prevS4, s4 int) int {
	if mtu <= 0 {
		if s4 > 0 {
			return DefaultMTU(s4)
		}
		return mtu
	}
	if mtu == LegacyDefaultMTU || mtu == DefaultMTU(prevS4) {
		return DefaultMTU(s4)
	}
	return mtu
}

// ValidateMTU rejects an MTU that cannot fit the link with the server's padding
// on top: every full-size packet would be dropped, which shows up far from here
// as connections that open and then hang.
func ValidateMTU(k Kind, server *Server) error {
	s4 := transportPadding(k, server)
	if limit := MaxMTU(s4); server.MTU > limit {
		return fmt.Errorf(
			"MTU %d is too large: with S4 = %d a full-size packet is %d bytes on the wire, over the %d-byte link; use at most %d (%d if clients may connect over IPv6)",
			server.MTU, s4, server.MTU+ipv4HeaderLen+dataPacketOverhead+s4, LinkMTU, limit, DefaultMTU(s4))
	}
	return nil
}

// FollowServerMTU applies FollowDefaultMTU to a server record whose padding was
// prevS4 before this change.
func FollowServerMTU(k Kind, server *Server, prevS4 int) {
	if !k.Obfuscation {
		prevS4 = 0
	}
	server.MTU = FollowDefaultMTU(server.MTU, prevS4, transportPadding(k, server))
}
