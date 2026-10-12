package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Compdeep/kaiju/agent/toolapi"
)

// What settling proposes, read from the claim: only what is due, the
// authority's figures only where this agent is the authority, and nothing
// before the window has closed for anybody else.
func TestSettlingProposesWhatIsDue(t *testing.T) {
	w, _, at := testWorker(t, "tell", "0", `{}`)
	settle := func(name string, p map[string]any) toolapi.ToolMessage {
		if p == nil {
			p = map[string]any{}
		}
		p["claim"] = at[name].String()
		return use(t, w.tools(), "unloop_settle", p)
	}

	if m := settle("pending in its window", nil); m.Status != toolapi.StatusError || !strings.Contains(m.Detail, "not yet") {
		t.Errorf("resolving before the window closed: %s", m.Detail)
	}
	if m := settle("pending, this agent its authority", map[string]any{"rootsConfirmed": []any{2.0}}); m.Status != toolapi.StatusError {
		t.Errorf("confirmed a root the claim never declared: %s", m.Content)
	}

	settle("pending past its window", map[string]any{"admit": true, "noveltyBps": 9000.0})
	settle("pending, this agent its authority", map[string]any{"admit": true, "noveltyBps": 7000.0, "scoreBps": 8150.0, "rootsConfirmed": []any{1.0}})
	settle("paid, an edge unpaid", nil)
	list := w.approvals.recent(5)
	if len(list) != 3 {
		t.Fatalf("%d proposed: %+v", len(list), list)
	}
	byClaim := map[string]Action{}
	for _, a := range list {
		var p settleParams
		json.Unmarshal(a.Params, &p)
		byClaim[p.Claim] = a
	}

	past := byClaim[at["pending past its window"].String()]
	if !strings.Contains(past.Summary, "as its validators decided") || !strings.Contains(past.Summary, "award") || strings.Contains(past.Summary, "edge") {
		t.Errorf("past its window: %s", past.Summary)
	}
	if p := params(past); p.Settlement != nil {
		t.Errorf("figures sent by somebody who is not the authority: %+v", p.Settlement)
	}

	mine := byClaim[at["pending, this agent its authority"].String()]
	if p := params(mine); p.Settlement == nil || !p.Settlement.Admit || p.Settlement.NoveltyBps != 7000 || p.Settlement.ScoreBps != 8150 || p.Settlement.RootsConfirmed != 2 {
		t.Errorf("the authority's settlement: %+v", p.Settlement)
	}
	if !strings.Contains(mine.Summary, "settlement authority") || !strings.Contains(mine.Summary, "root 1") || strings.Contains(mine.Summary, "root 0") {
		t.Errorf("the authority's: %s", mine.Summary)
	}

	paid := byClaim[at["paid, an edge unpaid"].String()]
	if !strings.Contains(paid.Summary, "root 1") || strings.Contains(paid.Summary, "root 0") || strings.Contains(paid.Summary, "resolve") || strings.Contains(paid.Summary, "award") {
		t.Errorf("paid, with its second edge unpaid: %s", paid.Summary)
	}
	for _, a := range list {
		if a.Spend != "0" || a.Shown != "" || a.Impact != toolapi.ImpactControl {
			t.Errorf("%s: spends %q at impact %d", a.Summary, a.Spend, a.Impact)
		}
	}
}

func params(a Action) (p settleParams) {
	json.Unmarshal(a.Params, &p)
	return
}
