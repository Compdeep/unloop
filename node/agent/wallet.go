package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Compdeep/unloop/node/solana"
)

// The agent's wallet: a Solana keypair in the data directory, in the Solana
// CLI's format (a JSON array of the 64 secret bytes), so `solana` can read it
// and a CLI keypair can be imported. It is the agent's own address: it signs
// the agent's messages and, once the agent works, its transactions. Readable
// by its owner alone.

func walletPath(dir string) string { return filepath.Join(dir, "wallet.json") }

// Wallet reads the agent's keypair.
func Wallet(dir string) (ed25519.PrivateKey, error) { return readKeypair(walletPath(dir)) }

// NewWallet makes a keypair, or imports a Solana CLI keypair file, and keeps
// it. It refuses to replace one already there.
func NewWallet(dir, importFrom string) (ed25519.PrivateKey, error) {
	if _, err := os.Stat(walletPath(dir)); err == nil {
		return nil, fmt.Errorf("%s already holds a wallet; move it away first", walletPath(dir))
	}
	var key ed25519.PrivateKey
	var err error
	if importFrom != "" {
		key, err = readKeypair(importFrom)
	} else {
		_, key, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		return nil, err
	}
	ints := make([]int, len(key))
	for i, b := range key {
		ints[i] = int(b)
	}
	b, _ := json.Marshal(ints)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return key, os.WriteFile(walletPath(dir), b, 0o600)
}

func readKeypair(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ints []int
	if err := json.Unmarshal(b, &ints); err != nil || len(ints) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s is not a Solana keypair", path)
	}
	key := make(ed25519.PrivateKey, len(ints))
	for i, n := range ints {
		if n < 0 || n > 255 {
			return nil, fmt.Errorf("%s is not a Solana keypair", path)
		}
		key[i] = byte(n)
	}
	// The second half is the public key; a file whose halves disagree is not one keypair.
	if !ed25519.NewKeyFromSeed(key.Seed()).Equal(key) {
		return nil, fmt.Errorf("%s does not hold a matching keypair", path)
	}
	return key, nil
}

// Address is a wallet's Solana address.
func Address(key ed25519.PrivateKey) solana.Key {
	var k solana.Key
	copy(k[:], key.Public().(ed25519.PublicKey))
	return k
}
