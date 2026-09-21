package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/coinman-dev/3ax-ui/v2/util/random"
)

// SecretLength is the length of a join token and of a hop secret: 32
// characters from random.Seq, the same shape as monToken and the panel's other
// secrets (§4.1).
const SecretLength = 32

// nameRe is the hop name format (§2.1). Names address a hop in the bot, in the
// UI and in the monitoring path, so they stay short, lowercase and free of
// anything that would need escaping.
var nameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// NewSecret returns a fresh join token or hop secret in the clear. The caller
// shows it exactly once and stores only HashSecret of it.
func NewSecret() string {
	return random.Seq(SecretLength)
}

// HashSecret is the sha256 hex digest the registry and the document carry in
// place of a secret. Lowercase hex, 64 characters — the box computes the same
// string and both sides compare it verbatim.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NameValid reports whether name is a legal hop name.
func NameValid(name string) bool {
	return nameRe.MatchString(name)
}
