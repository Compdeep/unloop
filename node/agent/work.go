package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/chain"
	"github.com/Compdeep/unloop/node/envelope"
	"github.com/Compdeep/unloop/node/solana"
)

// The agent's working tools (docs/PLATFORM.md section 7): drafting and
// filing a claim, the thread, validating, collecting. Drafting reads; every
// tool that would sign proposes an action instead (approvals.go), and the
// executors below carry it out once it is approved. What they sign they sign
// with the agent's own wallet, through node/chain and the agent's own node.
type worker struct {
	dir       string
	key       ed25519.PrivateKey
	me        solana.Key
	program   *solana.Program
	chain     *chain.Client
	node      *bus.Node
	platform  *platform
	approvals *approvals
	chat      string

	mu       sync.Mutex
	decimals int // the protocol token's; -1 until read
}

// ── the files the worker keeps ─────────────────────────────────────────────

// draft is a claim drafted and not filed: kept so what is filed is exactly
// what was drafted, whatever the model says later.
type draft struct {
	Objective string         `json:"objective"`
	Colony    string         `json:"colony"`
	Title     string         `json:"title"`
	Mode      string         `json:"mode"`
	Claim     string         `json:"claim"`
	Fee       string         `json:"fee"`
	Body      map[string]any `json:"body"`
}

// sealedFinding is a finding committed to: kept before the commitment is
// sent, because nothing else can open it.
type sealedFinding struct {
	Claim      string         `json:"claim"`
	Colony     string         `json:"colony"`
	Message    map[string]any `json:"message"`
	Nonce      string         `json:"nonce"`
	Commitment string         `json:"commitment"`
	Kept       time.Time      `json:"kept"`
}

// filed is a claim this agent filed: the thread it answers in.
type filed struct {
	Claim     string `json:"claim"`
	Objective string `json:"objective"`
	Colony    string `json:"colony"`
	Hash      string `json:"hash"`
}

func (w *worker) keep(sub, name string, v any) error {
	dir := filepath.Join(w.dir, sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".json"), b, 0o600)
}

func (w *worker) read(sub, name string, v any) error {
	b, err := os.ReadFile(filepath.Join(w.dir, sub, name+".json"))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// mine is the claims this agent filed.
func (w *worker) mine() []filed {
	var out []filed
	entries, _ := os.ReadDir(filepath.Join(w.dir, "claims"))
	for _, e := range entries {
		var f filed
		if w.read("claims", strings.TrimSuffix(e.Name(), ".json"), &f) == nil {
			out = append(out, f)
		}
	}
	return out
}

// ── helpers ────────────────────────────────────────────────────────────────

// decoded is v as envelope.Decode reads JSON, which is what the canonical
// encoding and the claim hash take.
func decoded(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	d, err := envelope.Decode(b)
	if err != nil {
		return nil, err
	}
	m, ok := d.(map[string]any)
	if !ok {
		return nil, errors.New("not an object")
	}
	return m, nil
}

// shown is an amount of the protocol token in whole tokens.
func (w *worker) shown(n *big.Int) string {
	return whole(n.String(), float64(w.protocolDecimals(context.Background())))
}

func (w *worker) protocolDecimals(ctx context.Context) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.decimals >= 0 {
		return w.decimals
	}
	t, err := w.program.ProtocolToken(ctx)
	if err != nil || t == nil {
		return 9
	}
	if d, err := w.program.MintDecimals(ctx, t.Mint); err == nil {
		w.decimals = d
	}
	return max(w.decimals, 0)
}

// limit is the day's limit in the protocol token's smallest unit.
func (w *worker) limit(dailyLimit string) func(context.Context) (*big.Int, error) {
	return func(ctx context.Context) (*big.Int, error) {
		r, ok := new(big.Rat).SetString(dailyLimit)
		if !ok {
			return nil, fmt.Errorf("daily_limit %q", dailyLimit)
		}
		r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(w.protocolDecimals(ctx))), nil)))
		return new(big.Int).Quo(r.Num(), r.Denom()), nil
	}
}

