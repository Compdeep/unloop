package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/chain"
	"github.com/Compdeep/unloop/node/envelope"
	"github.com/Compdeep/unloop/node/solana"
)

// Leading through the agent (docs/PLATFORM.md section 7): an objective drafted
// in conversation, with both of its hashes, then either handed to the site's
// create page, filled in, for the leader's browser wallet to sign, or created
// from the agent's own wallet where that wallet leads the colony. Creating
// one is always the person's to approve, whatever the autonomy.
//
// The draft is built as the site's create page builds it from its form
// (platform/web/src/views/NewObjectiveView.vue, body), so the page, filled in
// from the hand-over, computes the same two hashes and says so.

// objectiveDraft is an objective drafted and not created, kept under its
// criteria hash so what is created is exactly what was drafted.
type objectiveDraft struct {
	Colony         string         `json:"colony"`
	Slug           string         `json:"slug"`
	Mint           string         `json:"mint"`
	Decimals       int            `json:"decimals"`
	Body           map[string]any `json:"body"`
	Awards         []chain.Award  `json:"awards"`
	CommitDeadline int64          `json:"commitDeadline"`
	Pool           uint64         `json:"pool"`
	Handover       string         `json:"handover"`
}

var predicateID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// baseUnits is an amount in whole tokens, as text, in the mint's smallest unit.
func baseUnits(s string, decimals int) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	if !r.IsInt() {
		return 0, fmt.Errorf("%s has more than the %d decimal places this token has", s, decimals)
	}
	if !r.Num().IsUint64() {
		return 0, fmt.Errorf("%s is more than an account can hold", s)
	}
	return r.Num().Uint64(), nil
}

