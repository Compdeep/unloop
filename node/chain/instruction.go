// Package chain is the Unloop program's instructions as the agent sends them
// (docs/PLATFORM.md section 7, "Transactions in Go"), and the transactions
// that carry them.
//
// chain/client (TypeScript) is the other implementation, and the first: what
// the site's wallet signs. protocol/vectors/transactions.json holds the two
// together — every instruction here, built from the same accounts, must come
// out byte for byte as chain/client builds it.
//
// Instructions are built from what the accounts say, read through
// solana.Program; nothing here holds a key. Sending signs with the key it is
// given and nothing else.
package chain

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/Compdeep/unloop/node/solana"
)

// Meta is one account an instruction names.
type Meta struct {
	Key      solana.Key
	Signer   bool
	Writable bool
}

// Instruction is one call to a program.
type Instruction struct {
	Program  solana.Key
	Accounts []Meta
	Data     []byte
}

// Programs the agent's instructions name besides Unloop's.
var (
	SystemProgram          solana.Key // all zeros: 11111111111111111111111111111111
	AssociatedTokenProgram = mustKey("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	SlotHashes             = mustKey("SysvarS1otHashes111111111111111111111111111")
)

func mustKey(s string) solana.Key {
	k, err := solana.ParseKey(s)
	if err != nil {
		panic(err)
	}
	return k
}

// discriminator is Anchor's: the first eight bytes of sha256("global:<name>").
func discriminator(name string) []byte {
	h := sha256.Sum256([]byte("global:" + name))
	return h[:8]
}

// args is an instruction's data in Borsh, after its discriminator.
type args []byte

func call(name string) args { return append(args{}, discriminator(name)...) }

func (a args) u8(v uint8) args     { return append(a, v) }
func (a args) u16(v uint16) args   { return binary.LittleEndian.AppendUint16(a, v) }
func (a args) u32(v uint32) args   { return binary.LittleEndian.AppendUint32(a, v) }
func (a args) u64(v uint64) args   { return binary.LittleEndian.AppendUint64(a, v) }
func (a args) bytes(b []byte) args { return append(a, b...) }

func w(k solana.Key) Meta  { return Meta{Key: k, Writable: true} }
func r(k solana.Key) Meta  { return Meta{Key: k} }
func ws(k solana.Key) Meta { return Meta{Key: k, Signer: true, Writable: true} }
func rs(k solana.Key) Meta { return Meta{Key: k, Signer: true} }

// openATA is the associated token program's create-idempotent: owner's
// account for mint, opened if it is not there, paid by payer.
func openATA(payer, owner, mint, tokenProgram solana.Key) (solana.Key, Instruction) {
	address, err := solana.FindAddress(AssociatedTokenProgram, owner[:], tokenProgram[:], mint[:])
	if err != nil {
		panic(err)
	}
	return address, Instruction{Program: AssociatedTokenProgram, Data: []byte{1}, Accounts: []Meta{
		ws(payer), w(address), r(owner), r(mint), r(SystemProgram), r(tokenProgram),
	}}
}

// ata is owner's associated account for mint.
func ata(owner, mint, tokenProgram solana.Key) solana.Key {
	address, _ := openATA(owner, owner, mint, tokenProgram)
	return address
}