// publish signs a message as the agent and sends it from the agent's node,
// which judges it as it judges every message.
func (w *worker) publish(typ, colony string, ref *envelope.ChainRef, body map[string]any) (string, error) {
	e, err := envelope.New(typ, colony, w.me.String(), w.node.ID().String(), ref, body)
	if err != nil {
		return "", err
	}
	wire, err := json.Marshal(envelope.Sign(e, w.key))
	if err != nil {
		return "", err
	}
	return w.node.Send(context.Background(), wire)
}

// wellFormed holds a message the agent would send to the schemas now, so a
// draft that would be refused is refused while it is still a draft.
func (w *worker) wellFormed(typ, colony string, chained bool, body map[string]any) error {
	var ref *envelope.ChainRef
	if chained {
		ref = &envelope.ChainRef{Tx: strings.Repeat("1", 88)}
	}
	e, err := envelope.New(typ, colony, w.me.String(), w.node.ID().String(), ref, body)
	if err != nil {
		return err
	}
	wire, _ := json.Marshal(envelope.Sign(e, w.key))
	if _, err := envelope.Check(wire); err != nil {
		return err
	}
	return nil
}

// claimRead is what a thread tool needs of a claim, from the platform.
type claimRead struct {
	Claim struct {
		Address, Objective, Agent, Hash, Status string
	} `json:"claim"`
	Colony struct {
		Slug string `json:"slug"`
	} `json:"colony"`
	Validators []struct {
		Validator  string         `json:"validator"`
		Validation map[string]any `json:"validation"`
	} `json:"validators"`
	ThreadState struct {
		Next struct {
			Seq, Answering, Submission int
		} `json:"next"`
		Open               []int `json:"open"`
		ClarificationsLeft int   `json:"clarificationsLeft"`
	} `json:"threadState"`
}

