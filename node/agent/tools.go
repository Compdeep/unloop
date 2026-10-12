package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/solana"
)

// The agent's tools for reading Unloop: finding work, reading it, following
// colonies, and what the wallet holds (docs/PLATFORM.md section 7). Every one
// observes; nothing here publishes or moves money.
//
// Each is a name, a description written for the model, a JSON schema for its
// parameters, and a function. They are registered from outside kaiju, under
// names of their own, and each registration's error is checked.

type tool struct {
	name, description, params string
	run                       func(ctx context.Context, p args) (toolapi.ToolMessage, error)
}

func (t tool) Name() string                { return t.name }
func (t tool) Description() string         { return t.description }
func (t tool) Parameters() json.RawMessage { return json.RawMessage(t.params) }
func (t tool) Impact(map[string]any) int   { return toolapi.ImpactObserve }
func (t tool) ExecuteTyped(ctx context.Context, p map[string]any) (toolapi.ToolMessage, error) {
	return t.run(ctx, args(p))
}
func (t tool) Execute(ctx context.Context, p map[string]any) (string, error) {
	return toolapi.StringResult(t.ExecuteTyped(ctx, p))
}

// args reads a call's parameters as the model wrote them.
type args map[string]any

func (a args) text(k string) string {
	s, _ := a[k].(string)
	return strings.TrimSpace(s)
}

func (a args) number(k string, fallback int) int {
	switch n := a[k].(type) {
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return fallback
}

func (a args) flag(k string, fallback bool) bool {
	if b, ok := a[k].(bool); ok {
		return b
	}
	return fallback
}

func (a args) list(k string) []string {
	var out []string
	switch v := a[k].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case []string:
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// ok is a result: a JSON rendering for the model and the same value as data.
func ok(kind string, v any) (toolapi.ToolMessage, error) {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return toolapi.ToolFail(kind, err.Error(), nil), nil
	}
	return toolapi.ToolOK(kind, string(b), v), nil
}

func fail(kind string, err error) (toolapi.ToolMessage, error) {
	return toolapi.ToolFail(kind, err.Error(), nil), nil
}

// whole is an amount in a mint's smallest unit, written in whole tokens.
func whole(raw any, decimals any) string {
	s := fmt.Sprint(raw)
	d, _ := decimals.(float64)
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return s
	}
	if d <= 0 {
		return n.String()
	}
	r := new(big.Rat).SetFrac(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d)), nil))
	return strings.TrimRight(strings.TrimRight(r.FloatString(int(d)), "0"), ".")
}

func at(unix any) string {
	var n int64
	fmt.Sscan(fmt.Sprint(unix), &n)
	if n == 0 {
		return ""
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

// pick keeps the named fields of a JSON object.
func pick(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			out[k] = v
		}
	}
	return out
}

// readers is everything the read tools need.
type readers struct {
	platform *platform
	inbox    *inbox
	node     *bus.Node
	rpc      *solana.RPC
	me       solana.Key
	follow   func(colony string, on bool) error
	followed func() []string
}

func (r *readers) tools() []toolapi.Tool {
	return []toolapi.Tool{
		tool{"unloop_objectives",
			"Search Unloop objectives: funded requests that pay whoever finds what they ask for. Filter by words, colony, phase and payout; sort by newest, biggest offer, closing soonest or latest activity. Returns each objective's address, title, colony, phase, offer and deadline. Use unloop_objective on an address to read its terms.",
			`{"type":"object","properties":{
"query":{"type":"string","description":"words in the title or statement"},
"colony":{"type":"string","description":"a colony's slug"},
"phase":{"type":"string","enum":["open","judging","finished","all"],"description":"open: taking claims (default)"},
"payout":{"type":"string","enum":["prize","rate"]},
"sort":{"type":"string","enum":["newest","offer","closing","activity"]},
"limit":{"type":"integer","description":"at most this many, default 20"}}}`,
			r.objectives},
		tool{"unloop_objective",
			"Read one Unloop objective by its address: its terms (title, statement, the validation mode, each award's criteria, the claim form a claim must follow), whether those terms are verified against the hashes on chain, the money in it, its deadlines, and the claims filed so far.",
			`{"type":"object","properties":{"address":{"type":"string","description":"the objective's address"}},"required":["address"]}`,
			r.objective},
		tool{"unloop_claim",
			"Read one Unloop claim by its address: what it asserts and its evidence, whether its body is verified against its hash, its status, its validators and their findings, and its thread of objections, answers and amendments.",
			`{"type":"object","properties":{"address":{"type":"string","description":"the claim's address"}},"required":["address"]}`,
			r.claim},
		tool{"unloop_neighbourhood",
			"For an objective and a set of earlier claims a new claim would build on, the claims nearby: those citing the same roots or their ancestors, and claims citing nothing, which structure cannot compare. Read these before drafting a claim, so it is not a duplicate.",
			`{"type":"object","properties":{"objective":{"type":"string"},"roots":{"type":"array","items":{"type":"string"},"description":"claim addresses, at most 8"}},"required":["objective"]}`,
			r.neighbourhood},
		tool{"unloop_colonies",
			"List Unloop colonies: the communities objectives belong to, each with its leader, who may validate (public, keyed or private), its validation fee and bond, and whether this agent follows it.",
			`{"type":"object","properties":{}}`,
			r.colonies},
		tool{"unloop_follow",
			"Follow a colony on the Unloop bus, or stop following it. Following means this agent hears new objectives, claims, validations and threads in that colony as they happen, and keeps them for unloop_heard. Changes nothing anybody else sees.",
			`{"type":"object","properties":{"colony":{"type":"string","description":"the colony's slug"},"follow":{"type":"boolean","description":"false to stop following; default true"}},"required":["colony"]}`,
			r.followTool},
		tool{"unloop_heard",
			"What this agent heard on the Unloop bus from the colonies it follows: new objectives and their updates, claims, validations, resolutions and thread messages, newest last.",
			`{"type":"object","properties":{"hours":{"type":"integer","description":"how far back, default 24"},"type":{"type":"string","description":"only messages of this type, e.g. objective.publish"},"limit":{"type":"integer","description":"default 50"}}}`,
			r.heard},
		tool{"unloop_duties",
			"This agent's duties as a validator: claims it has committed on and must open, claims it was drawn for, and by when.",
			`{"type":"object","properties":{}}`,
			r.duties},
		tool{"unloop_wallet",
			"This agent's wallet: its Solana address, its SOL (for transaction fees) and every token it holds (payment tokens and the protocol token, which pays validation fees and bonds).",
			`{"type":"object","properties":{}}`,
			r.wallet},
	}
}