// isoTime is a time as the site writes one: the browser's toISOString.
func isoTime(unix int64) string { return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05.000Z") }

// deadline is an RFC 3339 time, or the default days from now.
func deadline(s string, days int) (int64, error) {
	if s = strings.TrimSpace(s); s == "" {
		return time.Now().Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Minute).Unix(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, fmt.Errorf("%q is not an RFC 3339 time, like 2026-11-01T12:00:00Z", s)
	}
	return t.Unix(), nil
}

func (w *worker) leadingTools() []toolapi.Tool {
	return []toolapi.Tool{
		working{tool{"unloop_draft_objective",
			"Draft an Unloop objective: a funded request in a colony, paying whoever finds what it asks for. Builds its document as the site's create page would, computes both hashes its validators will check against, and keeps it. Sends nothing. Returns a link that opens the site's create page filled in, for the colony leader's browser wallet to sign; where this agent's own wallet leads the colony, unloop_create_objective can create it from here.",
			`{"type":"object","properties":{
"colony":{"type":"string","description":"the colony's slug"},
"title":{"type":"string"},"statement":{"type":"string","description":"what is being asked for, plainly"},
"payout":{"type":"string","enum":["prize","rate"],"description":"prize: a finite thing to find, each award paid once; rate: an open class of findings, paid per discovery until the pool runs out"},
"validationMode":{"type":"string","enum":["deterministic","procedural","semantic"],"description":"deterministic: a fixed procedure derives a measurement and a predicate judges it; procedural: the claimant supplies the method; semantic: validators judge against the predicates as a rubric"},
"claimForm":{"type":"object","properties":{"name":{"type":"string"},"spec":{"type":"string","description":"where the format is defined, or the definition"},"envelope":{"type":"array","items":{"type":"string"},"description":"the parts every claim must carry; not under deterministic"}},"required":["name","spec"]},
"predicates":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","description":"P1, P2, …"},"proposition":{"type":"string"},"kind":{"type":"string","enum":["required","scored"]},"weight":{"type":"integer","minimum":1,"maximum":100,"description":"scored only, default 1"}},"required":["proposition","kind"]}},
"requiredPass":{"type":"boolean","description":"every required predicate must pass; default true"},
"minScore":{"type":"number","description":"the lowest score admitted, a percentage; optional"},
"awards":{"type":"array","maxItems":3,"items":{"type":"object","properties":{"title":{"type":"string"},"criteria":{"type":"string"},"mode":{"type":"string","enum":["winnerTakeAll","graded"]},"amount":{"type":"string","description":"in whole tokens of the colony's currency: the prize, or the rate per discovery"}},"required":["title","criteria","mode","amount"]}},
"pool":{"type":"string","description":"whole tokens put in at creation; prizes must fit in it"},
"validatorsPerClaim":{"type":"integer","description":"default: the colony's least, and at least 5"},
"claimDeadline":{"type":"string","description":"RFC 3339; default 14 days from now"},
"commitDeadline":{"type":"string","description":"RFC 3339; default 21 days from now"},
"validationDeadline":{"type":"string","description":"RFC 3339; default 28 days from now"}},
"required":["colony","title","statement","payout","validationMode","claimForm","awards","pool"]}`,
			w.draftObjective}, toolapi.ImpactObserve},
		working{tool{"unloop_create_objective",
			"Create and fund a drafted objective from this agent's wallet, which must lead its colony; then publish its document. Always waits for the person's approval, whatever the autonomy. Where this wallet does not lead the colony, give the person the draft's link to the site instead.",
			`{"type":"object","properties":{"draft":{"type":"string","description":"the draft's criteria hash, from unloop_draft_objective"}},"required":["draft"]}`,
			w.createObjective}, toolapi.ImpactControl},
		working{tool{"unloop_withdraw",
			"Take back what an objective's pool holds that no claim is owed — all of it, or an amount in whole tokens of its currency — into this agent's wallet, which must lead the objective's colony. Proposed for the person's approval unless their autonomy covers it.",
			`{"type":"object","properties":{"objective":{"type":"string"},"amount":{"type":"string","description":"whole tokens; default: everything withdrawable now"}},"required":["objective"]}`,
			w.withdraw}, toolapi.ImpactControl},
	}
}

func (w *worker) draftObjective(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	slug := strings.TrimSpace(a.text("colony"))
	address := w.program.Address([]byte("colony"), []byte(slug))
	col, err := w.program.Colony(ctx, address)
	if err != nil {
		return fail("draft", err)
	}
	if col == nil {
		return fail("draft", fmt.Errorf("there is no colony %q", slug))
	}
	decimals, err := w.program.MintDecimals(ctx, col.PaymentMint)
	if err != nil {
		return fail("draft", fmt.Errorf("the colony's currency: %w", err))
	}

	var problems []string
	problem := func(f string, v ...any) { problems = append(problems, fmt.Sprintf(f, v...)) }
	title, statement := strings.TrimSpace(a.text("title")), strings.TrimSpace(a.text("statement"))
	payout, mode := a.text("payout"), a.text("validationMode")
	if title == "" {
		problem("Give it a title.")
	}
	if statement == "" {
		problem("Say what is being asked for.")
	}
	if payout != "prize" && payout != "rate" {
		problem("The payout is prize or rate.")
	}
	if mode != "deterministic" && mode != "procedural" && mode != "semantic" {
		problem("The validation mode is deterministic, procedural or semantic.")
	}
	form, _ := a["claimForm"].(map[string]any)
	fa := args(form)
	formName, formSpec := strings.TrimSpace(fa.text("name")), strings.TrimSpace(fa.text("spec"))
	if formName == "" || formSpec == "" {
		problem("Name the claim format and say where it is defined.")
	}
	var envelopeParts []string
	if mode != "deterministic" {
		for _, s := range fa.list("envelope") {
			if s = strings.TrimSpace(s); s != "" {
				envelopeParts = append(envelopeParts, s)
			}
		}
	}

	// Predicates, as the page writes them: a weight only where scored.
	var predicates []map[string]any
	seen := map[string]bool{}
	list, _ := a["predicates"].([]any)
	for i, x := range list {
		p := args(asMap(x))
		id := strings.TrimSpace(p.text("id"))
		if id == "" {
			id = fmt.Sprintf("P%d", i+1)
		}
		if !predicateID.MatchString(id) {
			problem("Predicate %d needs an id like P%d.", i+1, i+1)
		}
		if seen[id] {
			problem("Predicate ids must differ: %s is used twice.", id)
		}
		seen[id] = true
		prop := strings.TrimSpace(p.text("proposition"))
		if prop == "" {
			problem("Predicate %s needs a proposition.", id)
		}
		kind := p.text("kind")
		if kind != "required" && kind != "scored" {
			problem("Predicate %s is required or scored.", id)
		}
		m := map[string]any{"id": id, "proposition": prop, "kind": kind}
		if kind == "scored" {
			weight := 1.0
			if v, ok := p["weight"].(float64); ok {
				weight = v
			}
			if weight != float64(int(weight)) || weight < 1 || weight > 100 {
				problem("Predicate %s's weight is a whole number, 1 to 100.", id)
			}
			m["weight"] = int(weight)
		}
		predicates = append(predicates, m)
	}
	var minScoreBps *int64
	if v, ok := a["minScore"].(float64); ok {
		if v < 0 || v > 100 {
			problem("The lowest score admitted is a percentage, 0 to 100.")
		}
		n := int64(v*100 + 0.5)
		minScoreBps = &n
	}

	awardsIn, _ := a["awards"].([]any)
	if len(awardsIn) == 0 || len(awardsIn) > 3 {
		problem("An objective has one to three awards.")
	}
	var awardText []any
	var awards []chain.Award
	var promised uint64
	for i, x := range awardsIn {
		aw := args(asMap(x))
		t, c := strings.TrimSpace(aw.text("title")), strings.TrimSpace(aw.text("criteria"))
		if t == "" || c == "" {
			problem("Award %d needs a title and criteria.", i+1)
		}
		m := aw.text("mode")
		if m != "winnerTakeAll" && m != "graded" {
			problem("Award %d is winnerTakeAll or graded.", i+1)
		}
		amount, err := baseUnits(fmt.Sprint(aw["amount"]), decimals)
		if err != nil {
			problem("Award %d: %v.", i+1, err)
		}
		if payout == "rate" && amount == 0 {
			problem("Award %d pays a rate, so it needs an amount per discovery.", i+1)
		}
		promised += amount
		awardText = append(awardText, map[string]any{"index": i, "title": t, "criteria": c})
		awards = append(awards, chain.Award{Mode: m, Allocation: amount})
	}
	pool, err := baseUnits(a.text("pool"), decimals)
	if err != nil {
		problem("Pool: %v.", err)
	}
	if payout == "prize" && promised > pool {
		problem("The prizes add up to %s, more than the pool of %s.", whole(fmt.Sprint(promised), float64(decimals)), whole(fmt.Sprint(pool), float64(decimals)))
	}

	least := int(col.MinVotes)
	if col.Admission == "public" {
		least *= 3
	}
	panel := a.number("validatorsPerClaim", max(least, 5))
	if panel < least || panel > 255 {
		problem("A claim in %s must select at least %d validators, and at most 255.", slug, least)
	}
	claimAt, err1 := deadline(a.text("claimDeadline"), 14)
	commitAt, err2 := deadline(a.text("commitDeadline"), 21)
	validateAt, err3 := deadline(a.text("validationDeadline"), 28)
	if err := errors.Join(err1, err2, err3); err != nil {
		problem("%v.", err)
	} else {
		if claimAt <= time.Now().Unix() {
			problem("The claim deadline must be in the future.")
		}
		if !(claimAt <= commitAt && commitAt <= validateAt) {
			problem("Deadlines must run claims, then commitments, then validation.")
		}
	}
	if len(problems) > 0 {
		return fail("draft", errors.New(strings.Join(problems, " ")))
	}

	next, err := w.chain.Next(ctx, address)
	if err != nil {
		return fail("draft", err)
	}
	claimForm := map[string]any{"name": formName, "spec": formSpec}
	if len(envelopeParts) > 0 {
		claimForm["envelope"] = envelopeParts
	}
	fields := map[string]any{
		"id": next.String(), "payout": payout, "title": title, "statement": statement, "validationMode": mode,
		"validatorsPerClaim": panel, "claimForm": claimForm, "awards": awardText,
		"claimDeadline": isoTime(claimAt), "validationDeadline": isoTime(validateAt),
	}
	if len(predicates) > 0 {
		composition := map[string]any{"requiredPass": a.flag("requiredPass", true)}
		if minScoreBps != nil {
			composition["minScoreBps"] = *minScoreBps
		}
		fields["predicates"], fields["composition"] = predicates, composition
	}
	body, err := decoded(fields)
	if err != nil {
		return fail("draft", err)
	}
	criteria, formHash, err := chain.SpecHashes(body)
	if err != nil {
		return fail("draft", err)
	}
	body["criteriaHash"] = criteria
	if err := w.wellFormed("objective.publish", slug, true, body); err != nil {
		return fail("draft", fmt.Errorf("the protocol would refuse this document: %w", err))
	}

	// The hand-over: the page's own form, filled in, carried in the fragment
	// so the server never sees a draft nobody has signed.
	shownAwards := make([]map[string]any, len(awards))
	for i, aw := range awards {
		t := awardText[i].(map[string]any)
		shownAwards[i] = map[string]any{"title": t["title"], "criteria": t["criteria"], "mode": aw.Mode,
			"allocation": whole(fmt.Sprint(aw.Allocation), float64(decimals))}
	}
	handover := map[string]any{"v": 1, "colony": address.String(), "title": title, "statement": statement, "payout": payout,
		"validationMode": mode, "claimForm": map[string]any{"name": formName, "spec": formSpec, "envelope": strings.Join(envelopeParts, "\n")},
		"predicates": predicates, "requiredPass": a.flag("requiredPass", true), "awards": shownAwards,
		"pool": whole(fmt.Sprint(pool), float64(decimals)), "validatorsPerClaim": panel,
		"claimDeadline": isoTime(claimAt), "commitDeadline": isoTime(commitAt), "validationDeadline": isoTime(validateAt),
		"criteriaHash": criteria, "claimFormHash": formHash}
	if minScoreBps != nil {
		handover["minScore"] = float64(*minScoreBps) / 100
	}
	hb, _ := json.Marshal(handover)
	link := w.platform.base + "/objectives/new#draft=" + base64.RawURLEncoding.EncodeToString(hb)

	d := objectiveDraft{Colony: address.String(), Slug: slug, Mint: col.PaymentMint.String(), Decimals: decimals, Body: body,
		Awards: awards, CommitDeadline: commitAt, Pool: pool, Handover: link}
	if err := w.keep("objectives", criteria, d); err != nil {
		return fail("draft", err)
	}
	leads := col.Leader == w.me
	next2 := "hand the person the link: it opens the site's create page filled in, for the colony leader's wallet to sign"
	if leads {
		next2 = "this agent's wallet leads " + slug + ": create it with unloop_create_objective, or hand the person the link to sign on the site"
	}
	return ok("draft", map[string]any{"draft": criteria, "colony": slug, "address": next.String(),
		"criteriaHash": criteria, "claimFormHash": formHash, "document": body,
		"money":            map[string]any{"currency": col.PaymentMint.String(), "pool": whole(fmt.Sprint(pool), float64(decimals)), "awards": shownAwards},
		"commitmentsClose": isoTime(commitAt), "thisWalletLeads": leads, "link": link, "next": next2})
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func (w *worker) createObjective(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	var d objectiveDraft
	if err := w.read("objectives", a.text("draft"), &d); err != nil {
		return fail("create", fmt.Errorf("no draft %q: draft it first with unloop_draft_objective", a.text("draft")))
	}
	colony, _ := solana.ParseKey(d.Colony)
	col, err := w.program.Colony(ctx, colony)
	if err != nil || col == nil {
		return fail("create", fmt.Errorf("the colony: %v", err))
	}
	if col.Leader != w.me {
		return fail("create", fmt.Errorf("only the leader of %s creates objectives in it, and that is %s, not this agent; give the person the link to sign on the site: %s", d.Slug, col.Leader, d.Handover))
	}
	title, _ := d.Body["title"].(string)
	amounts := make([]string, len(d.Awards))
	for i, aw := range d.Awards {
		amounts[i] = whole(fmt.Sprint(aw.Allocation), float64(d.Decimals))
	}
	pool := whole(fmt.Sprint(d.Pool), float64(d.Decimals))
	act := &Action{Kind: "create_objective", Impact: toolapi.ImpactControl, Asks: true,
		Summary: fmt.Sprintf("Create the objective %q in %s, putting in %s of its currency", short(title, 60), d.Slug, pool),
		Detail: map[string]any{"address": d.Body["id"], "colony": d.Slug, "currency": d.Mint, "pool": pool, "awards": amounts,
			"commitmentsClose": isoTime(d.CommitDeadline), "document": d.Body}}
	act.Params, _ = json.Marshal(map[string]string{"draft": a.text("draft")})
	return toolapi.ToolOK("create", w.approvals.propose(act, w.chat), map[string]any{"action": act.ID}), nil
}

// create is an objective from its draft to the bus: created and funded, its
// document sent to the platform its URI names once the index has it, then
// published from the agent's own node.
func (w *worker) create(ctx context.Context, hash string) (string, error) {
	var d objectiveDraft
	if err := w.read("objectives", hash, &d); err != nil {
		return "", err
	}
	colony, _ := solana.ParseKey(d.Colony)
	id, _ := d.Body["id"].(string)
	objective, err := solana.ParseKey(id)
	if err != nil {
		return "", err
	}
	body, err := decoded(d.Body)
	if err != nil {
		return "", err
	}
	path := "/api/objectives/" + id + "/spec"
	uri := w.platform.base + path
	if len(uri) > chain.MaxURIBytes {
		uri = "" // bus-only: the document still travels
	}
	ixs, err := w.chain.Create(ctx, colony, w.me, objective, chain.ObjectiveDraft{Body: body, Awards: d.Awards,
		CommitDeadline: d.CommitDeadline, Pool: d.Pool, URI: uri})
	if err != nil {
		return "", fmt.Errorf("%w (if the colony has moved on, draft it again)", err)
	}
	landed, err := w.chain.Send(ctx, w.key, ixs)
	if err != nil {
		return "", err
	}
	steps := []string{fmt.Sprintf("created at %s in %s (slot %d)", id, landed.Signature, landed.Slot)}
	steps = append(steps, w.sendWhenIndexed(ctx, path, body, "its document"))
	mid, err := w.publish("objective.publish", d.Slug, &envelope.ChainRef{Tx: landed.Signature, Slot: landed.Slot}, body)
	if err != nil {
		return strings.Join(steps, "; "), fmt.Errorf("not published on the bus: %w", err)
	}
	return strings.Join(append(steps, "on the bus as "+mid), "; "), nil
}

// withdraw proposes taking back what an objective's pool is not owed. How
// much that is comes from the platform, which reads it off the account by
// chain/client's arithmetic; the program refuses more, and the action is
// simulated before it is sent.
func (w *worker) withdraw(ctx context.Context, a args) (toolapi.ToolMessage, error) {
	objective, err := solana.ParseKey(a.text("objective"))
	if err != nil {
		return fail("withdraw", err)
	}
	o, err := w.program.Objective(ctx, objective)
	if err == nil && o == nil {
		err = fmt.Errorf("no objective at %s", objective)
	}
	if err != nil {
		return fail("withdraw", err)
	}
	col, err := w.program.Colony(ctx, o.Colony)
	if err != nil || col == nil {
		return fail("withdraw", fmt.Errorf("its colony: %v", err))
	}
	if col.Leader != w.me {
		return fail("withdraw", fmt.Errorf("only the leader of its colony, %s, takes back from an objective's pool", col.Leader))
	}
	var d struct {
		Live *struct {
			Decimals int
			Funds    struct{ Withdrawable string }
		}
		LiveError string
	}
	if err := w.platform.get(ctx, "/api/objectives/"+objective.String(), nil, &d); err != nil {
		return fail("withdraw", err)
	}
	if d.Live == nil {
		return fail("withdraw", fmt.Errorf("the platform could not read the pool: %s", d.LiveError))
	}
	free, _ := new(big.Int).SetString(d.Live.Funds.Withdrawable, 10)
	amount := free
	if s := a.text("amount"); s != "" {
		n, err := baseUnits(s, d.Live.Decimals)
		if err != nil {
			return fail("withdraw", err)
		}
		amount = new(big.Int).SetUint64(n)
	}
	if amount == nil || amount.Sign() <= 0 {
		return fail("withdraw", errors.New("nothing in this pool is free to take back: what it holds is owed to claims"))
	}
	if free != nil && amount.Cmp(free) > 0 {
		return fail("withdraw", fmt.Errorf("only %s is free to take back", whole(free.String(), float64(d.Live.Decimals))))
	}
	shown := whole(amount.String(), float64(d.Live.Decimals))
	return w.propose("withdraw", toolapi.ImpactControl, fmt.Sprintf("Take back %s from objective %s's pool", shown, short(objective.String(), 8)),
		map[string]any{"objective": objective.String(), "amount": shown, "currency": o.PaymentMint.String()}, nil,
		map[string]string{"objective": objective.String(), "amount": amount.String()})
}
