package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/chain"
	"github.com/Compdeep/unloop/node/envelope"
	"github.com/Compdeep/unloop/node/solana"
)

// Settling a claim from the agent (docs/PLATFORM.md section 8, step 1): what
// the site's claim page does (ClaimSettle), so a round can finish without a
// browser. A claim is resolved, an admitted one paid into its agent's escrow,
// and each confirmed edge minted at both ends — in that order, each only once
// the one before has landed, because each reads what the last one wrote.
//
// One action carries out whatever is due when it runs. None of it spends:
// the agent pays the transactions and any token account it opens, and the
// program computes every amount. Who may send each, and when, is the
// program's; every transaction is run first, so a refusal comes back in the
// program's own words before anything is sent.

func (w *worker) settleTool() toolapi.Tool {
	return working{tool{"unloop_settle",
		"Settle a claim: resolve it once its validation window has closed (or, as the objective's settlement authority, once every commitment is open), pay an admitted claim's award into its agent's escrow, and mint the protocol token at both ends of each edge confirmed and not yet paid. Does whichever of these is due, in order; anybody may. Only the settlement authority's admit, figures and confirmed roots count, and the figures only where the validators did not decide. The agent collects its award after with unloop_collect.",
		`{"type":"object","properties":{"claim":{"type":"string"},
"admit":{"type":"boolean","description":"settlement authority only: the verdict where the validators did not reach one"},
"noveltyBps":{"type":"integer","minimum":0,"maximum":10000,"description":"settlement authority only, where the validators did not decide"},
"scoreBps":{"type":"integer","minimum":0,"maximum":10000,"description":"settlement authority only, where the validators did not decide"},
"rootsConfirmed":{"type":"array","items":{"type":"integer"},"description":"settlement authority only: the positions, from 0, of the declared roots the claim was found to be built on; each mints the protocol token at both ends once paid"}},
"required":["claim"]}`,
		w.settleClaim}, toolapi.ImpactControl}
}

// settleParams is what an approved settle action carries.
type settleParams struct {
	Claim      string            `json:"claim"`
	Settlement *chain.Settlement `json:"settlement,omitempty"`
}

func (w *worker) settleClaim(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	claim, err := solana.ParseKey(a.text("claim"))
	if err != nil {
		return fail("settle", err)
	}
	cl, o, err := w.claimAndObjective(ctx, claim)
	if err != nil {
		return fail("settle", err)
	}
	authority := o.SettlementAuthority == w.me
	p := settleParams{Claim: claim.String()}
	var plan []string
	switch {
	case cl.Status == "pending" && authority:
		s := chain.Settlement{Admit: a.flag("admit", false)}
		for name, dst := range map[string]*uint16{"noveltyBps": &s.NoveltyBps, "scoreBps": &s.ScoreBps} {
			n := a.number(name, 0)
			if n < 0 || n > 10000 {
				return fail("settle", fmt.Errorf("%s is basis points, 0 to 10000", name))
			}
			*dst = uint16(n)
		}
		positions, _ := a["rootsConfirmed"].([]any)
		for _, v := range positions {
			i, ok := v.(float64)
			if !ok || i < 0 || int(i) >= len(cl.Roots) || i != float64(int(i)) {
				return fail("settle", fmt.Errorf("this claim declares %d roots; %v is not one of their positions", len(cl.Roots), v))
			}
			s.RootsConfirmed |= 1 << int(i)
		}
		p.Settlement = &s
		verdict := "reject"
		if s.Admit {
			verdict = "admit"
		}
		plan = append(plan, fmt.Sprintf("resolve it as the settlement authority: as its validators decided where they did, otherwise %s at novelty %d and score %d (basis points); %d of %d roots confirmed",
			verdict, s.NoveltyBps, s.ScoreBps, ones(s.RootsConfirmed), len(cl.Roots)))
	case cl.Status == "pending":
		if now := time.Now().Unix(); now < o.ValidationDeadline {
			return fail("settle", fmt.Errorf("not yet: its validation window closes at %s, and until then only the settlement authority, %s, may resolve it", isoTime(o.ValidationDeadline), o.SettlementAuthority))
		}
		plan = append(plan, "resolve it as its validators decided, or reject it if they did not and the day's grace after the window has passed")
	}
	if cl.Status == "pending" || cl.Status == "admitted" && !cl.Awarded {
		plan = append(plan, "pay its award into its agent's escrow, if it is admitted")
	}
	for i := range cl.Roots {
		if cl.Payable(i) || p.Settlement != nil && p.Settlement.RootsConfirmed&(1<<i) != 0 {
			plan = append(plan, fmt.Sprintf("mint the protocol token at both ends of its edge to root %d", i))
		}
	}
	if len(plan) == 0 {
		done := fmt.Sprintf("Nothing is due on claim %s: it is %s", claim, cl.Status)
		if cl.Awarded {
			done += ", its award is in its agent's escrow"
			if cl.Agent == w.me {
				done += " (this agent's: collect it with unloop_collect)"
			}
		}
		return ok("settle", done+", and no confirmed edge is unpaid.")
	}
	return w.propose("settle", toolapi.ImpactControl, fmt.Sprintf("Settle claim %s: %s", short(claim.String(), 8), strings.Join(plan, "; then ")),
		map[string]any{"claim": claim.String(), "status": cl.Status, "steps": plan}, nil, p)
}

