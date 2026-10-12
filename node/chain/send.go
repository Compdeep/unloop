package chain

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Compdeep/unloop/node/solana"
)

// Landed is a transaction on chain: its signature and the slot it landed in,
// which is what a message names as its `chain`.
type Landed struct {
	Signature string
	Slot      uint64
}

// Send signs instructions as signer, the fee payer, runs them first so
// nothing the program would refuse is sent, sends them, and waits for them
// to be confirmed. It returns where they landed, or the program's own reason.
func (c *Client) Send(ctx context.Context, signer ed25519.PrivateKey, ixs []Instruction) (Landed, error) {
	var payer solana.Key
	copy(payer[:], signer.Public().(ed25519.PublicKey))
	blockhash, lastValid, err := c.rpc.LatestBlockhash(ctx)
	if err != nil {
		return Landed{}, err
	}
	m, err := Compile(payer, ixs, blockhash)
	if err != nil {
		return Landed{}, err
	}
	tx, err := m.Sign(signer)
	if err != nil {
		return Landed{}, err
	}
	if failure, logs, err := c.rpc.Simulate(ctx, tx); err != nil {
		return Landed{}, err
	} else if failure != nil {
		return Landed{}, reason(logs, string(failure))
	}
	sig, err := c.rpc.Send(ctx, tx)
	if err != nil {
		return Landed{}, err
	}
	for {
		confirmed, slot, failure, err := c.rpc.Status(ctx, sig)
		if err != nil {
			return Landed{Signature: sig}, err
		}
		if failure != nil {
			return Landed{Signature: sig}, reason(nil, string(failure))
		}
		if confirmed {
			return Landed{Signature: sig, Slot: slot}, nil
		}
		if h, err := c.rpc.BlockHeight(ctx); err == nil && h > lastValid {
			return Landed{Signature: sig}, errors.New("the transaction expired before it landed")
		}
		select {
		case <-ctx.Done():
			return Landed{Signature: sig}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

var (
	anchorMessage = regexp.MustCompile(`Error Message: (.*)$`)
	programError  = regexp.MustCompile(`^Program log: Error: (.*)$`)
)

// reason is a failure in the words of whichever program refused it, as the
// site words it (web/src/lib/solana.js): Anchor's error message, the token
// program's, an account that exists already; the raw error when none said.
func reason(logs []string, failure string) error {
	for _, l := range logs {
		if m := anchorMessage.FindStringSubmatch(l); m != nil {
			return errors.New(sentence(m[1]))
		}
		if m := programError.FindStringSubmatch(l); m != nil {
			return errors.New(sentence(m[1]))
		}
		if strings.HasSuffix(l, "already in use") {
			return errors.New("An account this creates already exists, so this was done before.")
		}
	}
	return fmt.Errorf("the transaction failed: %s", failure)
}

func sentence(s string) string {
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
}
