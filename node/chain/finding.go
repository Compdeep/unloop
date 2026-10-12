package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/Compdeep/unloop/node/envelope"
)

// The words a validation.publish message uses, in the order the program's
// enums number them (chain/client/finding.ts).
var (
	Reasons      = []string{"none", "refused", "unavailable", "access_denied", "timed_out", "unsupported", "environment", "resource_limit", "internal"}
	Findings     = []string{"pass", "fail", "insufficient"}
	Reproduction = []string{"not_attempted", "pass", "fail"}
)

// Finding is the part of a validation.publish body the chain sees: the
// enums and numbers it counts, and the hash of the detail it does not.
type Finding struct {
	Reason, ClaimSupport, ObjectiveAlignment, Reproduction string
	NoveltyBps, ScoreBps                                   uint16
	Finding                                                [32]byte
}

// ReadFinding takes a validation.publish body, decoded as envelope.Decode
// decodes one.
func ReadFinding(body map[string]any) (Finding, error) {
	f := Finding{}
	text := func(k string) string { s, _ := body[k].(string); return s }
	f.Reason, f.ClaimSupport, f.ObjectiveAlignment, f.Reproduction =
		text("reason"), text("claimSupport"), text("objectiveAlignment"), text("reproduction")
	for k, dst := range map[string]*uint16{"noveltyBps": &f.NoveltyBps, "scoreBps": &f.ScoreBps} {
		n, err := number(body[k])
		if err != nil || n < 0 || n > 10000 {
			return f, fmt.Errorf("%s is not a basis-point figure", k)
		}
		*dst = uint16(n)
	}
	h, err := hex.DecodeString(text("finding"))
	if err != nil || len(h) != 32 {
		return f, fmt.Errorf("finding is not a 32-byte hash")
	}
	copy(f.Finding[:], h)
	return f, nil
}

func number(v any) (int64, error) {
	switch n := v.(type) {
	case interface{ Int64() (int64, error) }:
		return n.Int64()
	case float64:
		return int64(n), nil
	}
	return 0, fmt.Errorf("not a number")
}

// FindingHash is the hash a finding commits its detail by:
// sha256(canonical({items, predicates, rationale})), each only if present.
func FindingHash(body map[string]any) (string, error) {
	detail := map[string]any{}
	for _, k := range []string{"items", "predicates", "rationale"} {
		if v, ok := body[k]; ok {
			detail[k] = v
		}
	}
	b, err := envelope.Canonical(detail)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// enum is a word's position in its list, which is the program's number for it.
func enum(list []string, v, what string) (byte, error) {
	i := slices.Index(list, v)
	if i < 0 {
		return 0, fmt.Errorf("%s is %q, not one of %v", what, v, list)
	}
	return byte(i), nil
}

// fields are the finding's enums in the order the program reads them.
func (f Finding) fields() ([4]byte, error) {
	var out [4]byte
	var err error
	for i, x := range []struct {
		list    []string
		v, what string
	}{{Reasons, f.Reason, "reason"}, {Findings, f.ClaimSupport, "claimSupport"}, {Findings, f.ObjectiveAlignment, "objectiveAlignment"}, {Reproduction, f.Reproduction, "reproduction"}} {
		if out[i], err = enum(x.list, x.v, x.what); err != nil {
			return out, err
		}
	}
	return out, nil
}

// Commitment is Validation::commitment_of: what a validator commits to before
// anybody's finding is open.
func Commitment(claim, validator [32]byte, f Finding, nonce [32]byte) ([32]byte, error) {
	e, err := f.fields()
	if err != nil {
		return [32]byte{}, err
	}
	b := append(append([]byte{}, claim[:]...), validator[:]...)
	b = append(b, e[0], e[1], e[2])
	b = binary.LittleEndian.AppendUint16(b, f.NoveltyBps)
	b = binary.LittleEndian.AppendUint16(b, f.ScoreBps)
	b = append(b, e[3])
	b = append(b, f.Finding[:]...)
	b = append(b, nonce[:]...)
	return sha256.Sum256(b), nil
}

// ClaimHash is a claim's hash under its objective's mode
// (chain/client/claim-body.ts): under deterministic, of the assertion, which
// is the canonical result; under the others, of the body canonically
// encoded without its own claimHash.
func ClaimHash(body map[string]any, mode string) (string, error) {
	var text []byte
	if mode == "deterministic" {
		a, ok := body["assertion"].(string)
		if !ok {
			return "", fmt.Errorf("a deterministic claim's assertion is its canonical result, and must be text")
		}
		text = []byte(a)
	} else {
		rest := make(map[string]any, len(body))
		for k, v := range body {
			if k != "claimHash" {
				rest[k] = v
			}
		}
		var err error
		if text, err = envelope.Canonical(rest); err != nil {
			return "", err
		}
	}
	h := sha256.Sum256(text)
	return hex.EncodeToString(h[:]), nil
}