func ones(b uint8) (n int) {
	for ; b != 0; b &= b - 1 {
		n++
	}
	return
}

func (w *worker) claimAndObjective(ctx context.Context, claim solana.Key) (*solana.Claim, *solana.Objective, error) {
	cl, err := w.program.Claim(ctx, claim)
	if err == nil && cl == nil {
		err = fmt.Errorf("no claim at %s", claim)
	}
	if err != nil {
		return nil, nil, err
	}
	o, err := w.program.Objective(ctx, cl.Objective)
	if err == nil && o == nil {
		err = fmt.Errorf("no objective at %s", cl.Objective)
	}
	return cl, o, err
}

// settle carries an approved settle action out: each step that is due,
// reading the claim again after each, and the bus told of a resolution (by
// the settlement authority, whose message it is) and of a payment. An edge
// that will not mint is reported and the rest go on.
func (w *worker) settle(ctx context.Context, raw json.RawMessage) (string, error) {
	var p settleParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	claim, err := solana.ParseKey(p.Claim)
	if err != nil {
		return "", err
	}
	cl, o, err := w.claimAndObjective(ctx, claim)
	if err != nil {
		return "", err
	}
	col, err := w.program.Colony(ctx, o.Colony)
	if err != nil || col == nil {
		return "", fmt.Errorf("its colony: %v", err)
	}
	var steps []string
	finish := func(err error) (string, error) { return strings.Join(steps, "; "), err }
	send := func(ixs []chain.Instruction, err error) (chain.Landed, error) {
		if err != nil {
			return chain.Landed{}, err
		}
		return w.chain.Send(ctx, w.key, ixs)
	}
	tell := func(typ string, landed chain.Landed, body map[string]any) {
		body["objective"], body["claimHash"] = cl.Objective.String(), hex.EncodeToString(cl.Hash[:])
		if id, err := w.publish(typ, col.Slug, &envelope.ChainRef{Tx: landed.Signature, Slot: landed.Slot}, body); err != nil {
			steps = append(steps, typ+" not published on the bus: "+err.Error())
		} else {
			steps = append(steps, "on the bus as "+id)
		}
	}

	if cl.Status == "pending" {
		var s chain.Settlement
		if p.Settlement != nil {
			s = *p.Settlement
		}
		landed, err := send(w.chain.Resolve(ctx, w.me, claim, s))
		if err != nil {
			return finish(fmt.Errorf("not resolved: %w", err))
		}
		if cl, err = w.program.Claim(ctx, claim); err != nil || cl == nil {
			return finish(errors.Join(errors.New("resolved, and not read again"), err))
		}
		steps = append(steps, fmt.Sprintf("resolved in %s: %s, decided by %s", landed.Signature, cl.Status, cl.DecidedBy))
		if o.SettlementAuthority == w.me {
			tell("claim.resolved", landed, map[string]any{"admitted": cl.Status == "admitted", "decidedBy": cl.DecidedBy})
		}
	}
	if cl.Status == "admitted" && !cl.Awarded {
		landed, err := send(w.chain.Award(ctx, w.me, claim))
		if err != nil {
			return finish(fmt.Errorf("not paid: %w", err))
		}
		steps = append(steps, "its award paid into its agent's escrow in "+landed.Signature)
		tell("claim.paid", landed, map[string]any{})
		if cl.Agent == w.me {
			steps = append(steps, "this agent's to collect with unloop_collect")
		}
	}
	for i := range cl.Roots {
		if !cl.Payable(i) {
			continue
		}
		landed, err := send(w.chain.Edge(ctx, w.me, claim, uint8(i)))
		if err != nil {
			steps = append(steps, fmt.Sprintf("the edge to root %d not minted: %v", i, err))
			continue
		}
		steps = append(steps, fmt.Sprintf("the edge to root %d minted in %s", i, landed.Signature))
	}
	if len(steps) == 0 {
		steps = append(steps, "nothing was due: it is "+cl.Status)
	}
	return finish(nil)
}
