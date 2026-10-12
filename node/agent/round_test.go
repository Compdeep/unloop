//go:build devnet

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/solana"
)

// The first round, scripted (docs/PLATFORM.md section 8, step 3): the four
// agents `npm run stage` made, run in one process against devnet and the
// platform, calling their tools as a model would — with fixed arguments and
// no model — and approving each action on the agent's own approvals page, as
// a person does. At each step the account, the index and what the agents
// heard on the bus must agree.
//
//	go test -tags devnet -run Round -timeout 60m -v ./agent
//
// UNLOOP_STAGE names the stage.json to run on (default: the newest under
// ~/.unloop-rounds); ROUND_MINUTES is how long claims stay open (default 10),
// with commitments closing four minutes later and validation six after that.
//
// The objective is a real one, small enough to check by hand: the shortest
// tour through fifty fixed cities. A files six claims — a nearest-neighbour
// tour, three tours improving on it each by 2-opt and citing the one before,
// a branch improving the first another way, and a tour whose stated length
// is false. V1 and V2 check each by recomputing it, so the last is rejected
// on its evidence. L settles each as the settlement authority, confirming
// the roots it was built on; A collects.

// ── the problem ───────────────────────────────────────────────────────────

type city struct{ x, y int }

// cities are fixed by a seed, so the document, the claims and the checks agree.
func cities() []city {
	s, out := uint32(2026), make([]city, 50)
	next := func() int { s = s*1664525 + 1013904223; return int(s>>8) % 1000 }
	for i := range out {
		out[i] = city{next(), next()}
	}
	return out
}

func length(cs []city, tour []int) float64 {
	d := 0.0
	for i := range tour {
		a, b := cs[tour[i]], cs[tour[(i+1)%len(tour)]]
		d += math.Hypot(float64(a.x-b.x), float64(a.y-b.y))
	}
	return d
}

// assertion is a tour as a claim states it: the canonical result.
func assertion(tour []int, stated float64) string {
	ns := make([]string, len(tour))
	for i, n := range tour {
		ns[i] = strconv.Itoa(n)
	}
	return fmt.Sprintf("tour %s; length %.3f", strings.Join(ns, " "), stated)
}

// check is what a validator does with a claim's assertion: the tour must
// visit every city once from city 0, and be as long as it says.
func check(cs []city, text string) (tour []int, ok bool, why string) {
	parts := strings.SplitN(strings.TrimPrefix(text, "tour "), "; length ", 2)
	if len(parts) != 2 {
		return nil, false, "not in the claim format"
	}
	seen := map[int]bool{}
	for _, f := range strings.Fields(parts[0]) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 || n >= len(cs) || seen[n] {
			return nil, false, "not a tour of the cities: " + f
		}
		seen[n] = true
		tour = append(tour, n)
	}
	if len(tour) != len(cs) || tour[0] != 0 {
		return nil, false, "does not visit every city once from city 0"
	}
	if got := fmt.Sprintf("%.3f", length(cs, tour)); got != parts[1] {
		return tour, false, fmt.Sprintf("its length is %s, not %s", got, parts[1])
	}
	return tour, true, "recomputed: " + parts[1]
}

func nearest(cs []city) []int {
	tour, used := []int{0}, map[int]bool{0: true}
	for len(tour) < len(cs) {
		last, best := cs[tour[len(tour)-1]], -1
		for i := range cs {
			if !used[i] && (best < 0 || math.Hypot(float64(cs[i].x-last.x), float64(cs[i].y-last.y)) < math.Hypot(float64(cs[best].x-last.x), float64(cs[best].y-last.y))) {
				best = i
			}
		}
		tour, used[best] = append(tour, best), true
	}
	return tour
}

