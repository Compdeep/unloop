package agent

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/Compdeep/unloop/node/solana"
)

// The site's hand-over to the agent (docs/PLATFORM.md section 7, "The site
// and the agent, together"). The site cannot reach a person's agent and
// should not; it can only link to it. An objective's page links to
// http://127.0.0.1:7777/unloop/work?objective=<address>, and if the person's
// agent is running there, this page answers: the objective, whether its
// colony is followed with a button to follow it, and what to say in the chat
// to start. Opening it does nothing; following takes a press here, which
// only this page can send.

var workPage = template.Must(template.New("work").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Work on this</title>
<style>
:root{--ink:#16181d;--mute:#5b616b;--line:#e3e5ea;--bg:#f7f8fa;--card:#fff;--blue:#1f6df0;--ok:#16a35a;--bad:#c2410c}
@media (prefers-color-scheme: dark){:root{--ink:#e8eaee;--mute:#a0a6b0;--line:#2b2f37;--bg:#121418;--card:#1a1d23}}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,sans-serif}
main{max-width:720px;margin:0 auto;padding:24px 16px}
h1{font-size:22px;margin:0 0 4px} .lede{color:var(--mute);margin:0 0 20px}
.c{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px;margin:0 0 12px}
.k{font:12px ui-monospace,monospace;color:var(--mute);text-transform:uppercase;letter-spacing:.04em}
.t{font-weight:600;font-size:17px;margin:4px 0} .m{color:var(--mute);font-size:14px;overflow-wrap:anywhere}
.say{display:flex;gap:10px;align-items:flex-start;margin:8px 0} .say q{flex:1;font-style:normal;quotes:none;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px 10px;overflow-wrap:anywhere}
button,a.b{font:inherit;border-radius:8px;padding:6px 14px;border:1px solid var(--line);background:var(--card);color:var(--ink);cursor:pointer;text-decoration:none;display:inline-block}
.go{background:var(--blue)!important;border-color:var(--blue)!important;color:#fff!important}
.ok{color:var(--ok)} .bad{color:var(--bad)} form{display:inline}
</style></head><body><main>
<h1>Work on this</h1>
<p class="lede">Your agent, on this machine. Nothing happens until you say so in the chat.</p>
<div class="c">
<div class="k">Objective{{if .Colony}} · {{.Colony}}{{end}}{{if .Status}} · {{.Status}}{{end}}</div>
{{if .Title}}<div class="t">{{.Title}}</div>{{else}}<div class="t bad">Not read from {{.Platform}}{{if .Problem}}: {{.Problem}}{{end}}</div>{{end}}
<div class="m">{{.Address}}{{if .Closes}} · claims close {{.Closes}}{{end}}{{if .Unverified}} · <span class="bad">its terms are not verified against the chain</span>{{end}}</div>
<div class="m"><a href="{{.Link}}">Read it on {{.Platform}}</a></div>
</div>
{{if .Colony}}<div class="c">
{{if .Following}}<span class="ok">Following {{.Colony}}</span>: your agent hears its new objectives, claims and threads.
{{else}}Not following <b>{{.Colony}}</b> yet. Following it, your agent hears its new objectives, claims and threads as they happen.
<form method="post" action="/unloop/work/follow"><input type="hidden" name="colony" value="{{.Colony}}"><input type="hidden" name="objective" value="{{.Address}}"><button class="go">Follow {{.Colony}}</button></form>{{end}}
</div>{{end}}
<div class="c">
<div class="k">Say to your agent</div>
{{range .Prompts}}<div class="say"><q>{{.}}</q><button type="button" data-say="{{.}}">Copy</button></div>{{end}}
<p><a class="b go" href="/">Open the chat</a></p>
</div>
<script>
for (const b of document.querySelectorAll("button[data-say]")) b.onclick = () =>
  navigator.clipboard.writeText(b.dataset.say).then(() => { b.textContent = "Copied"; setTimeout(() => (b.textContent = "Copy"), 1500); });
</script>
</main></body></html>`))

// workHandler serves the page and the follow it offers.
func workHandler(site *platform, follow func(string, bool) error, followed func() []string, chat string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /unloop/work", func(w http.ResponseWriter, r *http.Request) {
		address := r.URL.Query().Get("objective")
		if _, err := solana.ParseKey(address); err != nil {
			http.Error(w, "name an objective: /unloop/work?objective=<its address>", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		v := map[string]any{"Address": address, "Platform": site.base, "Link": site.base + "/objectives/" + address,
			"Prompts": []string{
				"Read objective " + address + " and tell me what a claim on it has to show, and how it is judged.",
				"What has already been claimed on " + address + ", and what is still open?",
				"Draft a claim on " + address + " from my results in ./results, and show it to me before filing.",
			}}
		var d struct {
			Objective struct {
				Status        string `json:"status"`
				ClaimDeadline any    `json:"claimDeadline"`
			} `json:"objective"`
			Colony struct {
				Slug string `json:"slug"`
			} `json:"colony"`
			Spec *struct {
				Verified bool           `json:"verified"`
				Body     map[string]any `json:"body"`
			} `json:"spec"`
		}
		if err := site.get(ctx, "/api/objectives/"+address, url.Values{}, &d); err != nil {
			v["Problem"] = err.Error()
		} else {
			v["Colony"], v["Status"], v["Closes"] = d.Colony.Slug, d.Objective.Status, at(d.Objective.ClaimDeadline)
			v["Following"] = slices.Contains(followed(), d.Colony.Slug)
			v["Title"] = "(its terms could not be read)"
			if d.Spec != nil {
				if t, _ := d.Spec.Body["title"].(string); t != "" {
					v["Title"] = t
				}
				v["Unverified"] = !d.Spec.Verified
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		workPage.Execute(w, v)
	})
	mux.HandleFunc("POST /unloop/work/follow", func(w http.ResponseWriter, r *http.Request) {
		if !fromItself(r, chat) {
			http.Error(w, "following is chosen only from this page", http.StatusForbidden)
			return
		}
		colony, objective := r.FormValue("colony"), r.FormValue("objective")
		if err := followColony(r.Context(), site, follow, colony, true); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/unloop/work?objective="+url.QueryEscape(objective), http.StatusSeeOther)
	})
	return loopbackHost(chat, mux)
}
