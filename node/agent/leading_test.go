package agent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Compdeep/unloop/node/chain"
	"github.com/Compdeep/unloop/node/envelope"
)

// An objective as a person might ask for one, with every deadline fixed so
// its hand-over is the same on every run.
func objectiveAsked(colony string) map[string]any {
	return map[string]any{
		"colony": colony, "title": "  Thermostable binders ", "statement": "Find a stable binder against TR-9 — any Tm above 75 °C.",
		"payout": "prize", "validationMode": "semantic",
		"claimForm": map[string]any{"name": "fasta-normalised", "spec": "As colony default.", "envelope": []any{"sequence", " melting curve ", ""}},
		"predicates": []any{
			map[string]any{"proposition": "Tm above 75C", "kind": "required"},
			map[string]any{"id": "P2", "proposition": "Expresses in E. coli", "kind": "scored", "weight": 3.0},
		},
		"minScore": 50.25,
		"awards": []any{
			map[string]any{"title": "First binder", "criteria": "The first that holds.", "mode": "winnerTakeAll", "amount": "400"},
			map[string]any{"title": "Runner-up", "criteria": "Graded by score.", "mode": "graded", "amount": "99.5"},
		},
		"pool":          "500.25",
		"claimDeadline": "2099-01-01T00:00:00Z", "commitDeadline": "2099-01-08T00:00:00Z", "validationDeadline": "2099-01-15T00:00:00Z",
	}
}

func handoverOf(t *testing.T, link string) map[string]any {
	t.Helper()
	_, frag, found := strings.Cut(link, "#draft=")
	if !found {
		t.Fatalf("no hand-over in %s", link)
	}
	b, err := base64.RawURLEncoding.DecodeString(frag)
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	json.Unmarshal(b, &h)
	return h
}

func TestDraftingAnObjective(t *testing.T) {
	w, _, keys := testWorker(t, "act", "1000", `{}`)
	tools := w.all()
	m := use(t, tools, "unloop_draft_objective", objectiveAsked("protein-design"))
	if m.Status != "ok" {
		t.Fatalf("drafted: %s %s", m.Content, m.Detail)
	}
	var d map[string]any
	json.Unmarshal([]byte(m.Content), &d)
	doc := d["document"].(map[string]any)

	// The colony's next address, its fourth objective; the hashes the document's own.
	colonyKey := keys["colony"]
	next := w.program.Address([]byte("objective"), colonyKey[:], []byte{4, 0, 0, 0, 0, 0, 0, 0})
	if d["address"] != next.String() || doc["id"] != next.String() {
		t.Errorf("named %v, the colony's next is %s", d["address"], next)
	}
	body, _ := decoded(doc)
	criteria, form, _ := chain.SpecHashes(body)
	if d["criteriaHash"] != criteria || d["claimFormHash"] != form || doc["criteriaHash"] != criteria {
		t.Errorf("hashes %v %v, the document's %s %s", d["criteriaHash"], d["claimFormHash"], criteria, form)
	}
	// As the page writes it: trimmed, the envelope's empty line gone, a weight only where scored, the score in basis points.
	if doc["title"] != "Thermostable binders" || len(doc["claimForm"].(map[string]any)["envelope"].([]any)) != 2 {
		t.Errorf("document: %v", doc)
	}
	preds := doc["predicates"].([]any)
	if _, has := preds[0].(map[string]any)["weight"]; has || preds[0].(map[string]any)["id"] != "P1" {
		t.Errorf("predicates: %v", preds)
	}
	if doc["composition"].(map[string]any)["minScoreBps"] != float64(5025) {
		t.Errorf("composition: %v", doc["composition"])
	}
	// Five validators by default in a colony needing two votes.
	if doc["validatorsPerClaim"] != float64(5) || doc["claimDeadline"] != "2099-01-01T00:00:00.000Z" {
		t.Errorf("panel and deadline: %v %v", doc["validatorsPerClaim"], doc["claimDeadline"])
	}
	// Somebody else leads it: the hand-over is the way, and creating from here is refused.
	if d["thisWalletLeads"] != false || !strings.HasPrefix(d["link"].(string), w.platform.base+"/objectives/new#draft=") {
		t.Errorf("leads %v, link %v", d["thisWalletLeads"], d["link"])
	}
	if c := use(t, tools, "unloop_create_objective", map[string]any{"draft": criteria}); c.Status == "ok" || !strings.Contains(c.Detail, "only the leader") {
		t.Errorf("created in a colony it does not lead: %s %s", c.Content, c.Detail)
	}

	// The hand-over carries the form, and its hashes. The site's test holds
	// the page to them (platform/tests/handover.test.ts).
	h := handoverOf(t, d["link"].(string))
	if h["criteriaHash"] != criteria || h["pool"] != "500.25" || h["colony"] != keys["colony"].String() {
		t.Errorf("hand-over: %v", h)
	}
	b, _ := json.MarshalIndent(map[string]any{
		"comment": []string{
			"An objective's hand-over from the agent to the site's create page (node/agent/leading.go), written by",
			"TestDraftingAnObjective with VECTORS_WRITE=1. platform/tests/handover.test.ts fills the page's form from it",
			"and must arrive at the same two hashes.",
		},
		"handover": h,
	}, "", "  ")
	const file = "../../protocol/vectors/handover.json"
	if os.Getenv("VECTORS_WRITE") == "1" {
		os.WriteFile(file, append(b, '\n'), 0o644)
	}
	if was, err := os.ReadFile(file); err != nil || string(was) != string(b)+"\n" {
		t.Errorf("protocol/vectors/handover.json differs from this hand-over (VECTORS_WRITE=1 rewrites it): %v", err)
	}
}

