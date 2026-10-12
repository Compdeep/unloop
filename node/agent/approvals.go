package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
)

// Approvals: what the agent would sign, waiting for the person.
//
// The model never signs. A tool that would publish a message or send a
// transaction proposes an action; the agent's own code carries it out, once
// it is approved — by the person on this page, which the model cannot reach,
// or by the autonomy the person chose:
//
//	affect    a message on the bus: approved by itself at `answer` and `act`
//	control   a transaction: approved by itself at `act`, while what it may
//	          spend fits in what is left of the day's limit
//
// except an action that asks: creating an objective, which spends from the
// colony's own currency and commits the person as its leader, is approved by
// the person alone, whatever the autonomy.
//
// The page is served beside the chat, on loopback, and takes a decision only
// from itself: a form posted from any other origin, or to any other host
// name, is refused, so a web page the person happens to have open cannot
// approve anything.

// Action is one thing the agent would do.
type Action struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Impact  int             `json:"impact"`
	Summary string          `json:"summary"`
	Detail  any             `json:"detail"`
	Spend   string          `json:"spend"`           // at most, in the protocol token's smallest unit
	Shown   string          `json:"shown,omitempty"` // the same, in whole tokens
	Params  json.RawMessage `json:"params"`
	State   string          `json:"state"`          // pending, running, done, failed, declined
	Auto    bool            `json:"auto,omitempty"` // approved by the autonomy, not by hand
	Asks    bool            `json:"asks,omitempty"` // the person's to approve, whatever the autonomy
	Result  string          `json:"result,omitempty"`
	Created time.Time       `json:"created"`
	Decided time.Time       `json:"decided,omitempty"`
}

type approvals struct {
	mu       sync.Mutex
	path     string
	actions  []*Action
	autonomy string
	// limit is the day's limit in the smallest unit, read when first needed.
	limit func(context.Context) (*big.Int, error)
	run   func(context.Context, *Action) (string, error)
	ctx   context.Context
	// done is told of each action that finished, for the inbox.
	done func(*Action)
}

const approvalsKept = 200

func newApprovals(ctx context.Context, dir, autonomy string) *approvals {
	a := &approvals{path: filepath.Join(dir, "actions.json"), autonomy: autonomy, ctx: ctx}
	if b, err := os.ReadFile(a.path); err == nil {
		json.Unmarshal(b, &a.actions)
	}
	// What was running when the agent stopped did not finish here; it may have
	// on chain. Say so rather than run it twice.
	for _, act := range a.actions {
		if act.State == "running" {
			act.State, act.Result = "failed", "the agent stopped while this ran; check the chain before trying again"
		}
	}
	return a
}

func (a *approvals) save() {
	if len(a.actions) > approvalsKept {
		a.actions = a.actions[len(a.actions)-approvalsKept:]
	}
	b, _ := json.MarshalIndent(a.actions, "", " ")
	os.WriteFile(a.path, b, 0o600)
}

// covered is whether the autonomy approves an action by itself.
func (a *approvals) covered(act *Action) (bool, string) {
	switch {
	case act.Asks:
		return false, "it is always the person's to approve"
	case act.Impact <= toolapi.ImpactAffect && (a.autonomy == "answer" || a.autonomy == "act"):
		return true, ""
	case act.Impact > toolapi.ImpactAffect && a.autonomy == "act":
		limit, err := a.limit(a.ctx)
		if err != nil {
			return false, "the day's limit could not be read: " + err.Error()
		}
		spend, _ := new(big.Int).SetString(act.Spend, 10)
		if spend == nil {
			spend = new(big.Int)
		}
		if left := new(big.Int).Sub(limit, a.spentToday()); spend.Cmp(left) > 0 {
			return false, "it would spend more than is left of the day's limit"
		}
		return true, ""
	}
	return false, ""
}

