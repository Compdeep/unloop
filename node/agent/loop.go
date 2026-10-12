package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	kaiju "github.com/Compdeep/kaiju/agent"
	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/envelope"
)

// What keeps the agent working when nobody is chatting (docs/PLATFORM.md
// section 7, "Events"): two things, both its own, since kaiju has no
// scheduler.
//
//	the bus     an objection to a claim this agent filed starts a run of its
//	            own, with the objection as its cause
//	a ticker    every few minutes, its duties as a validator: what it was
//	            drawn for, and what it has sealed and must open
//
// A run started this way may go as far as the autonomy: at `tell` it only
// reads, and says what it found; at `answer` it may answer the thread; at
// `act` it may spend. Whatever it would sign still goes through the
// approvals, which carry it out by themselves only where the autonomy
// covers it. What a run concluded, and what the ticker noticed, is a note
// in the inbox, which unloop_heard reads.
type loop struct {
	ag       *kaiju.Agent
	worker   *worker
	inbox    *inbox
	platform *platform
	autonomy string

	mu      sync.Mutex
	noticed map[string]bool
	// begin starts a run on a cause: start, or a test's stand-in.
	begin func(id string, cause any)
}

// ceiling is the intent a run of its own starts with.
func (l *loop) ceiling() int {
	switch l.autonomy {
	case "act":
		return toolapi.ImpactControl
	case "answer":
		return toolapi.ImpactAffect
	}
	return toolapi.ImpactObserve
}

// objection is what started a run: an objection on a claim this agent filed.
type objection struct {
	claim, colony, from string
	body                map[string]any
}

// deliver is bus.Config.Deliver: the inbox, then whatever wants the agent.
func (l *loop) deliver(t bus.Topic, m *envelope.Message) {
	l.inbox.deliver(t, m)
	if m.Type != "cannot_verify" || m.Body == nil {
		return
	}
	hash, _ := m.Body["claimHash"].(string)
	for _, f := range l.worker.mine() {
		if f.Hash == hash && f.Colony == m.Colony && l.first("objection "+m.ID()) {
			l.begin(m.ID(), &objection{claim: f.Claim, colony: m.Colony, from: m.Author, body: m.Body})
		}
	}
}

// first is whether key has not been noticed before, and notes it.
func (l *loop) first(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.noticed[key] {
		return false
	}
	l.noticed[key] = true
	return true
}

// describe is kaiju's DescribeTrigger: the cause in words for the planner.
func (l *loop) describe(t kaiju.Trigger) string {
	o, ok := t.Cause.(*objection)
	if !ok {
		return ""
	}
	detail, _ := json.MarshalIndent(o.body, "", "  ")
	var next string
	switch l.autonomy {
	case "tell":
		next = "Read the claim and its thread with unloop_claim, and say plainly what the validator could not do and what the person might answer or add. Change nothing."
	default:
		next = "Read the claim and its thread with unloop_claim. If the claim already holds what the validator missed, answer with unloop_answer, saying where; if evidence is missing and the person's results hold it, propose it with unloop_amend; otherwise say what the person needs to supply. Never assert anything the claim's evidence does not show."
	}
	return fmt.Sprintf("A validator (%s) filed an objection on claim %s, which this agent filed, in colony %s:\n%s\n\n%s",
		o.from, o.claim, o.colony, detail, next)
}

// start runs the agent on a cause, and keeps what it concluded as a note.
func (l *loop) start(id string, cause any) {
	ceiling := l.ceiling()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		res, err := l.ag.SubmitSync(ctx, kaiju.Trigger{Type: "event", ID: id, Source: "bus", MaxIntent: &ceiling, Cause: cause,
			Data: json.RawMessage(`{}`)})
		text := ""
		switch {
		case err != nil:
			text = "The agent could not work on it: " + err.Error()
		case res != nil:
			text = res.Outcome
		}
		l.inbox.note(id, "objection", text)
	}()
}

// tick is the ticker's round: every few minutes until ctx ends.
func (l *loop) tick(ctx context.Context) {
	for {
		l.duties(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// duties notes each new duty once, and at `act` opens a sealed finding as
// soon as its window is open: a commitment never opened costs the bond.
func (l *loop) duties(ctx context.Context) {
	var d struct {
		Sealed []struct {
			Claim, Window      string
			ValidationDeadline any `json:"validationDeadline"`
		} `json:"sealed"`
		Awaiting []struct {
			Claim          string
			CommitDeadline any `json:"commitDeadline"`
		} `json:"awaiting"`
	}
	if err := l.platform.get(ctx, "/api/claims/duties", url.Values{"validator": {l.worker.me.String()}}, &d); err != nil {
		log.Printf("duties: %v", err)
		return
	}
	for _, a := range d.Awaiting {
		if l.first("drawn " + a.Claim) {
			l.inbox.note("drawn-"+a.Claim, "duty", fmt.Sprintf("Drawn to validate claim %s; commitments close %s. Read it with unloop_claim, check it, and commit with unloop_commit.", a.Claim, at(a.CommitDeadline)))
		}
	}
	for _, s := range d.Sealed {
		if s.Window != "open" || !l.first("open "+s.Claim) {
			continue
		}
		var held sealedFinding
		if l.worker.read("sealed", s.Claim, &held) != nil {
			l.inbox.note("open-"+s.Claim, "duty", fmt.Sprintf("The commitment on claim %s can be opened until %s, and this agent does not hold the finding that opens it.", s.Claim, at(s.ValidationDeadline)))
			continue
		}
		if l.autonomy == "act" {
			res, _ := l.worker.reveal(ctx, args{"claim": s.Claim})
			l.inbox.note("open-"+s.Claim, "duty", "Opening the finding on claim "+s.Claim+": "+res.Content)
		} else {
			l.inbox.note("open-"+s.Claim, "duty", fmt.Sprintf("The finding on claim %s can be opened now, until %s: unloop_reveal.", s.Claim, at(s.ValidationDeadline)))
		}
	}
}

// done is told of each action that finished, and keeps it as a note.
func (l *loop) done(act *Action) {
	l.inbox.note("action-"+act.ID, "action", strings.TrimSpace(fmt.Sprintf("%s: %s. %s", act.Summary, act.State, act.Result)))
}
