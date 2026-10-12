package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/chain"
	"github.com/Compdeep/unloop/node/envelope"
	"github.com/Compdeep/unloop/node/solana"
	"github.com/libp2p/go-libp2p/core/crypto"
)

var program = must(solana.ParseKey("3bPsoRTKT1J9U6V6Zua42VKHVBZ9XAuBPJq3RFFF3Qdz"))

func must(k solana.Key, err error) solana.Key {
	if err != nil {
		panic(err)
	}
	return k
}

func disc(kind string) []byte { h := sha256.Sum256([]byte("account:" + kind)); return h[:8] }

// A chain with one colony charging 2 tokens a validator, private, two votes
// to decide, led by somebody else; and two objectives in it with a panel of
// three: one open, one closed. Beside it, "agents-own", public, led by the
// agent itself. Both pay in a currency of six decimals.
func fakeChain(t *testing.T, me solana.Key) (*solana.RPC, map[string]solana.Key) {
	colony := must(solana.FindAddress(program, []byte("colony"), []byte("protein-design")))
	own := must(solana.FindAddress(program, []byte("colony"), []byte("agents-own")))
	open := must(solana.ParseKey("HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu"))
	closed := must(solana.ParseKey("4rPuCNpWz6h2NVjzhvuMk9gAYA7hWQ9jnSNCfA6vQLvP"))
	unverified := must(solana.ParseKey("6FvgwEyhHyjhh6bvrnDWuo1VUHDyXSMXmCPqFT5vzbaj"))
	mint := must(solana.ParseKey("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"))
	token := must(solana.ParseKey("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"))
	objective := func(deadline int64, authority ...solana.Key) []byte {
		b := append(disc("Objective"), colony[:]...)
		b = append(b, append(authority, solana.Key{})[0][:]...)
		b = append(b, make([]byte, 32)...)
		b = append(b, make([]byte, 459-len(b))...)
		b = append(b, 3)
		for _, d := range []int64{deadline, deadline + 100, deadline + 200} {
			b = binary.LittleEndian.AppendUint64(b, uint64(d))
		}
		return append(b, make([]byte, 64)...)
	}
	colonyAt := func(leader solana.Key, slug string, minVotes uint16, admission byte, fee, count uint64) []byte {
		c := append(disc("Colony"), leader[:]...)
		c = binary.LittleEndian.AppendUint32(c, uint32(len(slug)))
		c = append(c, slug...)
		c = append(c, mint[:]...)
		c = binary.LittleEndian.AppendUint16(c, minVotes)
		c = append(c, make([]byte, 4)...)
		c = append(c, admission)
		c = append(c, make([]byte, 92-32-6-1)...)
		c = binary.LittleEndian.AppendUint64(c, fee)
		c = binary.LittleEndian.AppendUint64(c, count)
		return append(c, 1)
	}
	// Claims to settle (settle.go): a status, its roots with the confirmed and
	// paid bits, and the two Options at Claim.decided_by.
	claimAt := func(objective solana.Key, status byte, roots []solana.Key, confirmed, paid byte, awarded bool) []byte {
		c := append(disc("Claim"), objective[:]...)
		c = append(c, make([]byte, 32+32+1+8)...)
		c = append(c, status)
		slots := make([]byte, 8*32)
		for i, r := range roots {
			copy(slots[32*i:], r[:])
		}
		c = append(append(c, slots...), byte(len(roots)), confirmed, paid)
		c = append(c, make([]byte, 422-len(c))...)
		if status == 0 {
			return append(c, 0, 0)
		}
		if awarded {
			return append(append(c, 1, 0, 1), make([]byte, 8)...)
		}
		return append(c, 1, 0, 0)
	}
	decided, mine, led := addrN(40), addrN(41), addrN(42)
	ledObjective := objective(time.Now().Unix() + 86400)
	copy(ledObjective[8:40], own[:])
	copy(ledObjective[72:104], mint[:])
	m := make([]byte, 82)
	m[44] = 6
	accounts := map[string][]byte{colony.String(): colonyAt(addrN(9), "protein-design", 2, 2, 2_000_000_000, 4),
		own.String(): colonyAt(me, "agents-own", 5, 0, 0, 0), mint.String(): m,
		open.String(): objective(time.Now().Unix() + 86400), closed.String(): objective(time.Now().Unix() - 10),
		unverified.String(): objective(time.Now().Unix() + 86400),
		decided.String():    objective(time.Now().Unix()-1000, addrN(9)), mine.String(): objective(time.Now().Unix()+86400, me),
		led.String():       ledObjective,
		addrN(50).String(): claimAt(open, 0, nil, 0, 0, false),
		addrN(51).String(): claimAt(decided, 0, nil, 0, 0, false),
		addrN(52).String(): claimAt(mine, 0, []solana.Key{addrN(50), addrN(51)}, 0, 0, false),
		addrN(53).String(): claimAt(decided, 1, []solana.Key{addrN(50), addrN(51)}, 3, 1, true)}
	owner := func(addr string) string {
		if addr == mint.String() {
			return token.String()
		}
		return program.String()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var addr string
		json.Unmarshal(req.Params[0], &addr)
		var value any
		if data, ok := accounts[addr]; ok {
			value = map[string]any{"owner": owner(addr), "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"value": value}})
	}))
	t.Cleanup(srv.Close)
	return solana.NewRPC(srv.URL), map[string]solana.Key{"open": open, "closed": closed, "unverified": unverified, "colony": colony, "own": own,
		"pending in its window": addrN(50), "pending past its window": addrN(51), "pending, this agent its authority": addrN(52),
		"paid, an edge unpaid": addrN(53), "in a colony this agent leads": led}
}