// spentToday is what the actions carried out since midnight UTC may have spent.
func (a *approvals) spentToday() *big.Int {
	day := time.Now().UTC().Truncate(24 * time.Hour)
	sum := new(big.Int)
	for _, act := range a.actions {
		if (act.State == "done" || act.State == "running") && act.Decided.After(day) {
			if n, ok := new(big.Int).SetString(act.Spend, 10); ok {
				sum.Add(sum, n)
			}
		}
	}
	return sum
}

// propose records an action, and starts it if the autonomy covers it. It
// returns what the model is told.
func (a *approvals) propose(act *Action, chat string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := make([]byte, 4)
	rand.Read(id)
	act.ID, act.State, act.Created = hex.EncodeToString(id), "pending", time.Now().UTC()
	if act.Spend == "" {
		act.Spend = "0"
	}
	a.actions = append(a.actions, act)
	ok, why := a.covered(act)
	if ok {
		act.Auto = true
		a.start(act)
		a.save()
		return fmt.Sprintf("Started, as the person's autonomy (%s) allows: %s. It is action %s; unloop_actions says how it went.", a.autonomy, act.Summary, act.ID)
	}
	a.save()
	msg := fmt.Sprintf("Waiting for the person to approve it at http://%s/unloop/approvals (action %s): %s.", chat, act.ID, act.Summary)
	if why != "" {
		msg += " Not started by itself because " + why + "."
	}
	return msg + " Tell the person, and do not propose it again."
}

// start carries out an approved action. a.mu is held.
func (a *approvals) start(act *Action) {
	act.State, act.Decided = "running", time.Now().UTC()
	go func() {
		result, err := a.run(a.ctx, act)
		a.mu.Lock()
		defer a.mu.Unlock()
		if err != nil {
			act.State, act.Result = "failed", err.Error()
			if result != "" {
				act.Result = result + " — then " + err.Error()
			}
		} else {
			act.State, act.Result = "done", result
		}
		a.save()
		log.Printf("action %s %s: %s", act.ID, act.State, act.Result)
		if a.done != nil {
			a.done(act)
		}
	}()
}

func (a *approvals) decide(id string, approve bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, act := range a.actions {
		if act.ID != id {
			continue
		}
		if act.State != "pending" {
			return fmt.Errorf("action %s is %s already", id, act.State)
		}
		if approve {
			a.start(act)
		} else {
			act.State, act.Decided = "declined", time.Now().UTC()
		}
		a.save()
		return nil
	}
	return fmt.Errorf("no action %s", id)
}

func (a *approvals) recent(n int) []Action {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Action
	for i := len(a.actions) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, *a.actions[i])
	}
	return out
}

// tool is unloop_actions.
func (a *approvals) tool() toolapi.Tool {
	return tool{"unloop_actions",
		"What this agent has proposed to sign or send, and what became of each: waiting for the person, running, done (with the transaction or message), failed (with why), or declined.",
		`{"type":"object","properties":{"limit":{"type":"integer","description":"default 10"}}}`,
		func(_ context.Context, p args) (toolapi.ToolMessage, error) {
			list := a.recent(min(max(p.number("limit", 10), 1), 50))
			if len(list) == 0 {
				return toolapi.ToolEmpty("actions", "nothing proposed yet"), nil
			}
			out := make([]map[string]any, len(list))
			for i, act := range list {
				out[i] = map[string]any{"id": act.ID, "what": act.Summary, "state": act.State, "result": act.Result, "auto": act.Auto, "spendsAtMost": act.Shown}
			}
			return ok("actions", out)
		}}
}