func TestAnObjectiveThatCannotBeIsNotDrafted(t *testing.T) {
	w, _, _ := testWorker(t, "tell", "0", `{}`)
	for name, edit := range map[string]func(map[string]any){
		"no colony":                 func(a map[string]any) { a["colony"] = "nowhere" },
		"prizes past the pool":      func(a map[string]any) { a["pool"] = "450" },
		"too fine for the currency": func(a map[string]any) { a["pool"] = "500.0000001" },
		"a panel under the floor":   func(a map[string]any) { a["validatorsPerClaim"] = 1.0 },
		"deadlines out of order":    func(a map[string]any) { a["commitDeadline"] = "2098-01-01T00:00:00Z" },
		"a deadline passed":         func(a map[string]any) { a["claimDeadline"] = "2001-01-01T00:00:00Z" },
		"two predicates, one id": func(a map[string]any) {
			a["predicates"] = []any{map[string]any{"id": "P1", "proposition": "a", "kind": "required"}, map[string]any{"id": "P1", "proposition": "b", "kind": "required"}}
		},
		"a rate of nothing": func(a map[string]any) {
			a["payout"] = "rate"
			a["awards"] = []any{map[string]any{"title": "t", "criteria": "c", "mode": "graded", "amount": "0"}}
		},
		"four awards": func(a map[string]any) {
			x := map[string]any{"title": "t", "criteria": "c", "mode": "graded", "amount": "1"}
			a["awards"] = []any{x, x, x, x}
		},
		"no claim form": func(a map[string]any) { a["claimForm"] = map[string]any{"name": "x"} },
		"no title":      func(a map[string]any) { a["title"] = "  " },
		"a fractional weight": func(a map[string]any) {
			a["predicates"] = []any{map[string]any{"proposition": "a", "kind": "scored", "weight": 2.5}}
		},
	} {
		a := objectiveAsked("protein-design")
		edit(a)
		if m := use(t, w.all(), "unloop_draft_objective", a); m.Status == "ok" {
			t.Errorf("%s: drafted", name)
		}
	}
	// A public colony draws three validators for each vote: fifteen at least.
	a := objectiveAsked("agents-own")
	a["validatorsPerClaim"] = 14.0
	if m := use(t, w.all(), "unloop_draft_objective", a); m.Status == "ok" || !strings.Contains(m.Detail, "at least 15") {
		t.Errorf("a public colony's floor: %s %s", m.Content, m.Detail)
	}
}

