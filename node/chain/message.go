package chain

import (
	"crypto/ed25519"
	"fmt"

	"github.com/Compdeep/unloop/node/solana"
)

// Message is a legacy transaction message: every account the instructions
// name, once, the fee payer first; then signers before the rest and writable
// before read-only, each group in the order the accounts first appear.
type Message struct {
	Signers, ReadonlySigners, ReadonlyUnsigned int
	Keys                                       []solana.Key
	Blockhash                                  solana.Key
	Instructions                               []compiled
}

type compiled struct {
	program  uint8
	accounts []uint8
	data     []byte
}

// Compile lays out a message for payer to sign first.
func Compile(payer solana.Key, ixs []Instruction, blockhash solana.Key) (*Message, error) {
	type flags struct{ signer, writable bool }
	seen := map[solana.Key]*flags{}
	var order []solana.Key
	note := func(k solana.Key, signer, writable bool) {
		f, ok := seen[k]
		if !ok {
			f = &flags{}
			seen[k] = f
			order = append(order, k)
		}
		f.signer = f.signer || signer
		f.writable = f.writable || writable
	}
	note(payer, true, true)
	for _, ix := range ixs {
		for _, m := range ix.Accounts {
			note(m.Key, m.Signer, m.Writable)
		}
		note(ix.Program, false, false)
	}

	m := &Message{Blockhash: blockhash}
	for _, group := range []flags{{true, true}, {true, false}, {false, true}, {false, false}} {
		for _, k := range order {
			if *seen[k] == group {
				m.Keys = append(m.Keys, k)
			}
		}
	}
	for _, k := range m.Keys {
		f := seen[k]
		if f.signer {
			m.Signers++
			if !f.writable {
				m.ReadonlySigners++
			}
		} else if !f.writable {
			m.ReadonlyUnsigned++
		}
	}
	if len(m.Keys) > 256 {
		return nil, fmt.Errorf("%d accounts is more than a message can name", len(m.Keys))
	}
	index := map[solana.Key]uint8{}
	for i, k := range m.Keys {
		index[k] = uint8(i)
	}
	for _, ix := range ixs {
		c := compiled{program: index[ix.Program], data: ix.Data}
		for _, a := range ix.Accounts {
			c.accounts = append(c.accounts, index[a.Key])
		}
		m.Instructions = append(m.Instructions, c)
	}
	return m, nil
}

// Bytes is the message as it is signed.
func (m *Message) Bytes() []byte {
	b := []byte{byte(m.Signers), byte(m.ReadonlySigners), byte(m.ReadonlyUnsigned)}
	b = compact(b, len(m.Keys))
	for _, k := range m.Keys {
		b = append(b, k[:]...)
	}
	b = append(b, m.Blockhash[:]...)
	b = compact(b, len(m.Instructions))
	for _, ix := range m.Instructions {
		b = append(b, ix.program)
		b = compact(b, len(ix.accounts))
		b = append(b, ix.accounts...)
		b = compact(b, len(ix.data))
		b = append(b, ix.data...)
	}
	return b
}

// Sign is the transaction, signed by every key the message needs, in the
// order it names them.
func (m *Message) Sign(keys ...ed25519.PrivateKey) ([]byte, error) {
	msg := m.Bytes()
	byKey := map[solana.Key]ed25519.PrivateKey{}
	for _, k := range keys {
		var pub solana.Key
		copy(pub[:], k.Public().(ed25519.PublicKey))
		byKey[pub] = k
	}
	tx := compact(nil, m.Signers)
	for _, signer := range m.Keys[:m.Signers] {
		k, ok := byKey[signer]
		if !ok {
			return nil, fmt.Errorf("%s must sign, and no key for it was given", signer)
		}
		tx = append(tx, ed25519.Sign(k, msg)...)
	}
	return append(tx, msg...), nil
}

// compact is Solana's short vector length: seven bits a byte, low first.
func compact(b []byte, n int) []byte {
	for {
		if n < 0x80 {
			return append(b, byte(n))
		}
		b = append(b, byte(n&0x7f)|0x80)
		n >>= 7
	}
}