var page = template.Must(template.New("approvals").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Approvals</title>
<style>
:root{--ink:#16181d;--mute:#5b616b;--line:#e3e5ea;--bg:#f7f8fa;--card:#fff;--ok:#16a35a;--bad:#c2410c;--blue:#1f6df0}
@media (prefers-color-scheme: dark){:root{--ink:#e8eaee;--mute:#a0a6b0;--line:#2b2f37;--bg:#121418;--card:#1a1d23}}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,sans-serif}
main{max-width:760px;margin:0 auto;padding:24px 16px}
h1{font-size:22px;margin:0 0 4px} p.lede{color:var(--mute);margin:0 0 20px}
.a{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px;margin:0 0 12px}
.k{font:12px ui-monospace,monospace;color:var(--mute);text-transform:uppercase;letter-spacing:.04em}
.s{font-weight:600;margin:4px 0}
pre{background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:10px;overflow:auto;font-size:12.5px;max-height:320px;white-space:pre-wrap;overflow-wrap:anywhere}
form{display:inline} button{font:inherit;border-radius:8px;padding:6px 14px;border:1px solid var(--line);background:var(--card);color:var(--ink);cursor:pointer}
button.go{background:var(--blue);border-color:var(--blue);color:#fff}
.done{color:var(--ok)} .failed,.declined{color:var(--bad)} .r{overflow-wrap:anywhere}
</style></head><body><main>
<h1>Approvals</h1>
<p class="lede">What your agent would sign. Nothing here happens until you approve it{{if ne .Autonomy "tell"}}, except what your autonomy, <b>{{.Autonomy}}</b>, allows{{end}}. Spent today without asking: {{.Spent}}.</p>
{{range .Actions}}<div class="a">
<div class="k">{{.Kind}} · {{.Created.Format "Jan 2 15:04"}}{{if .Shown}} · spends at most {{.Shown}}{{end}}{{if .Auto}} · approved by the autonomy{{end}}{{if .Asks}} · only you approve this{{end}}</div>
<div class="s">{{.Summary}}</div>
<pre>{{.DetailText}}</pre>
{{if eq .State "pending"}}
<form method="post" action="/unloop/approvals/{{.ID}}/approve"><button class="go">Approve</button></form>
<form method="post" action="/unloop/approvals/{{.ID}}/decline"><button>Decline</button></form>
{{else}}<div class="{{.State}}">{{.State}}</div>{{if .Result}}<div class="r">{{.Result}}</div>{{end}}{{end}}
</div>{{else}}<p>Nothing yet.</p>{{end}}
</main></body></html>`))

// handler is the approvals page, for the agent's own server.
func (a *approvals) handler(chat string, shown func(*big.Int) string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /unloop/approvals", func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			Action
			DetailText string
		}
		var rows []row
		for _, act := range a.recent(50) {
			b, _ := json.MarshalIndent(act.Detail, "", "  ")
			rows = append(rows, row{act, string(b)})
		}
		a.mu.Lock()
		spent := shown(a.spentToday())
		a.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page.Execute(w, map[string]any{"Actions": rows, "Autonomy": a.autonomy, "Spent": spent})
	})
	mux.HandleFunc("POST /unloop/approvals/{id}/{decision}", func(w http.ResponseWriter, r *http.Request) {
		if !fromItself(r, chat) {
			http.Error(w, "a decision is taken only from this page", http.StatusForbidden)
			return
		}
		d := r.PathValue("decision")
		if d != "approve" && d != "decline" {
			http.NotFound(w, r)
			return
		}
		if err := a.decide(r.PathValue("id"), d == "approve"); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Redirect(w, r, "/unloop/approvals", http.StatusSeeOther)
	})
	return loopbackHost(chat, mux)
}

// fromItself is a request this page made: the browser says it came from the
// same origin, and the Host is the address the chat is served on.
func fromItself(r *http.Request, chat string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	return origin == "http://"+r.Host
}

// loopbackHost refuses a request naming any host but a loopback one, which a
// page elsewhere could only send by pointing a name it controls at 127.0.0.1.
func loopbackHost(chat string, next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(chat)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, p, err := net.SplitHostPort(r.Host)
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if err != nil || p != port || (host != "localhost" && (ip == nil || !ip.IsLoopback())) {
			http.Error(w, "not this machine's agent", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
