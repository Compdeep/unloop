// Package solana is the chain as the bus asks it (docs/PLATFORM.md section 6):
// a read-only RPC client, program-derived addresses, and Program, which
// answers bus.Accounts from the Unloop program's accounts.
package solana

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
	"github.com/mr-tron/base58"
)

// Key is a Solana address.
type Key [32]byte

// ParseKey reads a base58 address.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base58.Decode(s)
	if err != nil || len(b) != len(k) {
		return k, fmt.Errorf("%q is not an address", s)
	}
	copy(k[:], b)
	return k, nil
}

func (k Key) String() string { return base58.Encode(k[:]) }

// FindAddress is the program-derived address for seeds, as the program's
// `seeds = [...]` finds it: the highest bump whose hash is off the curve.
func FindAddress(program Key, seeds ...[]byte) (Key, error) {
	for bump := 255; bump >= 0; bump-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(bump)})
		h.Write(program[:])
		h.Write([]byte("ProgramDerivedAddress"))
		var k Key
		copy(k[:], h.Sum(nil))
		if _, err := new(edwards25519.Point).SetBytes(k[:]); err != nil {
			return k, nil
		}
	}
	return Key{}, errors.New("no bump puts the address off the curve")
}