func (w *worker) readClaim(ctx context.Context, address string) (*claimRead, error) {
	if _, err := solana.ParseKey(address); err != nil {
		return nil, err
	}
	var c claimRead
	if err := w.platform.get(ctx, "/api/claims/"+address, url.Values{}, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func short(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func evidenceItems(a args, k string) []map[string]string {
	var out []map[string]string
	list, _ := a[k].([]any)
	for _, x := range list {
		m, _ := x.(map[string]any)
		kind, _ := m["kind"].(string)
		statement, _ := m["statement"].(string)
		provenance, _ := m["provenance"].(string)
		if strings.TrimSpace(statement) != "" {
			out = append(out, map[string]string{"kind": kind, "statement": strings.TrimSpace(statement), "provenance": strings.TrimSpace(provenance)})
		}
	}
	return out
}

// evidenceSet groups items by kind, every kind present, ids numbered by kind
// with prefix before them — "" for a claim (D1, P1, S1), "A2" for the second
// amendment — so no two collide (chain/client/thread.ts, amendmentEvidence).
func evidenceSet(prefix string, items []map[string]string) (map[string]any, error) {
	set := map[string]any{}
	for _, kind := range []string{"deterministic", "procedural", "semantic"} {
		list := []any{}
		for _, it := range items {
			if it["kind"] != kind {
				continue
			}
			if it["provenance"] == "" {
				return nil, fmt.Errorf("every piece of evidence says where it came from: %q does not", short(it["statement"], 40))
			}
			list = append(list, map[string]any{"id": fmt.Sprintf("%s%s%d", prefix, strings.ToUpper(kind[:1]), len(list)+1),
				"kind": kind, "statement": it["statement"], "provenance": it["provenance"]})
		}
		set[kind] = list
	}
	for _, it := range items {
		if it["kind"] != "deterministic" && it["kind"] != "procedural" && it["kind"] != "semantic" {
			return nil, fmt.Errorf("evidence of kind %q: the kinds are deterministic, procedural and semantic", it["kind"])
		}
	}
	return set, nil
}

// ── the tools ──────────────────────────────────────────────────────────────

// impact is a tool's authority, by what it proposes: the engine's gate holds
// a run to it, and the approvals page holds the action.
type working struct {
	tool
	impact int
}

func (t working) Impact(map[string]any) int { return t.impact }

func (w *worker) tools() []toolapi.Tool {
	evidence := `{"type":"array","items":{"type":"object","properties":{"kind":{"type":"string","enum":["deterministic","procedural","semantic"]},"statement":{"type":"string"},"provenance":{"type":"string","description":"where it came from: a run, a file, a source"}},"required":["kind","statement","provenance"]}}`
	return []toolapi.Tool{
		working{tool{"unloop_draft_claim",
			"Draft a claim on an Unloop objective from the person's results: the assertion (what was found; under a deterministic objective, exactly the canonical result), why it matters, the evidence a validator needs to check it, and the earlier claims it builds on. Computes its hash under the objective's mode, checks it against the protocol's schema, and keeps it. Sends nothing; file it with unloop_file_claim.",
			`{"type":"object","properties":{
"objective":{"type":"string"},"awardIndex":{"type":"integer","description":"which award, 0 for the main one"},
"assertion":{"type":"string"},"significance":{"type":"string"},
"evidence":` + evidence + `,
"references":{"type":"array","items":{"type":"string"}},
"roots":{"type":"array","items":{"type":"string"},"description":"addresses of earlier claims in this objective it builds on, at most 8"}},
"required":["objective","assertion","significance","evidence"]}`,
			w.draftClaim}, toolapi.ImpactObserve},
		working{tool{"unloop_file_claim",
			"File a drafted claim: record it on chain, which pays the colony's validation fee, fix who judges it, send its body, and publish it on the bus. Proposed for the person's approval unless their autonomy covers it.",
			`{"type":"object","properties":{"hash":{"type":"string","description":"the draft's hash, from unloop_draft_claim"}},"required":["hash"]}`,
			w.fileClaim}, toolapi.ImpactControl},
		working{tool{"unloop_object",
			"As a validator of a claim, say where checking it stopped (cannot_verify): a blocker and what was tried. Posted to the claim's thread on the bus.",
			`{"type":"object","properties":{"claim":{"type":"string"},
"blocker":{"type":"string","enum":["step_missing","not_reproducible","ambiguous_notation","insufficient_evidence","out_of_reach"],"description":"out_of_reach: this validator lacks the means, which says nothing against the claim"},
"detail":{"type":"string"}},"required":["claim","blocker","detail"]}`,
			w.object}, toolapi.ImpactAffect},
		working{tool{"unloop_answer",
			"As a claim's author, answer objections in its thread (clarification): explain what the claim already holds. Changes nothing the claim committed to; missing evidence goes in unloop_amend.",
			`{"type":"object","properties":{"claim":{"type":"string"},"addresses":{"type":"array","items":{"type":"integer"},"description":"the objection numbers answered"},"detail":{"type":"string"}},"required":["claim","addresses","detail"]}`,
			w.answer}, toolapi.ImpactAffect},
		working{tool{"unloop_amend",
			"As a claim's author, add evidence it lacked (amendment), beside the claim and never into it.",
			`{"type":"object","properties":{"claim":{"type":"string"},"evidence":` + evidence + `,"detail":{"type":"string","description":"what this adds and why it was not in the claim"}},"required":["claim","evidence","detail"]}`,
			w.amend}, toolapi.ImpactAffect},
		working{tool{"unloop_commit",
			"As a validator drawn for a claim, commit to a finding: whether the evidence supports the claim and the objective, novelty and score in basis points, whether it reproduced, and the detail. The finding is sealed and kept here first, then only its commitment goes on chain, so nobody sees it before the reveal. A commitment never opened costs the bond.",
			`{"type":"object","properties":{"claim":{"type":"string"},
"reason":{"type":"string","enum":["none","refused","unavailable","access_denied","timed_out","unsupported","environment","resource_limit","internal"],"description":"none when it was evaluated; otherwise why not"},
"claimSupport":{"type":"string","enum":["pass","fail","insufficient"]},"objectiveAlignment":{"type":"string","enum":["pass","fail","insufficient"]},
"noveltyBps":{"type":"integer","minimum":0,"maximum":10000},"scoreBps":{"type":"integer","minimum":0,"maximum":10000},
"reproduction":{"type":"string","enum":["not_attempted","pass","fail"]},
"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","description":"the evidence item's id, e.g. P1"},"holds":{"type":"string","enum":["pass","fail","insufficient"]},"supportsClaim":{"type":"string","enum":["pass","fail","insufficient"]},"note":{"type":"string"}},"required":["id","holds","supportsClaim"]},"description":"a finding per evidence item checked; none when it was not evaluated"},
"rationale":{"type":"string"}},
"required":["claim","reason","claimSupport","objectiveAlignment","noveltyBps","scoreBps","reproduction"]}`,
			w.commit}, toolapi.ImpactControl},
		working{tool{"unloop_reveal",
			"Open a finding this agent committed to, once the commit window has closed, and publish it on the bus.",
			`{"type":"object","properties":{"claim":{"type":"string"}},"required":["claim"]}`,
			w.reveal}, toolapi.ImpactControl},
		working{tool{"unloop_collect",
			"Collect what an objective's escrow holds for this agent — its awards — less the protocol's share, into its wallet.",
			`{"type":"object","properties":{"objective":{"type":"string"}},"required":["objective"]}`,
			w.collect}, toolapi.ImpactControl},
		working{tool{"unloop_collect_fee",
			"As a validator who opened its finding on a claim, collect its share of the claim's validation fee.",
			`{"type":"object","properties":{"claim":{"type":"string"}},"required":["claim"]}`,
			w.collectFee}, toolapi.ImpactControl},
		w.settleTool(),
		w.approvals.tool(),
	}
}

// all is every working tool: the claimant's and validator's above, and the leader's.
func (w *worker) all() []toolapi.Tool { return append(w.tools(), w.leadingTools()...) }

func (w *worker) propose(kind string, impact int, summary string, detail any, spend *big.Int, params any) (toolapi.ToolMessage, error) {
	p, err := json.Marshal(params)
	if err != nil {
		return fail(kind, err)
	}
	act := &Action{Kind: kind, Impact: impact, Summary: summary, Detail: detail, Params: p}
	if spend != nil && spend.Sign() > 0 {
		act.Spend, act.Shown = spend.String(), w.shown(spend)
	}
	return toolapi.ToolOK(kind, w.approvals.propose(act, w.chat), map[string]any{"action": act.ID}), nil
}

func (w *worker) draftClaim(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	objective := a.text("objective")
	o, err := solana.ParseKey(objective)
	if err != nil {
		return fail("draft", err)
	}
	var d struct {
		Colony struct {
			Slug string `json:"slug"`
		} `json:"colony"`
		Spec *struct {
			Verified bool           `json:"verified"`
			Body     map[string]any `json:"body"`
		} `json:"spec"`
	}
	if err := w.platform.get(ctx, "/api/objectives/"+objective, nil, &d); err != nil {
		return fail("draft", err)
	}
	if d.Spec == nil || !d.Spec.Verified {
		return fail("draft", errors.New("this objective's terms are not verified, so how its claims are hashed is unknown; nothing can be drafted against it"))
	}
	mode, _ := d.Spec.Body["validationMode"].(string)
	title, _ := d.Spec.Body["title"].(string)
	account, err := w.program.Objective(ctx, o)
	if err != nil || account == nil {
		return fail("draft", fmt.Errorf("the objective's account: %v", err))
	}
	if time.Now().Unix() >= account.ClaimDeadline {
		return fail("draft", errors.New("this objective has stopped taking claims"))
	}
	fee, err := w.program.ValidationFee(ctx, account.Colony)
	if err != nil {
		return fail("draft", err)
	}
	items := evidenceItems(a, "evidence")
	if len(items) == 0 {
		return fail("draft", errors.New("a claim carries at least one piece of evidence: what a validator checks it by"))
	}
	evidence, err := evidenceSet("", items)
	if err != nil {
		return fail("draft", err)
	}
	roots := a.list("roots")
	if len(roots) > 8 {
		return fail("draft", errors.New("a claim builds on at most 8 earlier claims"))
	}
	fields := map[string]any{
		"objective": objective, "awardIndex": a.number("awardIndex", 0), "assertion": a.text("assertion"),
		"significance": a.text("significance"), "evidence": evidence, "roots": append([]string{}, roots...),
	}
	if refs := a.list("references"); len(refs) > 0 {
		fields["references"] = refs
	}
	body, err := decoded(fields)
	if err != nil {
		return fail("draft", err)
	}
	hash, err := chain.ClaimHash(body, mode)
	if err != nil {
		return fail("draft", err)
	}
	body["claimHash"] = hash
	if err := w.wellFormed("claim.publish", d.Colony.Slug, true, body); err != nil {
		return fail("draft", fmt.Errorf("the protocol would refuse this claim: %w", err))
	}
	h, _ := hex.DecodeString(hash)
	claim := w.program.Address([]byte("claim"), o[:], h, w.me[:])
	total := new(big.Int).Mul(new(big.Int).SetUint64(fee), big.NewInt(int64(account.ValidatorsPerClaim)))
	if err := w.keep("drafts", hash, draft{Objective: objective, Colony: d.Colony.Slug, Title: title, Mode: mode,
		Claim: claim.String(), Fee: total.String(), Body: body}); err != nil {
		return fail("draft", err)
	}
	return ok("draft", map[string]any{"hash": hash, "claim": claim.String(), "mode": mode, "fee": w.shown(total) + " protocol tokens",
		"body": body, "next": "show the person the draft; file it with unloop_file_claim when they want it filed"})
}

func (w *worker) fileClaim(_ context.Context, a args) (toolapi.ToolMessage, error) {
	var d draft
	if err := w.read("drafts", a.text("hash"), &d); err != nil {
		return fail("file", fmt.Errorf("no draft %q: draft it first with unloop_draft_claim", a.text("hash")))
	}
	fee, _ := new(big.Int).SetString(d.Fee, 10)
	assertion, _ := d.Body["assertion"].(string)
	return w.propose("file_claim", toolapi.ImpactControl,
		fmt.Sprintf("File a claim on %q: %s", d.Title, short(assertion, 90)),
		map[string]any{"objective": d.Objective, "claim": d.Claim, "validationFee": w.shown(fee), "body": d.Body},
		fee, map[string]string{"hash": a.text("hash")})
}

func (w *worker) object(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	c, err := w.readClaim(ctx, a.text("claim"))
	if err != nil {
		return fail("object", err)
	}
	body := map[string]any{"claimHash": c.Claim.Hash, "seq": c.ThreadState.Next.Seq, "answering": c.ThreadState.Next.Answering,
		"blocker": a.text("blocker"), "detail": a.text("detail")}
	return w.thread(c, "cannot_verify", fmt.Sprintf("Object to claim %s: %s", short(c.Claim.Address, 8), short(a.text("detail"), 90)), body)
}

func (w *worker) answer(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	c, err := w.readClaim(ctx, a.text("claim"))
	if err != nil {
		return fail("answer", err)
	}
	if c.Claim.Agent != w.me.String() {
		return fail("answer", errors.New("only a claim's author answers in its thread, and this agent did not file it"))
	}
	if c.ThreadState.ClarificationsLeft == 0 {
		return fail("answer", errors.New("this thread has had all the answers the colony allows"))
	}
	var addresses []int
	if list, _ := a["addresses"].([]any); list != nil {
		for _, x := range list {
			if n := (args{"n": x}).number("n", -1); n >= 0 {
				addresses = append(addresses, n)
			}
		}
	}
	if len(addresses) == 0 {
		return fail("answer", fmt.Errorf("say which objections this answers; open: %v", c.ThreadState.Open))
	}
	body := map[string]any{"claimHash": c.Claim.Hash, "seq": c.ThreadState.Next.Seq, "addresses": addresses, "detail": a.text("detail")}
	return w.thread(c, "clarification", fmt.Sprintf("Answer objections %v on your claim %s", addresses, short(c.Claim.Address, 8)), body)
}

func (w *worker) amend(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	c, err := w.readClaim(ctx, a.text("claim"))
	if err != nil {
		return fail("amend", err)
	}
	if c.Claim.Agent != w.me.String() {
		return fail("amend", errors.New("only a claim's author amends it, and this agent did not file it"))
	}
	n := c.ThreadState.Next.Submission
	items := evidenceItems(a, "evidence")
	if len(items) == 0 {
		return fail("amend", errors.New("an amendment adds evidence, and this adds none"))
	}
	evidence, err := evidenceSet(fmt.Sprintf("A%d", n), items)
	if err != nil {
		return fail("amend", err)
	}
	body := map[string]any{"claimHash": c.Claim.Hash, "submission": n, "evidence": evidence, "detail": a.text("detail")}
	return w.thread(c, "amendment", fmt.Sprintf("Amend your claim %s (submission %d): %s", short(c.Claim.Address, 8), n, short(a.text("detail"), 80)), body)
}

// thread proposes a thread message, once the schemas would take it.
func (w *worker) thread(c *claimRead, typ, summary string, fields map[string]any) (toolapi.ToolMessage, error) {
	body, err := decoded(fields)
	if err != nil {
		return fail(typ, err)
	}
	if err := w.wellFormed(typ, c.Colony.Slug, false, body); err != nil {
		return fail(typ, fmt.Errorf("the protocol would refuse it: %w", err))
	}
	return w.propose("publish", toolapi.ImpactAffect, summary, body, nil,
		map[string]any{"type": typ, "colony": c.Colony.Slug, "body": body})
}

func (w *worker) commit(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	c, err := w.readClaim(ctx, a.text("claim"))
	if err != nil {
		return fail("commit", err)
	}
	var existing sealedFinding
	if w.read("sealed", c.Claim.Address, &existing) == nil {
		return fail("commit", errors.New("this agent has already sealed a finding on this claim; a commitment is made once"))
	}
	fields := map[string]any{"claimHash": c.Claim.Hash}
	for _, k := range []string{"reason", "claimSupport", "objectiveAlignment", "reproduction", "rationale"} {
		if s := a.text(k); s != "" {
			fields[k] = s
		}
	}
	fields["noveltyBps"], fields["scoreBps"] = a.number("noveltyBps", 0), a.number("scoreBps", 0)
	// Always a list, empty where nothing was evaluated: the schema requires it.
	fields["items"] = []any{}
	if items, ok := a["items"].([]any); ok {
		fields["items"] = items
	}
	m, err := decoded(fields)
	if err != nil {
		return fail("commit", err)
	}
	if m["finding"], err = chain.FindingHash(m); err != nil {
		return fail("commit", err)
	}
	f, err := chain.ReadFinding(m)
	if err != nil {
		return fail("commit", err)
	}
	if err := w.wellFormed("validation.publish", c.Colony.Slug, false, m); err != nil {
		return fail("commit", fmt.Errorf("the protocol would refuse this finding: %w", err))
	}
	var nonce [32]byte
	rand.Read(nonce[:])
	claim, _ := solana.ParseKey(c.Claim.Address)
	commitment, err := chain.Commitment(claim, w.me, f, nonce)
	if err != nil {
		return fail("commit", err)
	}
	// Kept before anything is proposed: without it the commitment cannot be opened.
	if err := w.keep("sealed", c.Claim.Address, sealedFinding{Claim: c.Claim.Address, Colony: c.Colony.Slug, Message: m,
		Nonce: hex.EncodeToString(nonce[:]), Commitment: hex.EncodeToString(commitment[:]), Kept: time.Now().UTC()}); err != nil {
		return fail("commit", err)
	}
	return w.propose("commit", toolapi.ImpactControl, fmt.Sprintf("Commit to a finding on claim %s (%s/%s)", short(c.Claim.Address, 8), f.ClaimSupport, f.ObjectiveAlignment),
		map[string]any{"claim": c.Claim.Address, "finding": m, "commitment": hex.EncodeToString(commitment[:])}, nil,
		map[string]string{"claim": c.Claim.Address})
}

func (w *worker) reveal(_ context.Context, a args) (toolapi.ToolMessage, error) {
	var s sealedFinding
	if err := w.read("sealed", a.text("claim"), &s); err != nil {
		return fail("reveal", errors.New("this agent holds no sealed finding on that claim"))
	}
	return w.propose("reveal", toolapi.ImpactControl, fmt.Sprintf("Open the finding on claim %s and publish it", short(s.Claim, 8)),
		map[string]any{"claim": s.Claim, "finding": s.Message}, nil, map[string]string{"claim": s.Claim})
}

func (w *worker) collect(_ context.Context, a args) (toolapi.ToolMessage, error) {
	if _, err := solana.ParseKey(a.text("objective")); err != nil {
		return fail("collect", err)
	}
	return w.propose("collect", toolapi.ImpactControl, "Collect this agent's awards from objective "+short(a.text("objective"), 8),
		map[string]any{"objective": a.text("objective")}, nil, map[string]string{"objective": a.text("objective")})
}

func (w *worker) collectFee(_ context.Context, a args) (toolapi.ToolMessage, error) {
	if _, err := solana.ParseKey(a.text("claim")); err != nil {
		return fail("collect", err)
	}
	return w.propose("collect_fee", toolapi.ImpactControl, "Collect this validator's fee share on claim "+short(a.text("claim"), 8),
		map[string]any{"claim": a.text("claim")}, nil, map[string]string{"claim": a.text("claim")})
}

// ── carrying an approved action out ────────────────────────────────────────

func (w *worker) run(ctx context.Context, act *Action) (string, error) {
	var p map[string]any
	json.Unmarshal(act.Params, &p)
	text := func(k string) string { s, _ := p[k].(string); return s }
	key := func(k string) solana.Key { k2, _ := solana.ParseKey(text(k)); return k2 }
	send := func(ixs []chain.Instruction, err error) (chain.Landed, error) {
		if err != nil {
			return chain.Landed{}, err
		}
		return w.chain.Send(ctx, w.key, ixs)
	}
	switch act.Kind {
	case "file_claim":
		return w.file(ctx, text("hash"))
	case "create_objective":
		return w.create(ctx, text("draft"))
	case "settle":
		return w.settle(ctx, act.Params)
	case "publish":
		body, _ := p["body"].(map[string]any)
		m, err := decoded(body)
		if err != nil {
			return "", err
		}
		id, err := w.publish(text("type"), text("colony"), nil, m)
		if err != nil {
			return "", err
		}
		return "on the bus as " + id, nil
	case "commit":
		var s sealedFinding
		if err := w.read("sealed", text("claim"), &s); err != nil {
			return "", err
		}
		var commitment [32]byte
		b, _ := hex.DecodeString(s.Commitment)
		copy(commitment[:], b)
		landed, err := send(w.chain.Commit(ctx, key("claim"), w.me, commitment))
		if err != nil {
			return "", err
		}
		return "committed in " + landed.Signature, nil
	case "reveal":
		var s sealedFinding
		if err := w.read("sealed", text("claim"), &s); err != nil {
			return "", err
		}
		m, err := decoded(s.Message)
		if err != nil {
			return "", err
		}
		f, err := chain.ReadFinding(m)
		if err != nil {
			return "", err
		}
		var nonce [32]byte
		b, _ := hex.DecodeString(s.Nonce)
		copy(nonce[:], b)
		landed, err := send(w.chain.Reveal(ctx, key("claim"), w.me, f, nonce))
		if err != nil {
			return "", err
		}
		result := "revealed in " + landed.Signature
		id, err := w.publish("validation.publish", s.Colony, nil, m)
		if err != nil {
			return result, fmt.Errorf("not published on the bus: %w", err)
		}
		return result + "; on the bus as " + id, nil
	case "collect":
		landed, err := send(w.chain.Collect(ctx, w.me, key("objective")))
		if err != nil {
			return "", err
		}
		return "collected in " + landed.Signature, nil
	case "withdraw":
		amount, _ := new(big.Int).SetString(text("amount"), 10)
		if amount == nil || !amount.IsUint64() {
			return "", fmt.Errorf("not an amount: %q", text("amount"))
		}
		landed, err := send(w.chain.Withdraw(ctx, key("objective"), w.me, amount.Uint64()))
		if err != nil {
			return "", err
		}
		return "taken back in " + landed.Signature, nil
	case "collect_fee":
		landed, err := send(w.chain.CollectFee(ctx, w.me, key("claim")))
		if err != nil {
			return "", err
		}
		return "collected in " + landed.Signature, nil
	}
	return "", fmt.Errorf("no action %q", act.Kind)
}

// sendWhenIndexed posts a document to the platform, which keeps one only for
// an account its index has seen and answers 404 until then: asked again for
// three minutes. It says what became of it.
func (w *worker) sendWhenIndexed(ctx context.Context, path string, body any, what string) string {
	for i := 0; i < 45; i++ {
		status, err := w.platform.post(ctx, path, body, nil)
		switch {
		case err == nil:
			return what + " sent to the platform"
		case status != 404:
			return what + " not sent: " + err.Error()
		}
		select {
		case <-ctx.Done():
			return what + " not sent: " + ctx.Err().Error()
		case <-time.After(4 * time.Second):
		}
	}
	return what + " not sent: the platform's index has not seen the account"
}

// file is a claim from its draft to the bus, in the order the protocol needs:
// recorded first, so nobody who reads it can file it ahead; then seeded, so
// its validators are fixed before the block that seeds it is forgotten; then
// its body to the platform, once the index has it; then on the bus.
func (w *worker) file(ctx context.Context, hash string) (string, error) {
	var d draft
	if err := w.read("drafts", hash, &d); err != nil {
		return "", err
	}
	objective, _ := solana.ParseKey(d.Objective)
	claim, _ := solana.ParseKey(d.Claim)
	var h [32]byte
	b, _ := hex.DecodeString(hash)
	copy(h[:], b)
	var roots []solana.Key
	if list, _ := d.Body["roots"].([]any); list != nil {
		for _, r := range list {
			k, _ := solana.ParseKey(fmt.Sprint(r))
			roots = append(roots, k)
		}
	}
	award, _ := d.Body["awardIndex"].(float64)
	fee, _ := new(big.Int).SetString(d.Fee, 10)
	ixs, err := w.chain.File(ctx, w.me, objective, h, uint8(award), roots, fee.Uint64())
	if err != nil {
		return "", err
	}
	landed, err := w.chain.Send(ctx, w.key, ixs)
	if err != nil {
		return "", err
	}
	w.keep("claims", d.Claim, filed{Claim: d.Claim, Objective: d.Objective, Colony: d.Colony, Hash: hash})
	steps := []string{fmt.Sprintf("recorded in %s (slot %d)", landed.Signature, landed.Slot)}

	// The seed exists a few blocks after the claim; the program refuses until then.
	var seedErr error
	for i := 0; i < 40; i++ {
		if ixs, err := w.chain.Seed(ctx, w.me, claim); err == nil {
			if _, seedErr = w.chain.Send(ctx, w.key, ixs); seedErr == nil {
				steps = append(steps, "its validators fixed")
				break
			}
		}
		select {
		case <-ctx.Done():
			return strings.Join(steps, "; "), ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if seedErr != nil {
		steps = append(steps, "not seeded: "+seedErr.Error()+" (anybody may seed it; unseeded, it expires)")
	}

	steps = append(steps, w.sendWhenIndexed(ctx, "/api/claims/"+d.Claim+"/body", d.Body, "its body"))
	id, err := w.publish("claim.publish", d.Colony, &envelope.ChainRef{Tx: landed.Signature, Slot: landed.Slot}, d.Body)
	if err != nil {
		return strings.Join(steps, "; "), fmt.Errorf("not published on the bus: %w", err)
	}
	steps = append(steps, "on the bus as "+id)
	return strings.Join(steps, "; "), nil
}
