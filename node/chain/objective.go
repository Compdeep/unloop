package chain

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Compdeep/unloop/node/envelope"
	"github.com/Compdeep/unloop/node/solana"
)

// SpecHashes is an objective document's two commitments
// (chain/client/spec.ts, specHashes; docs/PROTOCOL.md rule 2.1):
//
//	criteria_hash   = sha256(canonical({ title, statement, validationMode,
//	                                      predicates, composition, awards,
//	                                      samples: samples.items }))
//	claim_form_hash = sha256(canonical(claimForm))
//
// with each award's definition only, and an absent field left out.
func SpecHashes(body map[string]any) (criteria, claimForm string, err error) {
	c := map[string]any{}
	for _, k := range []string{"title", "statement", "validationMode", "predicates", "composition"} {
		if v, ok := body[k]; ok {
			c[k] = v
		}
	}
	if list, ok := body["awards"].([]any); ok {
		awards := make([]any, len(list))
		for i, x := range list {
			a, _ := x.(map[string]any)
			def := map[string]any{}
			for _, k := range []string{"index", "title", "criteria"} {
				if v, ok := a[k]; ok {
					def[k] = v
				}
			}
			awards[i] = def
		}
		c["awards"] = awards
	}
	if s, ok := body["samples"].(map[string]any); ok {
		if items, ok := s["items"]; ok {
			c["samples"] = items
		}
	}
	form, ok := body["claimForm"]
	if !ok {
		return "", "", errors.New("the document has no claimForm")
	}
	hash := func(v any) (string, error) {
		b, err := envelope.Canonical(v)
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:]), err
	}
	if criteria, err = hash(c); err != nil {
		return "", "", err
	}
	claimForm, err = hash(form)
	return criteria, claimForm, err
}

// Award is what one award pays: the money beside its text, which the
// document holds and the account never does.
type Award struct {
	Mode       string // winnerTakeAll or graded
	Allocation uint64
}

// ObjectiveDraft is an objective as its leader creates it (chain/client
// objective-actions.ts, Draft): the objective.publish body, id included, and
// what the chain holds beside it.
type ObjectiveDraft struct {
	Body           map[string]any
	Awards         []Award // in the body's awards' order
	CommitDeadline int64   // unix seconds; the document does not carry it
	Pool           uint64  // put in at creation from the leader's own account
	URI            string  // where the body can be fetched; empty for bus-only
}

// MaxURIBytes is the program's MAX_URI_LEN.
const MaxURIBytes = 200

// Next is the address a colony's next objective will have, which its
// document names before it exists.
func (c *Client) Next(ctx context.Context, colony solana.Key) (solana.Key, error) {
	col, err := c.colony(ctx, colony)
	if err != nil {
		return solana.Key{}, err
	}
	return c.next(col), nil
}

func (c *Client) next(col *solana.Colony) solana.Key {
	return c.seed([]byte("objective"), col.Address[:], binary.LittleEndian.AppendUint64(nil, col.ObjectiveCount))
}

func (c *Client) colony(ctx context.Context, address solana.Key) (*solana.Colony, error) {
	col, err := c.program.Colony(ctx, address)
	if err == nil && col == nil {
		err = fmt.Errorf("no colony at %s", address)
	}
	return col, err
}

// Create is create_objective for d at objective, which must be the colony's
// next: the terms are derived from the document, so the hashes signed are
// its own and every field the account copies agrees with it. A document with
// worked samples is created on the site, which keeps their results.
func (c *Client) Create(ctx context.Context, colony, leader, objective solana.Key, d ObjectiveDraft) ([]Instruction, error) {
	b := d.Body
	if id, _ := b["id"].(string); id != objective.String() {
		return nil, fmt.Errorf("the document names %q, not %s", id, objective)
	}
	if _, ok := b["samples"]; ok {
		return nil, errors.New("an objective with worked samples is created on the site, which keeps their results")
	}
	payouts := map[string]byte{"prize": 0, "rate": 1}
	payout, ok := payouts[fmt.Sprint(b["payout"])]
	if !ok {
		return nil, errors.New("the document's payout must be prize or rate")
	}
	panel, err := number(b["validatorsPerClaim"])
	if err != nil || panel < 1 || panel > 255 {
		return nil, errors.New("the document must say how many validators a claim selects, 1 to 255")
	}
	claimDeadline, err1 := unix(b["claimDeadline"])
	validationDeadline, err2 := unix(b["validationDeadline"])
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("the document must carry both deadlines: %w", err)
	}
	if list, _ := b["awards"].([]any); len(list) != len(d.Awards) {
		return nil, errors.New("every award needs its text in the document and its amount here")
	}
	if len(d.URI) > MaxURIBytes {
		return nil, fmt.Errorf("a URI is at most %d bytes; this one is %d", MaxURIBytes, len(d.URI))
	}
	criteria, form, err := SpecHashes(b)
	if err != nil {
		return nil, err
	}
	if declared, ok := b["criteriaHash"]; ok && declared != criteria {
		return nil, errors.New("the document's criteriaHash is not the hash of its criteria")
	}
	col, err := c.colony(ctx, colony)
	if err != nil {
		return nil, err
	}
	if next := c.next(col); next != objective {
		return nil, fmt.Errorf("the colony's next objective is %s, not %s", next, objective)
	}
	tokenProgram, err := c.program.Owner(ctx, col.PaymentMint)
	if err != nil {
		return nil, err
	}

	modes := map[string]byte{"winnerTakeAll": 0, "graded": 1}
	ch, _ := hex.DecodeString(criteria)
	fh, _ := hex.DecodeString(form)
	data := call("create_objective").u8(payout).bytes(ch).bytes(fh).bytes(make([]byte, 32)).u32(0).
		u32(uint32(len(d.URI))).bytes([]byte(d.URI)).u32(uint32(len(d.Awards)))
	for i, a := range d.Awards {
		mode, ok := modes[a.Mode]
		if !ok {
			return nil, fmt.Errorf("award %d: the modes are winnerTakeAll and graded", i)
		}
		data = data.u8(mode).u64(a.Allocation)
	}
	data = data.u8(uint8(panel)).u64(uint64(claimDeadline)).u64(uint64(d.CommitDeadline)).u64(uint64(validationDeadline)).u64(d.Pool)
	return []Instruction{{Program: c.program.ID(), Data: data, Accounts: []Meta{
		ws(leader), w(colony), w(objective), w(c.seed([]byte("vault"), objective[:])), r(col.PaymentMint),
		w(ata(leader, col.PaymentMint, tokenProgram)), r(tokenProgram), r(SystemProgram),
	}}}, nil
}

// unix is an RFC 3339 time as the unix seconds an account stores it as.
func unix(v any) (int64, error) {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}