// improvements is every tour 2-opt passes through from `from` to a local
// optimum, first improvement found, scanning forward or backward.
func improvements(cs []city, from []int, backward bool) [][]int {
	tour, out := slices.Clone(from), [][]int{}
	n := len(tour)
	for improved := true; improved; {
		improved = false
		for k := 1; k < n-1 && !improved; k++ {
			i := k
			if backward {
				i = n - 1 - k
			}
			for j := i + 1; j < n && !improved; j++ {
				next := slices.Clone(tour)
				slices.Reverse(next[i : j+1])
				if length(cs, next) < length(cs, tour)-1e-9 {
					tour, improved = next, true
					out = append(out, tour)
				}
			}
		}
	}
	return out
}

// ── the agents ────────────────────────────────────────────────────────────

type stage struct {
	Colony struct{ Slug, Address string }
	Agents map[string]struct{ Home, Address, Chat string }
}

type roundAgent struct {
	name string
	*assembled
}

type round struct {
	t      *testing.T
	ctx    context.Context
	agents map[string]*roundAgent
	stage  stage
	log    []string
}

func stagePath(t *testing.T) string {
	if p := os.Getenv("UNLOOP_STAGE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	found, _ := filepath.Glob(filepath.Join(home, ".unloop-rounds", "*", "stage.json"))
	if len(found) == 0 {
		t.Skip("no stage: run `npm run stage` in chain/ first, or set UNLOOP_STAGE")
	}
	slices.SortFunc(found, func(a, b string) int {
		ia, _ := os.Stat(a)
		ib, _ := os.Stat(b)
		return ib.ModTime().Compare(ia.ModTime())
	})
	return found[0]
}

// call runs one of an agent's tools as the model would, and must succeed.
// The arguments go through JSON first, as a model's do, so a number arrives
// as a float64 and a list as []any.
func (r *round) call(who, name string, given map[string]any) toolapi.ToolMessage {
	r.t.Helper()
	var a map[string]any
	b, _ := json.Marshal(given)
	json.Unmarshal(b, &a)
	ag := r.agents[who]
	for _, tl := range ag.tools() {
		if tl.Name() != name {
			continue
		}
		m, err := tl.(interface {
			ExecuteTyped(context.Context, map[string]any) (toolapi.ToolMessage, error)
		}).ExecuteTyped(r.ctx, a)
		if err != nil || m.Status != toolapi.StatusOK {
			r.t.Fatalf("%s %s: %v %s %s", who, name, err, m.Detail, m.Content)
		}
		return m
	}
	r.t.Fatalf("%s has no tool %s", who, name)
	return toolapi.ToolMessage{}
}

// approve decides a proposed action on the agent's approvals page, as the
// person's browser does, and waits for it to be carried out.
func (r *round) approve(who string, m toolapi.ToolMessage) string {
	r.t.Helper()
	var d struct{ Action string }
	if err := json.Unmarshal(m.Data, &d); err != nil || d.Action == "" {
		r.t.Fatalf("%s: nothing was proposed: %s", who, m.Content)
	}
	ag := r.agents[who]
	req, _ := http.NewRequestWithContext(r.ctx, http.MethodPost, "http://"+ag.cfg.Chat+"/unloop/approvals/"+d.Action+"/approve", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil || res.StatusCode != http.StatusSeeOther {
		r.t.Fatalf("%s: approving %s: %v %v", who, d.Action, err, res)
	}
	res.Body.Close()
	var act Action
	r.eventually(who+"'s action "+d.Action, 6*time.Minute, func() (bool, string) {
		for _, a := range ag.approvals.recent(100) {
			if a.ID == d.Action {
				act = a
				return a.State == "done" || a.State == "failed", a.State
			}
		}
		return false, "not found"
	})
	if act.State != "done" {
		r.t.Fatalf("%s: %s failed: %s", who, act.Summary, act.Result)
	}
	r.note("%s: %s — %s", who, act.Summary, act.Result)
	return act.Result
}

func (r *round) note(f string, v ...any) {
	line := fmt.Sprintf(f, v...)
	r.log = append(r.log, line)
	r.t.Log(line)
}

// eventually waits for something the chain, the index or the bus will come to say.
func (r *round) eventually(what string, limit time.Duration, f func() (bool, string)) {
	r.t.Helper()
	end, last := time.Now().Add(limit), ""
	for {
		ok, state := f()
		if ok {
			return
		}
		last = state
		if time.Now().After(end) {
			r.t.Fatalf("%s: still %s after %s", what, last, limit)
		}
		select {
		case <-r.ctx.Done():
			r.t.Fatal(r.ctx.Err())
		case <-time.After(4 * time.Second):
		}
	}
}

// index reads the platform's API as the agents do.
func (r *round) index(path string, out any) error {
	return r.agents["L"].work.platform.get(r.ctx, path, nil, out)
}

// heard waits for each of `who` to have heard a message of type typ about subject.
func (r *round) heard(typ, subject string, since time.Time, who ...string) {
	r.t.Helper()
	for _, name := range who {
		r.eventually(name+" hearing "+typ+" "+short(subject, 8), 3*time.Minute, func() (bool, string) {
			for _, h := range r.agents[name].loop.inbox.since(since, 500) {
				b, _ := json.Marshal(h.Body)
				if h.Type == typ && strings.Contains(string(b), subject) {
					return true, ""
				}
			}
			return false, "silent"
		})
	}
}

func (r *round) until(unix int64, what string) {
	wait := time.Until(time.Unix(unix, 0)) + 5*time.Second
	if wait <= 0 {
		return
	}
	r.note("waiting %s for %s", wait.Round(time.Second), what)
	select {
	case <-r.ctx.Done():
		r.t.Fatal(r.ctx.Err())
	case <-time.After(wait):
	}
}

// ── the round ─────────────────────────────────────────────────────────────

func TestRound(t *testing.T) {
	path := stagePath(t)
	var st stage
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	r := &round{t: t, ctx: ctx, agents: map[string]*roundAgent{}, stage: st}
	for _, name := range []string{"L", "A", "V1", "V2"} {
		home := st.Agents[name].Home
		cfg, err := Load(home)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a, err := assemble(ctx, home, cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer a.node.Close()
		go a.serve(ctx, nil)
		r.agents[name] = &roundAgent{name, a}
	}
	time.Sleep(3 * time.Second) // the pages listening, the nodes meshed
	start := time.Now()
	slug, L, A := st.Colony.Slug, r.agents["L"], r.agents["A"]
	cs := cities()

	// 1. L drafts the objective and creates it, approved by hand.
	minutes := 10
	if m, err := strconv.Atoi(os.Getenv("ROUND_MINUTES")); err == nil && m > 0 {
		minutes = m
	}
	claimsClose := time.Now().Add(time.Duration(minutes) * time.Minute)
	var listing []string
	for i, c := range cs {
		listing = append(listing, fmt.Sprintf("%d:%d,%d", i, c.x, c.y))
	}
	draft := r.call("L", "unloop_draft_objective", map[string]any{
		"colony": slug, "title": "Shortest tour through 50 cities",
		"statement": "Find a short closed tour through the fifty cities listed in the claim format, visiting each once and returning to the start. Each shorter tour is a discovery; build on earlier tours and cite them.",
		"payout":    "rate", "validationMode": "deterministic",
		"claimForm": map[string]any{"name": "tour-v1", "spec": "One line: `tour <the 50 city indices, space-separated, starting at 0>; length <its total Euclidean length, 3 decimals>`. Cities as index:x,y — " + strings.Join(listing, " ")},
		"predicates": []any{
			map[string]any{"id": "P1", "proposition": "The tour visits each of the 50 cities exactly once, starting at city 0, and returns to it.", "kind": "required"},
			map[string]any{"id": "P2", "proposition": "The stated length is the tour's Euclidean length, to 3 decimals.", "kind": "required"},
			map[string]any{"id": "P3", "proposition": "The tour is shorter than the nearest-neighbour tour from city 0.", "kind": "scored", "weight": 1},
		},
		"awards":             []any{map[string]any{"title": "A shorter tour", "criteria": "Each verified tour, paid by its score.", "mode": "graded", "amount": "15"}},
		"pool":               "100",
		"validatorsPerClaim": 2,
		"claimDeadline":      claimsClose.UTC().Format(time.RFC3339),
		"commitDeadline":     claimsClose.Add(4 * time.Minute).UTC().Format(time.RFC3339),
		"validationDeadline": claimsClose.Add(10 * time.Minute).UTC().Format(time.RFC3339),
	})
	var drafted struct{ Draft, Address string }
	json.Unmarshal(draft.Data, &drafted)
	r.approve("L", r.call("L", "unloop_create_objective", map[string]any{"draft": drafted.Draft}))
	objective, _ := solana.ParseKey(drafted.Address)

	o, err := L.work.program.Objective(ctx, objective)
	if err != nil || o == nil || o.SettlementAuthority != L.me || o.ValidatorsPerClaim != 2 {
		t.Fatalf("the objective's account: %+v %v", o, err)
	}
	r.eventually("the index verifying the objective's document", 3*time.Minute, func() (bool, string) {
		var d struct{ Spec *struct{ Verified bool } }
		if err := r.index("/api/objectives/"+drafted.Address, &d); err != nil {
			return false, err.Error()
		}
		return d.Spec != nil && d.Spec.Verified, "unverified"
	})
	r.heard("objective.publish", drafted.Address, start, "A", "V1", "V2")

	// 2. A files six claims, each approved by hand: a lineage, a branch and a false one.
	base := nearest(cs)
	forward := improvements(cs, base, false)
	backward := improvements(cs, base, true)
	if len(forward) < 3 || len(backward) < 1 {
		t.Fatalf("2-opt found too little: %d, %d", len(forward), len(backward))
	}
	best := forward[len(forward)-1]
	type plan struct {
		tour      []int
		stated    float64
		root      int // the plan it cites; -1 for none
		how       string
		misstated bool // its stated length is not its length
	}
	plans := []plan{
		{base, length(cs, base), -1, "nearest neighbour from city 0", false},
		{forward[len(forward)/3], 0, 0, "2-opt from claim 1, a third of the way to a local optimum", false},
		{forward[2*len(forward)/3], 0, 1, "2-opt continued from claim 2", false},
		{best, 0, 2, "2-opt from claim 3 to a local optimum", false},
		{backward[len(backward)/2], 0, 0, "2-opt from claim 1 scanning backward", false},
		{best, length(cs, best) - 150, 3, "claim 4's tour, its length misstated", true},
	}
	var claims []string
	for i := range plans {
		p := &plans[i]
		if p.stated == 0 {
			p.stated = length(cs, p.tour)
		}
		args := map[string]any{
			"objective": drafted.Address, "assertion": assertion(p.tour, p.stated),
			"significance": fmt.Sprintf("A tour of length %.3f: %s.", p.stated, p.how),
			"evidence":     []any{map[string]any{"kind": "deterministic", "statement": "Recompute the tour's length from the listed cities; it visits each once from city 0.", "provenance": p.how}},
		}
		if p.root >= 0 {
			args["roots"] = []any{claims[p.root]}
		}
		d := r.call("A", "unloop_draft_claim", args)
		var dc struct{ Hash, Claim string }
		json.Unmarshal(d.Data, &dc)
		r.approve("A", r.call("A", "unloop_file_claim", map[string]any{"hash": dc.Hash}))
		claims = append(claims, dc.Claim)
		key, _ := solana.ParseKey(dc.Claim)
		c, err := A.work.program.Claim(ctx, key)
		if err != nil || c == nil || c.Agent != A.me || c.Status != "pending" || len(c.Roots) != map[bool]int{true: 1, false: 0}[p.root >= 0] {
			t.Fatalf("claim %d's account: %+v %v", i+1, c, err)
		}
		r.eventually(fmt.Sprintf("the index holding claim %d's body", i+1), 3*time.Minute, func() (bool, string) {
			var d struct {
				Claim struct{ Status string }
				Body  *struct{ Check struct{ Verified bool } }
			}
			if err := r.index("/api/claims/"+dc.Claim, &d); err != nil {
				return false, err.Error()
			}
			return d.Body != nil && d.Body.Check.Verified && d.Claim.Status == "pending", "no verified body"
		})
		r.heard("claim.publish", dc.Hash, start, "V1", "V2")
	}
	if time.Now().Unix() >= o.ClaimDeadline {
		t.Fatalf("filing took past the claim deadline: set ROUND_MINUTES higher")
	}

	// 3. V1 and V2 find their duties, check each claim and commit, approved by hand.
	verdicts := map[string]map[string]any{}
	for i, c := range claims {
		tour, ok, why := check(cs, assertion(plans[i].tour, plans[i].stated))
		v := map[string]any{"claim": c, "reason": "none", "reproduction": "pass", "rationale": why,
			"items": []any{map[string]any{"id": "D1", "holds": "pass", "supportsClaim": "pass", "note": why}}}
		if ok {
			shorter := true
			for _, earlier := range plans[:i] {
				if length(cs, earlier.tour) <= length(cs, tour) {
					shorter = false
				}
			}
			score := 5000 + int(50000*(length(cs, base)-length(cs, tour))/length(cs, base))
			v["claimSupport"], v["objectiveAlignment"], v["scoreBps"] = "pass", "pass", min(score, 10000)
			v["noveltyBps"] = map[bool]int{true: 7000, false: 3000}[shorter]
		} else {
			v["claimSupport"], v["objectiveAlignment"], v["noveltyBps"], v["scoreBps"], v["reproduction"] = "fail", "fail", 0, 0, "fail"
			v["items"] = []any{map[string]any{"id": "D1", "holds": "fail", "supportsClaim": "fail", "note": why}}
		}
		verdicts[c] = v
	}
	for _, name := range []string{"V1", "V2"} {
		r.eventually(name+" drawn for every claim", 4*time.Minute, func() (bool, string) {
			d := r.call(name, "unloop_duties", nil)
			var duties struct{ Awaiting []struct{ Claim string } }
			json.Unmarshal(d.Data, &duties)
			got := 0
			for _, a := range duties.Awaiting {
				if slices.Contains(claims, a.Claim) {
					got++
				}
			}
			return got == len(claims), fmt.Sprintf("drawn for %d of %d", got, len(claims))
		})
		for _, c := range claims {
			// Each checks the claim itself, from what the claim says: the body the index verified.
			var d struct {
				Body struct {
					Body struct{ Assertion string }
				}
			}
			if err := r.index("/api/claims/"+c, &d); err != nil {
				t.Fatal(err)
			}
			_, ok, why := check(cs, d.Body.Body.Assertion)
			if want := verdicts[c]["claimSupport"] == "pass"; ok != want {
				t.Fatalf("%s's check of %s: %v (%s), expected %v", name, c, ok, why, want)
			}
			r.approve(name, r.call(name, "unloop_commit", verdicts[c]))
		}
	}
	for _, c := range claims {
		r.eventually("the index counting both commitments on "+short(c, 8), 3*time.Minute, func() (bool, string) {
			var d struct{ Claim struct{ Commitments int } }
			r.index("/api/claims/"+c, &d)
			return d.Claim.Commitments == 2, fmt.Sprintf("%d", d.Claim.Commitments)
		})
	}

	// 4. Once commitments close, each opens its findings.
	r.until(o.CommitDeadline, "commitments to close")
	for _, name := range []string{"V1", "V2"} {
		for _, c := range claims {
			r.approve(name, r.call(name, "unloop_reveal", map[string]any{"claim": c}))
		}
	}
	for _, c := range claims {
		r.eventually("the index counting both reveals on "+short(c, 8), 3*time.Minute, func() (bool, string) {
			var d struct{ Claim struct{ Reveals int } }
			r.index("/api/claims/"+c, &d)
			return d.Claim.Reveals == 2, fmt.Sprintf("%d", d.Claim.Reveals)
		})
	}
	r.heard("validation.publish", claimHashOf(r, claims[0]), start, "A")

	// 5. L settles each as the settlement authority: its validators decided,
	// so the verdict and figures are theirs; L confirms the roots.
	for i, c := range claims {
		admit := !plans[i].misstated
		a := map[string]any{"claim": c, "admit": admit, "noveltyBps": 0, "scoreBps": 0}
		if plans[i].root >= 0 && admit {
			a["rootsConfirmed"] = []any{0}
		}
		r.approve("L", r.call("L", "unloop_settle", a))
		key, _ := solana.ParseKey(c)
		cl, err := L.work.program.Claim(ctx, key)
		want := map[bool]string{true: "admitted", false: "rejected"}[admit]
		if err != nil || cl == nil || cl.Status != want || cl.DecidedBy != "consensus" || (want == "admitted") != cl.Awarded {
			t.Fatalf("claim %d after settling: %+v %v", i+1, cl, err)
		}
		r.eventually(fmt.Sprintf("the index settling claim %d", i+1), 3*time.Minute, func() (bool, string) {
			var d struct {
				Claim struct {
					Status, DecidedBy string
					Awarded           *string
				}
				Roots []struct{ Confirmed bool }
			}
			r.index("/api/claims/"+c, &d)
			confirmed := len(d.Roots) == 0 || d.Roots[0].Confirmed == (want == "admitted")
			return d.Claim.Status == want && (d.Claim.Awarded != nil) == (want == "admitted") && confirmed, d.Claim.Status
		})
		r.heard("claim.resolved", claimHashOf(r, c), start, "A")
	}

	// 6. A collects what its escrow holds.
	mint := o.PaymentMint.String()
	before := balance(r, A.me, mint)
	r.approve("A", r.call("A", "unloop_collect", map[string]any{"objective": drafted.Address}))
	if after := balance(r, A.me, mint); after.Cmp(before) <= 0 {
		t.Fatalf("A's balance did not grow: %s, then %s", before, after)
	}

	r.note("the round took %s; the graph: %s/objectives/%s/graph?as=%s", time.Since(start).Round(time.Second), L.cfg.Platform, drafted.Address,
		strings.Join([]string{L.me.String() + ":L", A.me.String() + ":A", r.agents["V1"].me.String() + ":V1", r.agents["V2"].me.String() + ":V2"}, ","))
	out, _ := json.MarshalIndent(map[string]any{"objective": drafted.Address, "claims": claims, "log": r.log}, "", "  ")
	os.WriteFile(filepath.Join(filepath.Dir(path), "round-"+time.Now().UTC().Format("20060102-1504")+".json"), out, 0o600)
}

func claimHashOf(r *round, claim string) string {
	var d struct{ Claim struct{ Hash string } }
	if err := r.index("/api/claims/"+claim, &d); err != nil {
		r.t.Fatal(err)
	}
	return d.Claim.Hash
}

// balance is what an owner holds of one mint, in base units.
func balance(r *round, owner solana.Key, mint string) *big.Int {
	total := new(big.Int)
	for _, tp := range tokenPrograms {
		program, _ := solana.ParseKey(tp)
		list, err := r.agents["A"].readers.rpc.TokenBalances(r.ctx, owner, program)
		if err != nil {
			r.t.Fatal(err)
		}
		for _, b := range list {
			if b.Mint == mint {
				n, _ := new(big.Int).SetString(b.Amount, 10)
				total.Add(total, n)
			}
		}
	}
	return total
}

// The round's problem, without the chain: the tours improve, and a
// validator's check takes a true one and refuses a misstated one.
func TestRoundTours(t *testing.T) {
	cs := cities()
	base := nearest(cs)
	forward, backward := improvements(cs, base, false), improvements(cs, base, true)
	if len(forward) < 3 || len(backward) < 1 {
		t.Fatalf("2-opt found %d and %d improvements", len(forward), len(backward))
	}
	for i := 1; i < len(forward); i++ {
		if length(cs, forward[i]) >= length(cs, forward[i-1]) {
			t.Fatalf("improvement %d is no shorter", i)
		}
	}
	best := forward[len(forward)-1]
	if _, ok, why := check(cs, assertion(best, length(cs, best))); !ok {
		t.Fatal(why)
	}
	if _, ok, _ := check(cs, assertion(best, length(cs, best)-150)); ok {
		t.Fatal("a misstated length passed")
	}
	t.Logf("nearest neighbour %.3f; 2-opt %d steps to %.3f; backward %d steps to %.3f", length(cs, base),
		len(forward), length(cs, best), len(backward), length(cs, backward[len(backward)-1]))
}

// The round as it can run today, for a demo: the leader judges each claim as
// it lands, as the settlement authority may while no validator has sealed a
// vote, so each is admitted in time for the next to build on it and the graph
// grows live. The misstated tour is checked by the leader and rejected.
//
//	go test -tags devnet -run LeaderRound -timeout 30m -v ./agent
//
// ROUND_RECLAIM names an objective of L's whose free pool it takes back first;
// ROUND_PAUSE is the seconds between claims (default 15).
func TestLeaderRound(t *testing.T) {
	path := stagePath(t)
	var st stage
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Minute)
	defer cancel()
	r := &round{t: t, ctx: ctx, agents: map[string]*roundAgent{}, stage: st}
	for _, name := range []string{"L", "A"} {
		home := st.Agents[name].Home
		cfg, err := Load(home)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a, err := assemble(ctx, home, cfg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer a.node.Close()
		go a.serve(ctx, nil)
		r.agents[name] = &roundAgent{name, a}
	}
	time.Sleep(3 * time.Second)
	L, A, cs := r.agents["L"], r.agents["A"], cities()
	pause := 15 * time.Second
	if s, err := strconv.Atoi(os.Getenv("ROUND_PAUSE")); err == nil && s >= 0 {
		pause = time.Duration(s) * time.Second
	}

	if old := os.Getenv("ROUND_RECLAIM"); old != "" {
		r.approve("L", r.call("L", "unloop_withdraw", map[string]any{"objective": old}))
	}

	var listing []string
	for i, c := range cs {
		listing = append(listing, fmt.Sprintf("%d:%d,%d", i, c.x, c.y))
	}
	closes := time.Now().Add(2 * time.Hour)
	draft := r.call("L", "unloop_draft_objective", map[string]any{
		"colony": st.Colony.Slug, "title": "Shortest tour through 50 cities",
		"statement": "Find a short closed tour through the fifty cities listed in the claim format, visiting each once and returning to the start. Each shorter tour is a discovery; build on earlier tours and cite them.",
		"payout":    "rate", "validationMode": "deterministic",
		"claimForm": map[string]any{"name": "tour-v1", "spec": "One line: `tour <the 50 city indices, space-separated, starting at 0>; length <its total Euclidean length, 3 decimals>`. Cities as index:x,y — " + strings.Join(listing, " ")},
		"predicates": []any{
			map[string]any{"id": "P1", "proposition": "The tour visits each of the 50 cities exactly once, starting at city 0, and returns to it.", "kind": "required"},
			map[string]any{"id": "P2", "proposition": "The stated length is the tour's Euclidean length, to 3 decimals.", "kind": "required"},
			map[string]any{"id": "P3", "proposition": "The tour is shorter than the nearest-neighbour tour from city 0.", "kind": "scored", "weight": 1},
		},
		"awards":             []any{map[string]any{"title": "A shorter tour", "criteria": "Each verified tour, paid by its score and novelty.", "mode": "graded", "amount": "100"}},
		"pool":               "500",
		"validatorsPerClaim": 2,
		"claimDeadline":      closes.UTC().Format(time.RFC3339),
		"commitDeadline":     closes.Add(time.Minute).UTC().Format(time.RFC3339),
		"validationDeadline": closes.Add(2 * time.Minute).UTC().Format(time.RFC3339),
	})
	var drafted struct{ Draft, Address string }
	json.Unmarshal(draft.Data, &drafted)
	r.approve("L", r.call("L", "unloop_create_objective", map[string]any{"draft": drafted.Draft}))
	graph := fmt.Sprintf("%s/objectives/%s/graph?as=%s:L,%s:A", L.cfg.Platform, drafted.Address, L.me, A.me)
	r.note("the graph: %s", graph)
	r.eventually("the index verifying the objective's document", 3*time.Minute, func() (bool, string) {
		var d struct{ Spec *struct{ Verified bool } }
		if err := r.index("/api/objectives/"+drafted.Address, &d); err != nil {
			return false, err.Error()
		}
		return d.Spec != nil && d.Spec.Verified, "unverified"
	})

	base := nearest(cs)
	forward, backward := improvements(cs, base, false), improvements(cs, base, true)
	best := forward[len(forward)-1]
	plans := []struct {
		tour   []int
		stated float64
		root   int
		how    string
	}{
		{base, length(cs, base), -1, "nearest neighbour from city 0"},
		{forward[len(forward)/3], 0, 0, "2-opt from claim 1, a third of the way to a local optimum"},
		{backward[len(backward)/2], 0, 0, "2-opt from claim 1 scanning backward"},
		{forward[2*len(forward)/3], 0, 1, "2-opt continued from claim 2"},
		{best, 0, 3, "2-opt from claim 4 to a local optimum"},
		{best, length(cs, best) - 150, 4, "claim 5's tour, its length misstated"},
	}
	var claims []string
	for i := range plans {
		p := &plans[i]
		if p.stated == 0 {
			p.stated = length(cs, p.tour)
		}
		args := map[string]any{
			"objective": drafted.Address, "assertion": assertion(p.tour, p.stated),
			"significance": fmt.Sprintf("A tour of length %.3f: %s.", p.stated, p.how),
			"evidence":     []any{map[string]any{"kind": "deterministic", "statement": "Recompute the tour's length from the listed cities; it visits each once from city 0.", "provenance": p.how}},
		}
		if p.root >= 0 {
			args["roots"] = []any{claims[p.root]}
		}
		d := r.call("A", "unloop_draft_claim", args)
		var dc struct{ Hash, Claim string }
		json.Unmarshal(d.Data, &dc)
		r.approve("A", r.call("A", "unloop_file_claim", map[string]any{"hash": dc.Hash}))
		claims = append(claims, dc.Claim)

		// The leader checks it as a validator would, from the body the index verified.
		var body struct {
			Body *struct {
				Check struct{ Verified bool }
				Body  struct{ Assertion string }
			}
		}
		r.eventually(fmt.Sprintf("the index holding claim %d's body", i+1), 3*time.Minute, func() (bool, string) {
			if err := r.index("/api/claims/"+dc.Claim, &body); err != nil {
				return false, err.Error()
			}
			return body.Body != nil && body.Body.Check.Verified, "no verified body"
		})
		tour, ok, why := check(cs, body.Body.Body.Assertion)
		settle := map[string]any{"claim": dc.Claim, "admit": ok, "noveltyBps": 0, "scoreBps": 0}
		if ok {
			shorter := true
			for _, earlier := range plans[:i] {
				if length(cs, earlier.tour) <= length(cs, tour) {
					shorter = false
				}
			}
			settle["scoreBps"] = min(5000+int(50000*(length(cs, base)-length(cs, tour))/length(cs, base)), 10000)
			settle["noveltyBps"] = map[bool]int{true: 8000, false: 4000}[shorter]
			if p.root >= 0 {
				settle["rootsConfirmed"] = []any{0}
			}
		}
		r.note("L checks claim %d: %s", i+1, why)
		r.approve("L", r.call("L", "unloop_settle", settle))
		key, _ := solana.ParseKey(dc.Claim)
		cl, err := L.work.program.Claim(ctx, key)
		want := map[bool]string{true: "admitted", false: "rejected"}[ok]
		if err != nil || cl == nil || cl.Status != want || cl.DecidedBy != "leader" || ok != cl.Awarded {
			t.Fatalf("claim %d after settling: %+v %v", i+1, cl, err)
		}
		if i < len(plans)-1 {
			time.Sleep(pause)
		}
	}
	r.approve("A", r.call("A", "unloop_collect", map[string]any{"objective": drafted.Address}))
	r.note("done: %s", graph)
}