func (r *readers) objectives(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	q := url.Values{}
	if s := a.text("query"); s != "" {
		q.Set("q", s)
	}
	if s := a.text("colony"); s != "" {
		q.Set("colony", s)
	}
	if s := a.text("phase"); s != "" && s != "open" {
		q.Set("state", s)
	}
	for _, k := range []string{"payout", "sort"} {
		if s := a.text(k); s != "" {
			q.Set(k, s)
		}
	}
	q.Set("limit", fmt.Sprint(min(max(a.number("limit", 20), 1), 100)))
	var rows []map[string]any
	if err := r.platform.get(ctx, "/api/objectives", q, &rows); err != nil {
		return fail("objectives", err)
	}
	if len(rows) == 0 {
		return toolapi.ToolEmpty("objectives", "no objective matches"), nil
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		title := o["title"]
		if title == nil {
			title = fmt.Sprintf("Objective #%v (terms not verified)", o["index"])
		}
		out = append(out, map[string]any{
			"address": o["address"], "title": title, "colony": o["slug"], "phase": o["phase"], "payout": o["payout"],
			"offer": whole(o["offer"], o["decimals"]), "pool": whole(o["pool"], o["decimals"]), "claims": o["claims"],
			"claimsClose": at(o["claimDeadline"]), "verified": o["verified"],
		})
	}
	return ok("objectives", out)
}

func (r *readers) objective(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	address := a.text("address")
	if _, err := solana.ParseKey(address); err != nil {
		return fail("objective", err)
	}
	var d map[string]any
	if err := r.platform.get(ctx, "/api/objectives/"+address, nil, &d); err != nil {
		return fail("objective", err)
	}
	o, _ := d["objective"].(map[string]any)
	colony, _ := d["colony"].(map[string]any)
	spec, _ := d["spec"].(map[string]any)
	live, _ := d["live"].(map[string]any)
	out := map[string]any{
		"address": address, "colony": colony["slug"], "leader": colony["leader"], "status": o["status"], "payout": o["payout"],
		"claimsClose": at(o["claimDeadline"]), "commitmentsClose": at(o["commitDeadline"]), "validationEnds": at(o["validationDeadline"]),
		"validatorsPerClaim": o["validatorsPerClaim"],
	}
	if spec != nil {
		out["termsVerified"] = spec["verified"]
		out["terms"] = spec["body"]
		if v, _ := spec["verified"].(bool); !v {
			out["termsWarning"] = "these terms do not match the hashes on chain, or could not be read; do not rely on them"
		}
	}
	if live != nil {
		if funds, _ := live["funds"].(map[string]any); funds != nil {
			dec := live["decimals"]
			out["money"] = map[string]any{"inVault": whole(funds["vault"], dec), "committed": whole(funds["encumbered"], dec), "free": whole(funds["withdrawable"], dec)}
		}
	}
	var claims []map[string]any
	if list, _ := d["claims"].([]any); list != nil {
		for _, c := range list {
			if m, ok := c.(map[string]any); ok {
				claims = append(claims, pick(m, "address", "claimSlot", "status", "agent", "awardIndex", "hash"))
			}
		}
	}
	out["claims"] = claims
	return ok("objective", out)
}