// The platform: the open objective's terms verified, the closed one's not;
// one claim filed by the agent, with a thread; and the agent's duties.
func fakePlatform(t *testing.T, me solana.Key, claimHash string, duties string) *platform {
	mux := http.NewServeMux()
	spec := func(verified bool) string {
		return fmt.Sprintf(`{"colony":{"slug":"protein-design"},"spec":{"verified":%v,"body":{"title":"Thermostable binders","validationMode":"semantic"}}}`, verified)
	}
	mux.HandleFunc("/api/objectives/HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(spec(true))) })
	mux.HandleFunc("/api/objectives/4rPuCNpWz6h2NVjzhvuMk9gAYA7hWQ9jnSNCfA6vQLvP", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(spec(true))) })
	mux.HandleFunc("/api/objectives/6FvgwEyhHyjhh6bvrnDWuo1VUHDyXSMXmCPqFT5vzbaj", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(spec(false))) })
	claim := func(agent string) string {
		return fmt.Sprintf(`{"claim":{"address":"Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer","agent":%q,"hash":%q,"status":"pending"},
"colony":{"slug":"protein-design"},"validators":[],"threadState":{"next":{"seq":3,"answering":2,"submission":2},"open":[1,3],"clarificationsLeft":4}}`, agent, claimHash)
	}
	mux.HandleFunc("/api/claims/Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(claim(me.String()))) })
	mux.HandleFunc("/api/claims/2sz8kf41u9FvmPkzbes2BzFeoW6h5jfJcBAAq2jY57sc", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Replace(claim("SomebodyElse1111111111111111111111111111111"), "Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer", "2sz8kf41u9FvmPkzbes2BzFeoW6h5jfJcBAAq2jY57sc", 1)))
	})
	mux.HandleFunc("/api/objectives/"+addrN(42).String(), func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"live":{"decimals":6,"funds":{"balance":"400000000","encumbered":"150000000","withdrawable":"250000000"}}}`))
	})
	mux.HandleFunc("/api/claims/duties", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(duties)) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return newPlatform(srv.URL)
}

// addrN is an address nobody derived.
func addrN(n byte) solana.Key {
	var k solana.Key
	k[0], k[31] = 2, n
	return k
}

type ran struct {
	mu   sync.Mutex
	acts []string
}

func (r *ran) run(_ context.Context, a *Action) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acts = append(r.acts, a.Kind)
	return "done in a test", nil
}

func (r *ran) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.acts...)
}

func testWorker(t *testing.T, autonomy, limit, duties string) (*worker, *ran, map[string]solana.Key) {
	dir := t.TempDir()
	key, _ := NewWallet(dir, "")
	me := Address(key)
	rpc, objectives := fakeChain(t, me)
	pk, _, _ := crypto.GenerateEd25519Key(nil)
	node, err := bus.New(bus.Config{Key: pk, Listen: []string{"/ip4/127.0.0.1/tcp/0"}, Cluster: "x",
		Accounts: accountsFunc(func(context.Context, *envelope.Message) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Close() })
	r := &ran{}
	acts := newApprovals(context.Background(), dir, autonomy)
	p := solana.NewProgram(rpc, program)
	w := &worker{dir: dir, key: key, me: me, program: p, chain: chain.NewClient(rpc, p), node: node,
		platform: fakePlatform(t, me, strings.Repeat("ab", 32), duties), approvals: acts, chat: "127.0.0.1:7777", decimals: 9}
	acts.limit, acts.run = w.limit(limit), r.run
	return w, r, objectives
}

type accountsFunc func(context.Context, *envelope.Message) error

func (f accountsFunc) Authorize(ctx context.Context, m *envelope.Message) error { return f(ctx, m) }

func use(t *testing.T, tools []toolapi.Tool, name string, p map[string]any) toolapi.ToolMessage {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			m, err := tl.(interface {
				ExecuteTyped(context.Context, map[string]any) (toolapi.ToolMessage, error)
			}).ExecuteTyped(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no tool %s", name)
	return toolapi.ToolMessage{}
}

var evidence = []any{map[string]any{"kind": "procedural", "statement": "Tm 81.1C across five seeds", "provenance": "runs 1-5"}}

func TestDraftingAClaim(t *testing.T) {
	w, _, objectives := testWorker(t, "tell", "0", `{}`)
	tools := w.tools()
	m := use(t, tools, "unloop_draft_claim", map[string]any{"objective": objectives["open"].String(),
		"assertion": "MKTAYIQKLKMSSAVLGSFLLALSLVSSQSVFA", "significance": "Clears 75C.", "evidence": evidence})
	if m.Status != toolapi.StatusOK {
		t.Fatalf("draft: %s", m.Content)
	}
	var out struct {
		Hash, Claim, Fee string
		Body             map[string]any
	}
	json.Unmarshal([]byte(m.Content), &out)
	// The hash is chain.ClaimHash's, of the body without its own hash.
	body, _ := decoded(out.Body)
	delete(body, "claimHash")
	if h, _ := chain.ClaimHash(body, "semantic"); h != out.Hash {
		t.Errorf("hash %s, recomputed %s", out.Hash, h)
	}
	h, _ := hex.DecodeString(out.Hash)
	o := objectives["open"]
	if want := must(solana.FindAddress(program, []byte("claim"), o[:], h, w.me[:])); out.Claim != want.String() {
		t.Errorf("claim %s, want %s", out.Claim, want)
	}
	if out.Fee != "6 protocol tokens" {
		t.Errorf("fee %q: two tokens to each of three validators", out.Fee)
	}
	ev, _ := out.Body["evidence"].(map[string]any)
	if p, _ := ev["procedural"].([]any); len(p) != 1 || p[0].(map[string]any)["id"] != "P1" || ev["semantic"] == nil {
		t.Errorf("evidence %v", ev)
	}

	refusals := map[string]map[string]any{
		"unverified terms":      {"objective": objectives["unverified"].String(), "assertion": "a", "significance": "b", "evidence": evidence},
		"closed for claims":     {"objective": objectives["closed"].String(), "assertion": "a", "significance": "b", "evidence": evidence},
		"evidence from nowhere": {"objective": objectives["open"].String(), "assertion": "a", "significance": "b", "evidence": []any{map[string]any{"kind": "semantic", "statement": "s"}}},
		"no evidence":           {"objective": objectives["open"].String(), "assertion": "a", "significance": "b", "evidence": []any{}},
		"too many roots":        {"objective": objectives["open"].String(), "assertion": "a", "significance": "b", "evidence": evidence, "roots": strings.Split(strings.Repeat(objectives["open"].String()+",", 9), ",")[:9]},
	}
	for name, p := range refusals {
		if m := use(t, tools, "unloop_draft_claim", p); m.Status != toolapi.StatusError {
			t.Errorf("%s: drafted: %s", name, m.Content)
		}
	}
}

func TestFilingWaitsForTheAutonomyOrThePerson(t *testing.T) {
	draftAndFile := func(w *worker) toolapi.ToolMessage {
		m := use(t, w.tools(), "unloop_draft_claim", map[string]any{"objective": "HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu",
			"assertion": "x", "significance": "y", "evidence": evidence})
		var d struct{ Hash string }
		json.Unmarshal([]byte(m.Content), &d)
		return use(t, w.tools(), "unloop_file_claim", map[string]any{"hash": d.Hash})
	}
	for _, c := range []struct {
		autonomy, limit string
		auto            bool
	}{{"tell", "0", false}, {"answer", "100", false}, {"act", "5", false}, {"act", "6", true}} {
		w, r, _ := testWorker(t, c.autonomy, c.limit, `{}`)
		m := draftAndFile(w)
		time.Sleep(50 * time.Millisecond)
		list := w.approvals.recent(5)
		if len(list) != 1 || list[0].Spend != "6000000000" || list[0].Shown != "6" {
			t.Fatalf("%s/%s: %+v", c.autonomy, c.limit, list)
		}
		if got := len(r.kinds()) == 1; got != c.auto {
			t.Errorf("%s with %s a day: started by itself %v, want %v (%s)", c.autonomy, c.limit, got, c.auto, m.Content)
		}
		if !c.auto && !strings.Contains(m.Content, "/unloop/approvals") {
			t.Errorf("the model was not told where the person approves it: %s", m.Content)
		}
	}
	if m := use(t, testWorkerTools(t), "unloop_file_claim", map[string]any{"hash": "nope"}); m.Status != toolapi.StatusError {
		t.Error("filed a claim never drafted")
	}
}

func testWorkerTools(t *testing.T) []toolapi.Tool {
	w, _, _ := testWorker(t, "tell", "0", `{}`)
	return w.tools()
}

func TestTheApprovalsPageTakesDecisionsOnlyFromItself(t *testing.T) {
	w, r, _ := testWorker(t, "tell", "0", `{}`)
	h := w.approvals.handler("127.0.0.1:7777", w.shown)
	propose := func() string {
		w.approvals.propose(&Action{Kind: "collect", Impact: toolapi.ImpactControl, Summary: "Collect"}, "127.0.0.1:7777")
		return w.approvals.recent(1)[0].ID
	}
	do := func(method, path, host string, headers map[string]string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Host = host
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	id := propose()
	if c := do("GET", "/unloop/approvals", "127.0.0.1:7777", nil); c != 200 {
		t.Errorf("the page: %d", c)
	}
	if c := do("GET", "/unloop/approvals", "attacker.example:7777", nil); c != http.StatusMisdirectedRequest {
		t.Errorf("a rebound host name: %d", c)
	}
	if c := do("POST", "/unloop/approvals/"+id+"/approve", "127.0.0.1:7777", map[string]string{"Sec-Fetch-Site": "cross-site"}); c != http.StatusForbidden {
		t.Errorf("approved from another site: %d", c)
	}
	if c := do("POST", "/unloop/approvals/"+id+"/approve", "127.0.0.1:7777", map[string]string{"Origin": "https://attacker.example"}); c != http.StatusForbidden {
		t.Errorf("approved from another origin: %d", c)
	}
	if len(r.kinds()) != 0 {
		t.Fatal("ran without approval")
	}
	if c := do("POST", "/unloop/approvals/"+id+"/approve", "127.0.0.1:7777", map[string]string{"Sec-Fetch-Site": "same-origin"}); c != http.StatusSeeOther {
		t.Errorf("approved from the page: %d", c)
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.kinds(); len(got) != 1 {
		t.Errorf("ran %v", got)
	}
	if c := do("POST", "/unloop/approvals/"+id+"/approve", "127.0.0.1:7777", map[string]string{"Sec-Fetch-Site": "same-origin"}); c != http.StatusConflict {
		t.Errorf("approved twice: %d", c)
	}
	declined := propose()
	do("POST", "/unloop/approvals/"+declined+"/decline", "127.0.0.1:7777", map[string]string{"Origin": "http://127.0.0.1:7777"})
	if w.approvals.recent(1)[0].State != "declined" {
		t.Error("not declined")
	}
	// Running when the agent stopped: failed, never run twice.
	w.approvals.mu.Lock()
	w.approvals.actions[0].State = "running"
	w.approvals.save()
	w.approvals.mu.Unlock()
	if again := newApprovals(context.Background(), w.dir, "tell"); again.actions[0].State != "failed" {
		t.Errorf("after a restart: %s", again.actions[0].State)
	}
}

func TestTheThreadAndValidating(t *testing.T) {
	w, r, _ := testWorker(t, "answer", "0", `{}`)
	tools := w.tools()
	claim := "Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer"
	// Each body passes the schemas before anything is proposed, numbered from the thread.
	if m := use(t, tools, "unloop_answer", map[string]any{"claim": claim, "addresses": []any{1.0, 3.0}, "detail": "The seed is in step 2."}); m.Status != toolapi.StatusOK {
		t.Fatalf("answer: %s", m.Content)
	}
	if m := use(t, tools, "unloop_amend", map[string]any{"claim": claim, "evidence": evidence, "detail": "The knockout."}); m.Status != toolapi.StatusOK {
		t.Fatalf("amend: %s", m.Content)
	}
	if m := use(t, tools, "unloop_object", map[string]any{"claim": claim, "blocker": "step_missing", "detail": "No seed."}); m.Status != toolapi.StatusOK {
		t.Fatalf("object: %s", m.Content)
	}
	list := w.approvals.recent(3)
	amendment := list[1].Detail.(map[string]any)
	if amendment["submission"] != json.Number("2") || !strings.Contains(fmt.Sprint(amendment["evidence"]), "A2P1") {
		t.Errorf("amendment %v", amendment)
	}
	if answer := list[2].Detail.(map[string]any); answer["seq"] != json.Number("3") {
		t.Errorf("answer %v", answer)
	}
	time.Sleep(50 * time.Millisecond)
	if got := r.kinds(); len(got) != 3 {
		t.Errorf("at `answer` the thread posts start by themselves: %v", got)
	}
	if m := use(t, tools, "unloop_answer", map[string]any{"claim": "2sz8kf41u9FvmPkzbes2BzFeoW6h5jfJcBAAq2jY57sc", "addresses": []any{1.0}, "detail": "x"}); m.Status != toolapi.StatusError {
		t.Error("answered on somebody else's claim")
	}
	// At `tell` the same post waits for the person.
	quiet, quietRan, _ := testWorker(t, "tell", "0", `{}`)
	use(t, quiet.tools(), "unloop_object", map[string]any{"claim": claim, "blocker": "step_missing", "detail": "No seed."})
	time.Sleep(50 * time.Millisecond)
	if len(quietRan.kinds()) != 0 || quiet.approvals.recent(1)[0].State != "pending" {
		t.Error("at `tell` a thread post went out by itself")
	}

	// Committing keeps the finding before anything is proposed, and once.
	m := use(t, tools, "unloop_commit", map[string]any{"claim": claim, "reason": "none", "claimSupport": "pass", "objectiveAlignment": "pass",
		"noveltyBps": 7000.0, "scoreBps": 8100.0, "reproduction": "pass", "rationale": "Reproduced."})
	if m.Status != toolapi.StatusOK {
		t.Fatalf("commit: %s %s %s", m.Status, m.Content, m.Detail)
	}
	var s sealedFinding
	if err := w.read("sealed", claim, &s); err != nil {
		t.Fatal(err)
	}
	f, _ := chain.ReadFinding(s.Message)
	var nonce [32]byte
	b, _ := hex.DecodeString(s.Nonce)
	copy(nonce[:], b)
	if c, _ := chain.Commitment(must(solana.ParseKey(claim)), w.me, f, nonce); hex.EncodeToString(c[:]) != s.Commitment {
		t.Error("the kept commitment is not the finding's")
	}
	if fh, _ := chain.FindingHash(s.Message); fh != s.Message["finding"] {
		t.Error("the finding hash is not its detail's")
	}
	if m := use(t, tools, "unloop_commit", map[string]any{"claim": claim, "reason": "none", "claimSupport": "fail", "objectiveAlignment": "pass",
		"noveltyBps": 0.0, "scoreBps": 0.0, "reproduction": "fail"}); m.Status != toolapi.StatusError {
		t.Error("committed twice")
	}
	if m := use(t, tools, "unloop_reveal", map[string]any{"claim": claim}); m.Status != toolapi.StatusOK {
		t.Errorf("reveal: %s", m.Content)
	}
	if got := w.approvals.recent(1)[0]; got.Kind != "reveal" || got.State != "pending" {
		t.Errorf("at `answer` a transaction waits for the person: %+v", got)
	}
}

func TestTheLoop(t *testing.T) {
	duties := `{"sealed":[{"claim":"Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer","window":"open","validationDeadline":"1792826870"},
{"claim":"2sz8kf41u9FvmPkzbes2BzFeoW6h5jfJcBAAq2jY57sc","window":"sealed","validationDeadline":"1792826870"}],
"awaiting":[{"claim":"7v3P6TB4VSyWjLdDsqXZ8rqqMqYZR5i5AAqnwz3kH1XD","commitDeadline":"1792222070"}]}`
	for _, autonomy := range []string{"tell", "act"} {
		w, r, _ := testWorker(t, autonomy, "0", duties)
		in := newInbox(w.dir)
		var begun []string
		l := &loop{worker: w, inbox: in, platform: w.platform, autonomy: autonomy, noticed: map[string]bool{},
			begin: func(id string, _ any) { begun = append(begun, id) }}

		// An objection to a claim this agent filed starts a run, once.
		obj := message(t, "cannot_verify")
		w.keep("claims", "c", filed{Claim: "Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer", Colony: obj.Colony, Hash: obj.Body["claimHash"].(string)})
		l.deliver(bus.Topic{Channel: bus.Claims, Colony: obj.Colony}, obj)
		l.deliver(bus.Topic{Channel: bus.Claims, Colony: obj.Colony}, obj)
		l.deliver(bus.Topic{Channel: bus.Claims, Colony: obj.Colony}, message(t, "clarification"))
		if len(begun) != 1 {
			t.Errorf("%s: runs begun %v", autonomy, begun)
		}

		// The duties: a note for being drawn; the open finding opened at `act`.
		w.keep("sealed", "Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer", sealedFinding{Claim: "Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer", Message: map[string]any{}})
		l.duties(context.Background())
		l.duties(context.Background())
		time.Sleep(50 * time.Millisecond)
		notes := 0
		for _, h := range in.since(time.Time{}, 50) {
			if h.Type == "note" {
				notes++
			}
		}
		if notes != 2 {
			t.Errorf("%s: %d notes, want one for the draw and one for the open finding", autonomy, notes)
		}
		want := 0
		if autonomy == "act" {
			want = 1
		}
		if got := r.kinds(); len(got) != want {
			t.Errorf("%s: actions run %v", autonomy, got)
		}
		if proposed := len(w.approvals.recent(5)); proposed != want {
			t.Errorf("%s: %d actions proposed; below `act` the loop only tells", autonomy, proposed)
		}
	}
}

func TestTheDayLimitCountsWhatWasSpent(t *testing.T) {
	w, r, _ := testWorker(t, "act", "10", `{}`)
	for i := 0; i < 3; i++ {
		w.approvals.propose(&Action{Kind: "file_claim", Impact: toolapi.ImpactControl, Summary: "file", Spend: "4000000000"}, "x")
	}
	time.Sleep(50 * time.Millisecond)
	if got := len(r.kinds()); got != 2 {
		t.Errorf("ran %d of three 4-token claims under a 10-token day", got)
	}
	if spent := w.approvals.spentToday(); spent.Cmp(big.NewInt(8_000_000_000)) != 0 {
		t.Errorf("spent %v", spent)
	}
}