func TestCreatingAnObjectiveIsAlwaysThePersons(t *testing.T) {
	// At act, with room in the day's limit: still waits.
	w, r, keys := testWorker(t, "act", "1000000", `{}`)
	m := use(t, w.all(), "unloop_draft_objective", objectiveAsked("agents-own"))
	var d map[string]any
	json.Unmarshal([]byte(m.Content), &d)
	if d["thisWalletLeads"] != true {
		t.Fatalf("the agent leads agents-own: %s %s", m.Content, m.Detail)
	}
	c := use(t, w.all(), "unloop_create_objective", map[string]any{"draft": d["draft"]})
	if c.Status != "ok" || !strings.Contains(c.Content, "Waiting for the person") || !strings.Contains(c.Content, "always the person's") {
		t.Fatalf("proposed: %s", c.Content)
	}
	act := w.approvals.recent(1)[0]
	if !act.Asks || act.Kind != "create_objective" || act.State != "pending" {
		t.Fatalf("action: %+v", act)
	}
	if err := w.approvals.decide(act.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(r.kinds()) == 1 && r.kinds()[0] == "create_objective" })

	// What it would sign is the draft, at the colony's next address, from the agent's wallet.
	var kept objectiveDraft
	if err := w.read("objectives", d["draft"].(string), &kept); err != nil {
		t.Fatal(err)
	}
	body, _ := decoded(kept.Body)
	ownKey := keys["own"]
	next := w.program.Address([]byte("objective"), ownKey[:], make([]byte, 8))
	ixs, err := w.chain.Create(t.Context(), keys["own"], w.me, next, chain.ObjectiveDraft{Body: body, Awards: kept.Awards,
		CommitDeadline: kept.CommitDeadline, Pool: kept.Pool})
	if err != nil {
		t.Fatalf("the kept draft does not build: %v", err)
	}
	if !ixs[0].Accounts[0].Signer || ixs[0].Accounts[0].Key != w.me || kept.Pool != 500_250_000 || kept.Awards[1].Allocation != 99_500_000 {
		t.Errorf("built %+v from %+v", ixs[0].Accounts[0], kept)
	}
	// The document it would publish is one the protocol takes.
	if err := w.wellFormed("objective.publish", "agents-own", true, body); err != nil {
		t.Error(err)
	}
	if _, err := envelope.New("objective.publish", "agents-own", w.me.String(), w.node.ID().String(), &envelope.ChainRef{Tx: strings.Repeat("1", 88)}, body); err != nil {
		t.Error(err)
	}
}

// A leader takes back what its objective's pool is not owed: all of it by
// default, never more, and only from an objective in a colony it leads.
func TestWithdrawingFromAPool(t *testing.T) {
	w, _, keys := testWorker(t, "tell", "0", `{}`)
	if m := use(t, w.all(), "unloop_withdraw", map[string]any{"objective": keys["open"].String()}); m.Status != "error" || !strings.Contains(m.Detail, "only the leader") {
		t.Fatalf("from a colony another leads: %s %s", m.Status, m.Detail)
	}
	led := keys["in a colony this agent leads"].String()
	if m := use(t, w.all(), "unloop_withdraw", map[string]any{"objective": led, "amount": "300"}); m.Status != "error" || !strings.Contains(m.Detail, "only 250 is free") {
		t.Fatalf("more than is free: %s %s", m.Status, m.Detail)
	}
	for amount, want := range map[string]string{"": "250000000", "100": "100000000"} {
		a := map[string]any{"objective": led}
		if amount != "" {
			a["amount"] = amount
		}
		if m := use(t, w.all(), "unloop_withdraw", a); m.Status != "ok" {
			t.Fatalf("%q: %s", amount, m.Detail)
		}
		var p map[string]string
		act := w.approvals.recent(1)[0]
		json.Unmarshal(act.Params, &p)
		if act.Kind != "withdraw" || act.State != "pending" || p["amount"] != want || p["objective"] != led {
			t.Errorf("%q: %+v %v", amount, act, p)
		}
	}
	// What it would sign: withdraw_unencumbered, to the agent's own account.
	ixs, err := w.chain.Withdraw(t.Context(), keys["in a colony this agent leads"], w.me, 250_000_000)
	if err != nil || len(ixs) != 2 || !ixs[1].Accounts[0].Signer || ixs[1].Accounts[0].Key != w.me {
		t.Fatalf("built %+v, %v", ixs, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("not in time")
}
