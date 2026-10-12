// Package envelope is a protocol message as a node reads one
// (docs/PLATFORM.md section 6; protocol/schemas/envelope.schema.json):
//
//	signed bytes  Canonical(envelope without "sig")
//	sig           Ed25519 by "author", base58
//	id            hex sha256 of Canonical(envelope): the same message
//	              whoever relays it
//
// Check is the whole of what a message is on its own: well formed against the
// schemas, then signed by its author — in that order, which is the order
// protocol/vectors/envelopes.json expects. Whether the author is the one its
// account names, and whether the type belongs on the topic it came in on, are
// the bus's questions, asked with the chain and the topic in hand.
//
// chain/client/envelope.ts builds the same bytes for a wallet to sign; the
// vectors hold the two together.
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"

	"github.com/Compdeep/unloop/protocol"
	"github.com/mr-tron/base58"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The stage a message was refused at, for errors.Is. A schema refusal is the
// sender's fault whatever the chain says; so is a signature refusal.
var (
	ErrSchema    = errors.New("not a well-formed message")
	ErrSignature = errors.New("not signed by its author")
)

// Message is an envelope that passed Check.
type Message struct {
	raw map[string]any

	Type   string
	Colony string
	Author string
	Peer   string
	// Body is nil for a message that carries a body_ref instead.
	Body map[string]any
}

// Raw is the decoded envelope, signature included.
func (m *Message) Raw() map[string]any { return m.raw }

// SigningBytes is what the author signed.
func (m *Message) SigningBytes() []byte { return signingBytes(m.raw) }

// ID is the message id.
func (m *Message) ID() string { return id(m.raw) }

// Check reads a message off the wire: decoded, held to the schemas, then its
// signature verified under its author's key.
func Check(data []byte) (*Message, error) {
	v, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	if err := validate(v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSchema, err)
	}
	raw := v.(map[string]any) // the schema requires an object
	m := &Message{raw: raw, Type: raw["type"].(string), Colony: raw["colony"].(string), Author: raw["author"].(string), Peer: raw["peer"].(string)}
	m.Body, _ = raw["body"].(map[string]any)

	key, err := base58.Decode(m.Author)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: the author is not an Ed25519 key", ErrSignature)
	}
	sig, err := base58.Decode(raw["sig"].(string))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: the signature is not 64 bytes", ErrSignature)
	}
	if !ed25519.Verify(key, signingBytes(raw), sig) {
		return nil, fmt.Errorf("%w: the signature does not verify", ErrSignature)
	}
	return m, nil
}

// Version is the protocol version the schemas are written at.
const Version = "0.1.0"

// ChainRef is the transaction a message names, where its type must name one.
type ChainRef struct {
	Tx   string `json:"tx"`
	Slot uint64 `json:"slot"`
}

// New is an envelope ready to Sign, as chain/client/envelope.ts's `unsigned`
// makes one: the version, the type, the colony, the author, the peer that
// will publish it, the time to the second, the transaction if given, and the
// body — every value as Decode reads one, so it encodes canonically.
func New(typ, colony, author, peer string, chain *ChainRef, body any) (map[string]any, error) {
	e := map[string]any{
		"v": Version, "type": typ, "colony": colony, "author": author, "peer": peer,
		"ts": time.Now().UTC().Format("2006-01-02T15:04:05Z"), "body": body,
	}
	if chain != nil {
		e["chain"] = chain
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	v, err := Decode(b)
	if err != nil {
		return nil, err
	}
	return v.(map[string]any), nil
}

// Sign returns the envelope, decoded, with "sig" set by key over its signing
// bytes. Ed25519 is deterministic, so the same envelope and key give the
// same signature a wallet would.
func Sign(envelope map[string]any, key ed25519.PrivateKey) map[string]any {
	out := make(map[string]any, len(envelope)+1)
	for k, v := range envelope {
		out[k] = v
	}
	delete(out, "sig")
	out["sig"] = base58.Encode(ed25519.Sign(key, signingBytes(out)))
	return out
}

func signingBytes(raw map[string]any) []byte {
	without := make(map[string]any, len(raw))
	for k, v := range raw {
		if k != "sig" {
			without[k] = v
		}
	}
	b, err := Canonical(without)
	if err != nil {
		panic(err) // a value Decode produced always encodes
	}
	return b
}

// IDOf is the id of a message as it came off the wire, before Check has
// read it: what gossipsub tells one message from another by. It fails only on
// what is not JSON or has no canonical form, and such a message fails Check.
func IDOf(data []byte) (string, error) {
	v, err := Decode(data)
	if err != nil {
		return "", err
	}
	b, err := Canonical(v)
	if err != nil {
		return "", err
	}
	return digest(b), nil
}

func id(raw map[string]any) string {
	b, err := Canonical(raw)
	if err != nil {
		panic(err)
	}
	return digest(b)
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// The envelope schema, compiled once from the embedded files.
var (
	compiled    *jsonschema.Schema
	compileErr  error
	compileOnce sync.Once
)

func validate(v any) error {
	compileOnce.Do(func() { compiled, compileErr = compile() })
	if compileErr != nil {
		return compileErr
	}
	return compiled.Validate(v)
}

func compile() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	files, err := fs.Glob(protocol.Schemas, "schemas/*.schema.json")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, err := fs.ReadFile(protocol.Schemas, f)
		if err != nil {
			return nil, err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		id, _ := doc.(map[string]any)["$id"].(string)
		if id == "" {
			return nil, fmt.Errorf("%s has no $id", f)
		}
		if err := c.AddResource(id, doc); err != nil {
			return nil, err
		}
	}
	return c.Compile(protocol.Envelope)
}