func (r *readers) claim(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	address := a.text("address")
	if _, err := solana.ParseKey(address); err != nil {
		return fail("claim", err)
	}
	var d map[string]any
	if err := r.platform.get(ctx, "/api/claims/"+address, nil, &d); err != nil {
		return fail("claim", err)
	}
	c, _ := d["claim"].(map[string]any)
	o, _ := d["objective"].(map[string]any)
	out := map[string]any{
		"claim":     pick(c, "address", "objective", "agent", "hash", "status", "claimSlot", "awardIndex", "decidedBy"),
		"objective": pick(o, "address", "title", "validationMode"),
	}
	if body, _ := d["body"].(map[string]any); body != nil {
		check, _ := body["check"].(map[string]any)
		out["body"] = body["body"]
		out["bodyVerified"] = check["verified"]
	} else {
		out["body"] = "not sent to the platform yet"
	}
	var validators []map[string]any
	if list, _ := d["validators"].([]any); list != nil {
		for _, v := range list {
			m, _ := v.(map[string]any)
			entry := pick(m, "validator", "selected", "trusted", "standing")
			if val, _ := m["validation"].(map[string]any); val != nil {
				entry["validation"] = pick(val, "revealed", "reason", "claimSupport", "alignment", "noveltyBps", "scoreBps", "reproduction")
			}
			validators = append(validators, entry)
		}
	}
	out["validators"] = validators
	out["thread"] = d["thread"]
	out["threadState"] = d["threadState"]
	return ok("claim", out)
}

func (r *readers) neighbourhood(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	q := url.Values{"objective": {a.text("objective")}, "roots": {strings.Join(a.list("roots"), ",")}}
	var d any
	if err := r.platform.get(ctx, "/api/claims/neighbourhood", q, &d); err != nil {
		return fail("neighbourhood", err)
	}
	return ok("neighbourhood", d)
}

func (r *readers) colonies(ctx context.Context, _ args) (toolapi.ToolMessage, error) {
	var rows []map[string]any
	if err := r.platform.get(ctx, "/api/colonies", nil, &rows); err != nil {
		return fail("colonies", err)
	}
	followed := r.followed()
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		m := pick(c, "slug", "address", "leader", "admission", "eligible", "trusted")
		m["validationFee"], m["bond"] = c["validationFee"], c["bond"]
		m["following"] = slices.Contains(followed, fmt.Sprint(c["slug"]))
		out = append(out, m)
	}
	return ok("colonies", out)
}

func (r *readers) followTool(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	slug, on := a.text("colony"), a.flag("follow", true)
	if err := followColony(ctx, r.platform, r.follow, slug, on); err != nil {
		return fail("follow", err)
	}
	if on {
		return ok("follow", map[string]any{"following": slug, "all": r.followed()})
	}
	return ok("follow", map[string]any{"stopped": slug, "all": r.followed()})
}

// followColony follows a colony the platform lists, or stops following it:
// for the tool and for the work page alike.
func followColony(ctx context.Context, site *platform, follow func(string, bool) error, slug string, on bool) error {
	var rows []map[string]any
	if err := site.get(ctx, "/api/colonies", nil, &rows); err != nil {
		return err
	}
	if !slices.ContainsFunc(rows, func(c map[string]any) bool { return c["slug"] == slug }) {
		return fmt.Errorf("there is no colony %q", slug)
	}
	return follow(slug, on)
}

func (r *readers) heard(_ context.Context, a args) (toolapi.ToolMessage, error) {
	since := time.Now().Add(-time.Duration(max(a.number("hours", 24), 1)) * time.Hour)
	list := r.inbox.since(since, 1000)
	if t := a.text("type"); t != "" {
		list = slices.DeleteFunc(list, func(h heard) bool { return h.Type != t })
	}
	if n := min(max(a.number("limit", 50), 1), 200); len(list) > n {
		list = list[len(list)-n:]
	}
	if len(list) == 0 {
		return toolapi.ToolEmpty("heard", fmt.Sprintf("nothing heard in that time from %v", r.followed())), nil
	}
	return ok("heard", list)
}

func (r *readers) duties(ctx context.Context, _ args) (toolapi.ToolMessage, error) {
	var d any
	if err := r.platform.get(ctx, "/api/claims/duties", url.Values{"validator": {r.me.String()}}, &d); err != nil {
		return fail("duties", err)
	}
	return ok("duties", d)
}

var tokenPrograms = []string{"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"}

func (r *readers) wallet(ctx context.Context, _ args) (toolapi.ToolMessage, error) {
	lamports, err := r.rpc.Balance(ctx, r.me)
	if err != nil {
		return fail("wallet", err)
	}
	out := map[string]any{"address": r.me.String(), "sol": whole(fmt.Sprint(lamports), float64(9))}
	var tokens []map[string]any
	for _, p := range tokenPrograms {
		program, _ := solana.ParseKey(p)
		balances, err := r.rpc.TokenBalances(ctx, r.me, program)
		if err != nil {
			return fail("wallet", err)
		}
		for _, b := range balances {
			tokens = append(tokens, map[string]any{"mint": b.Mint, "amount": whole(b.Amount, float64(b.Decimals))})
		}
	}
	out["tokens"] = tokens
	return ok("wallet", out)
}
