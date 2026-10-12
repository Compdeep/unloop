package envelope

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mr-tron/base58"
)

// protocol/vectors/envelopes.json, built by chain/tests/envelope.test.ts:
// what the browser signs, which this side must read byte for byte the same.
func vectors(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile("../../protocol/vectors/envelopes.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func TestCanonicalMatchesTheBrowser(t *testing.T) {
	for _, c := range vectors(t)["canonical"].([]any) {
		c := c.(map[string]any)
		v, err := Decode([]byte(c["json"].(string)))
		if err != nil {
			t.Fatalf("%s: %v", c["name"], err)
		}
		got, err := Canonical(v)
		if err != nil {
			t.Fatalf("%s: %v", c["name"], err)
		}
		if string(got) != c["canonical"].(string) {
			t.Errorf("%s:\n got %s\nwant %s", c["name"], got, c["canonical"])
		}
	}
}

func TestCanonicalRefusesWhatHasNoEncoding(t *testing.T) {
	v, err := Decode([]byte(`[1e400]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Canonical(v); err == nil {
		t.Error("1e400 is Infinity in the browser, which RFC 8785 cannot write")
	}
	if _, err := Decode([]byte(`{} {}`)); err == nil {
		t.Error("two values read as one")
	}
}

func TestEveryVectorChecksAsTheBrowserExpects(t *testing.T) {
	vs := vectors(t)
	key := func(i int) ed25519.PrivateKey {
		seed, _ := hex.DecodeString(vs["keys"].([]any)[i].(map[string]any)["seed"].(string))
		return ed25519.NewKeyFromSeed(seed)
	}
	for _, e := range vs["envelopes"].([]any) {
		e := e.(map[string]any)
		name, want := e["name"].(string), e["expect"].(string)
		raw := e["envelope"].(map[string]any)

		// The bytes signed and the id are the browser's, refused or not.
		if got := string(signingBytes(raw)); got != e["signing"].(string) {
			t.Errorf("%s: signing bytes differ:\n got %s\nwant %s", name, got, e["signing"])
		}
		if got := id(raw); got != e["id"].(string) {
			t.Errorf("%s: id %s, want %s", name, got, e["id"])
		}

		wire, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := IDOf(wire); err != nil || got != e["id"].(string) {
			t.Errorf("%s: off the wire, id %s (%v), want %s", name, got, err, e["id"])
		}
		m, err := Check(wire)
		switch want {
		case "ok":
			if err != nil {
				t.Errorf("%s: refused: %v", name, err)
				continue
			}
			if m.ID() != e["id"].(string) || m.Type != raw["type"] {
				t.Errorf("%s: read back as %s %s", name, m.Type, m.ID())
			}
			// Ed25519 is deterministic: signing here gives the browser's signature.
			if got := Sign(raw, key(0))["sig"]; got != raw["sig"] {
				t.Errorf("%s: signed here as %s, in the browser as %s", name, got, raw["sig"])
			}
		case "schema":
			if !errors.Is(err, ErrSchema) {
				t.Errorf("%s: want a schema refusal, got %v", name, err)
			}
		case "signature":
			if !errors.Is(err, ErrSignature) {
				t.Errorf("%s: want a signature refusal, got %v", name, err)
			}
		default:
			t.Fatalf("%s: unknown expectation %q", name, want)
		}
	}
}

// An envelope made here, signed here, checks out as one from a browser does.
func TestNewSignsAndChecks(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	pub := key.Public().(ed25519.PublicKey)
	e, err := New("claim.publish", "protein-design", base58.Encode(pub), "12D3KooWGjMkhTLBFLhvpCnuPRqLCxJZcgqQmLhJzWpKNXVdmTQL",
		&ChainRef{Tx: strings.Repeat("4", 88), Slot: 312884401},
		map[string]any{"objective": "4Nd1mYt8mQuW5Uy7vFkZq2Rj9HhcXeL3sPbTgAoC6wDx", "awardIndex": 0, "claimHash": strings.Repeat("ab", 32),
			"assertion": "x", "significance": "y", "evidence": map[string]any{"deterministic": []any{}, "procedural": []any{},
				"semantic": []any{map[string]any{"id": "S1", "kind": "semantic", "statement": "s", "provenance": "p"}}}, "roots": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(Sign(e, key))
	if _, err := Check(wire); err != nil {
		t.Errorf("refused: %v", err)
	}
	if _, err := New("claim.publish", "c", "a", "p", nil, map[string]any{"x": func() {}}); err == nil {
		t.Error("made an envelope of what JSON cannot hold")
	}
}
