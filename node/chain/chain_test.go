package chain

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Compdeep/unloop/node/envelope"
	"github.com/Compdeep/unloop/node/solana"
)

// protocol/vectors/transactions.json, built by chain/tests/transactions.test.ts.
type vectors struct {
	Program   string `json:"program"`
	Blockhash string `json:"blockhash"`
	Accounts  map[string]struct {
		Owner string `json:"owner"`
		Data  string `json:"data"`
	} `json:"accounts"`
	Cases []struct {
		Name         string                     `json:"name"`
		Action       string                     `json:"action"`
		Params       map[string]json.RawMessage `json:"params"`
		Instructions []wireInstruction          `json:"instructions"`
		Message      string                     `json:"message"`
	} `json:"cases"`
	Hashes []struct {
		Mode string          `json:"mode"`
		Body json.RawMessage `json:"body"`
		Hash string          `json:"hash"`
	} `json:"hashes"`
	Documents []struct {
		Body          json.RawMessage `json:"body"`
		CriteriaHash  string          `json:"criteriaHash"`
		ClaimFormHash string          `json:"claimFormHash"`
	} `json:"documents"`
	Findings []struct {
		Message    json.RawMessage `json:"message"`
		Nonce      string          `json:"nonce"`
		Claim      string          `json:"claim"`
		Validator  string          `json:"validator"`
		Finding    string          `json:"finding"`
		Commitment string          `json:"commitment"`
	} `json:"findings"`
}

type wireInstruction struct {
	Program  string `json:"program"`
	Accounts []struct {
		Key      string `json:"key"`
		Signer   bool   `json:"signer"`
		Writable bool   `json:"writable"`
	} `json:"accounts"`
	Data string `json:"data"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../protocol/vectors/transactions.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func key(s string) solana.Key { return mustKey(s) }

// rpc serves the vectors' accounts, as a chain would.
func rpc(t *testing.T, v vectors) *solana.RPC {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var addr string
		json.Unmarshal(req.Params[0], &addr)
		var value any
		if a, ok := v.Accounts[addr]; ok {
			value = map[string]any{"owner": a.Owner, "data": []string{a.Data, "base64"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"value": value}})
	}))
	t.Cleanup(srv.Close)
	return solana.NewRPC(srv.URL)
}

func (ix Instruction) wire() wireInstruction {
	out := wireInstruction{Program: ix.Program.String(), Data: hex.EncodeToString(ix.Data)}
	for _, m := range ix.Accounts {
		out.Accounts = append(out.Accounts, struct {
			Key      string `json:"key"`
			Signer   bool   `json:"signer"`
			Writable bool   `json:"writable"`
		}{m.Key.String(), m.Signer, m.Writable})
	}
	return out
}

// run is one case's action, from its params, as the agent would call it.
func run(ctx context.Context, c *Client, action string, p map[string]json.RawMessage) ([]Instruction, solana.Key, error) {
	text := func(k string) string { var s string; json.Unmarshal(p[k], &s); return s }
	k := func(name string) solana.Key { return key(text(name)) }
	h := func(name string) (out [32]byte) { b, _ := hex.DecodeString(text(name)); copy(out[:], b); return }
	u64 := func(name string) uint64 { n, _ := new(big.Int).SetString(text(name), 10); return n.Uint64() }
	switch action {
	case "file":
		var roots []string
		json.Unmarshal(p["roots"], &roots)
		var rk []solana.Key
		for _, r := range roots {
			rk = append(rk, key(r))
		}
		var award uint8
		json.Unmarshal(p["awardIndex"], &award)
		ixs, err := c.File(ctx, k("agent"), k("objective"), h("hash"), award, rk, u64("maxFee"))
		return ixs, k("agent"), err
	case "seed":
		ixs, err := c.Seed(ctx, k("caller"), k("claim"))
		return ixs, k("caller"), err
	case "commit":
		ixs, err := c.Commit(ctx, k("claim"), k("validator"), h("commitment"))
		return ixs, k("validator"), err
	case "reveal":
		v, _ := envelope.Decode(p["message"])
		f, err := ReadFinding(v.(map[string]any))
		if err != nil {
			return nil, solana.Key{}, err
		}
		ixs, err := c.Reveal(ctx, k("claim"), k("validator"), f, h("nonce"))
		return ixs, k("validator"), err
	case "collect":
		ixs, err := c.Collect(ctx, k("agent"), k("objective"))
		return ixs, k("agent"), err
	case "collectFee":
		ixs, err := c.CollectFee(ctx, k("validator"), k("claim"))
		return ixs, k("validator"), err
	case "fund":
		ixs, err := c.Fund(ctx, k("objective"), k("payer"), u64("amount"))
		return ixs, k("payer"), err
	case "withdraw":
		ixs, err := c.Withdraw(ctx, k("objective"), k("leader"), u64("amount"))
		return ixs, k("leader"), err
	case "resolve":
		var s Settlement
		for name, dst := range map[string]any{"admit": &s.Admit, "noveltyBps": &s.NoveltyBps, "scoreBps": &s.ScoreBps, "rootsConfirmed": &s.RootsConfirmed} {
			json.Unmarshal(p[name], dst)
		}
		ixs, err := c.Resolve(ctx, k("caller"), k("claim"), s)
		return ixs, k("caller"), err
	case "award":
		ixs, err := c.Award(ctx, k("caller"), k("claim"))
		return ixs, k("caller"), err
	case "edge":
		var i uint8
		json.Unmarshal(p["rootIndex"], &i)
		ixs, err := c.Edge(ctx, k("caller"), k("claim"), i)
		return ixs, k("caller"), err
	case "create":
		var d struct {
			Body   json.RawMessage
			Awards []struct {
				Mode       string
				Allocation string
			}
			CommitDeadline int64
			Pool, URI      string
		}
		json.Unmarshal(p["draft"], &d)
		body, _ := envelope.Decode(d.Body)
		pool, _ := new(big.Int).SetString(d.Pool, 10)
		draft := ObjectiveDraft{Body: body.(map[string]any), CommitDeadline: d.CommitDeadline, Pool: pool.Uint64(), URI: d.URI}
		for _, a := range d.Awards {
			n, _ := new(big.Int).SetString(a.Allocation, 10)
			draft.Awards = append(draft.Awards, Award{Mode: a.Mode, Allocation: n.Uint64()})
		}
		ixs, err := c.Create(ctx, k("colony"), k("leader"), k("objective"), draft)
		return ixs, k("leader"), err
	}
	return nil, solana.Key{}, fmt.Errorf("no action %q", action)
}

func TestInstructionsAsChainClientBuildsThem(t *testing.T) {
	v := load(t)
	r := rpc(t, v)
	c := NewClient(r, solana.NewProgram(r, key(v.Program)))
	for _, tc := range v.Cases {
		ixs, signer, err := run(context.Background(), c, tc.Action, tc.Params)
		if err != nil {
			t.Errorf("%s: %v", tc.Name, err)
			continue
		}
		var got []wireInstruction
		for _, ix := range ixs {
			got = append(got, ix.wire())
		}
		if !reflect.DeepEqual(got, tc.Instructions) {
			g, _ := json.MarshalIndent(got, "", " ")
			w, _ := json.MarshalIndent(tc.Instructions, "", " ")
			t.Errorf("%s:\n got %s\nwant %s", tc.Name, g, w)
			continue
		}

		// The message: the same accounts with the same roles, and the same
		// instructions over them, as web3.js compiled it. web3.js orders
		// accounts within a role its own way, so the order is not compared.
		m, err := Compile(signer, ixs, key(v.Blockhash))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := base64.StdEncoding.DecodeString(tc.Message)
		if g, w := decompile(t, m.Bytes()), decompile(t, want); g != w {
			t.Errorf("%s: the message differs:\n got %s\nwant %s", tc.Name, g, w)
		}
		if len(m.Bytes()) != len(want) {
			t.Errorf("%s: message of %d bytes, web3.js %d", tc.Name, len(m.Bytes()), len(want))
		}
	}
}

// decompile reads a message back into what it says: its header, the fee
// payer, every account with its role, and each instruction over account
// addresses rather than indices.
func decompile(t *testing.T, b []byte) string {
	t.Helper()
	at := 0
	read := func() int {
		n, shift := 0, 0
		for {
			c := b[at]
			at++
			n |= int(c&0x7f) << shift
			if c < 0x80 {
				return n
			}
			shift += 7
		}
	}
	sig, roSig, roUnsig := int(b[0]), int(b[1]), int(b[2])
	at = 3
	keys := make([]string, read())
	for i := range keys {
		keys[i] = solana.Key(b[at : at+32]).String()
		at += 32
	}
	at += 32 // blockhash
	role := map[string]string{}
	for i, k := range keys {
		signer, writable := i < sig, (i < sig-roSig) || (i >= sig && i < len(keys)-roUnsig)
		role[k] = fmt.Sprintf("%v/%v", signer, writable)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "header %d %d %d payer %s roles %v |", sig, roSig, roUnsig, keys[0], role)
	for n := read(); n > 0; n-- {
		program := keys[b[at]]
		at++
		var accounts []string
		for a := read(); a > 0; a-- {
			accounts = append(accounts, keys[b[at]])
			at++
		}
		l := read()
		fmt.Fprintf(&out, " %s %v %x |", program, accounts, b[at:at+l])
		at += l
	}
	return out.String()
}

func TestClaimHashesAsChainClientComputesThem(t *testing.T) {
	for _, h := range load(t).Hashes {
		v, err := envelope.Decode(h.Body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ClaimHash(v.(map[string]any), h.Mode)
		if err != nil || got != h.Hash {
			t.Errorf("%s: %s %v, want %s", h.Mode, got, err, h.Hash)
		}
	}
}

func TestDocumentHashesAsChainClientComputesThem(t *testing.T) {
	docs := load(t).Documents
	if len(docs) == 0 {
		t.Fatal("no documents in the vectors")
	}
	for i, d := range docs {
		v, err := envelope.Decode(d.Body)
		if err != nil {
			t.Fatal(err)
		}
		criteria, form, err := SpecHashes(v.(map[string]any))
		if err != nil || criteria != d.CriteriaHash || form != d.ClaimFormHash {
			t.Errorf("document %d: %s %s %v, want %s %s", i, criteria, form, err, d.CriteriaHash, d.ClaimFormHash)
		}
	}
}

func TestCreateRefusesWhatTheProgramWould(t *testing.T) {
	v := load(t)
	c := NewClient(rpc(t, v), solana.NewProgram(rpc(t, v), key(v.Program)))
	var p map[string]json.RawMessage
	for _, x := range v.Cases {
		if x.Action == "create" {
			p = x.Params
		}
	}
	if p == nil {
		t.Fatal("no create case")
	}
	build := func(edit func(map[string]any, map[string]json.RawMessage)) error {
		q := map[string]json.RawMessage{}
		for k, x := range p {
			q[k] = x
		}
		var d map[string]any
		json.Unmarshal(p["draft"], &d)
		body := d["body"].(map[string]any)
		edit(body, q)
		q["draft"], _ = json.Marshal(d)
		_, _, err := run(context.Background(), c, "create", q)
		return err
	}
	if err := build(func(map[string]any, map[string]json.RawMessage) {}); err != nil {
		t.Fatalf("the vector's own draft: %v", err)
	}
	for name, edit := range map[string]func(map[string]any, map[string]json.RawMessage){
		"edited after sealing": func(b map[string]any, _ map[string]json.RawMessage) { b["title"] = "something else" },
		"another address":      func(b map[string]any, _ map[string]json.RawMessage) { b["id"] = v.Program },
		"not the colony's next": func(b map[string]any, q map[string]json.RawMessage) {
			b["id"] = v.Program
			q["objective"], _ = json.Marshal(v.Program)
		},
		"an unknown payout":  func(b map[string]any, _ map[string]json.RawMessage) { b["payout"] = "lottery" },
		"no panel":           func(b map[string]any, _ map[string]json.RawMessage) { delete(b, "validatorsPerClaim") },
		"a deadline missing": func(b map[string]any, _ map[string]json.RawMessage) { delete(b, "claimDeadline") },
		"an award without its sum": func(b map[string]any, _ map[string]json.RawMessage) {
			b["awards"] = append(b["awards"].([]any), b["awards"].([]any)[0])
		},
		"samples": func(b map[string]any, _ map[string]json.RawMessage) {
			b["samples"] = map[string]any{"items": []any{"x"}}
		},
		"no claim form": func(b map[string]any, _ map[string]json.RawMessage) { delete(b, "claimForm") },
	} {
		if err := build(edit); err == nil {
			t.Errorf("%s: built", name)
		}
	}
}

func TestFindingsAsChainClientCommitsThem(t *testing.T) {
	for _, f := range load(t).Findings {
		v, _ := envelope.Decode(f.Message)
		body := v.(map[string]any)
		if got, err := FindingHash(body); err != nil || got != f.Finding {
			t.Errorf("finding hash %s %v, want %s", got, err, f.Finding)
		}
		finding, err := ReadFinding(body)
		if err != nil {
			t.Fatal(err)
		}
		var nonce [32]byte
		n, _ := hex.DecodeString(f.Nonce)
		copy(nonce[:], n)
		got, err := Commitment(key(f.Claim), key(f.Validator), finding, nonce)
		if err != nil || hex.EncodeToString(got[:]) != f.Commitment {
			t.Errorf("commitment %x %v, want %s", got, err, f.Commitment)
		}
	}
}

// The claims settling reads, as the vectors' world left them: the first
// admitted and unpaid, the second pending and built on the first.
func TestClaimsReadAsSettlingNeedsThem(t *testing.T) {
	v := load(t)
	r := rpc(t, v)
	program := solana.NewProgram(r, key(v.Program))
	var first, second string
	for _, tc := range v.Cases {
		var claim string
		json.Unmarshal(tc.Params["claim"], &claim)
		switch tc.Action {
		case "award":
			first = claim
		case "edge":
			second = claim
		}
	}
	a, err := program.Claim(context.Background(), key(first))
	if err != nil || a == nil {
		t.Fatal(err)
	}
	if a.Status != "admitted" || a.Awarded || len(a.Roots) != 0 {
		t.Errorf("first: %s, awarded %v, %d roots", a.Status, a.Awarded, len(a.Roots))
	}
	b, err := program.Claim(context.Background(), key(second))
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if b.Status != "pending" || b.Awarded || len(b.Roots) != 1 || b.Roots[0] != a.Address || b.Payable(0) {
		t.Errorf("second: %s, awarded %v, roots %v, payable %v", b.Status, b.Awarded, b.Roots, b.Payable(0))
	}
}

func TestDiscriminatorsAreTheIDLs(t *testing.T) {
	b, err := os.ReadFile("../../chain/target/idl/unloop.json")
	if err != nil {
		t.Skip("no IDL; chain/build-idl.sh makes it")
	}
	var idl struct {
		Instructions []struct {
			Name          string `json:"name"`
			Discriminator []int  `json:"discriminator"`
		} `json:"instructions"`
	}
	json.Unmarshal(b, &idl)
	used := map[string]bool{"record_claim": true, "select_validators": true, "commit_validation": true, "reveal_validation": true,
		"collect": true, "collect_validation_fee": true, "top_up_objective": true,
		"resolve_claim": true, "award_claim": true, "mint_edge_reward": true, "withdraw_unencumbered": true}
	for _, ix := range idl.Instructions {
		if !used[ix.Name] {
			continue
		}
		delete(used, ix.Name)
		want := make([]byte, len(ix.Discriminator))
		for i, x := range ix.Discriminator {
			want[i] = byte(x)
		}
		if !bytes.Equal(discriminator(ix.Name), want) {
			t.Errorf("%s: %x, the IDL's %x", ix.Name, discriminator(ix.Name), want)
		}
	}
	if len(used) > 0 {
		t.Errorf("not in the IDL: %v", used)
	}
}

func TestSignedBySignerFirst(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var payer solana.Key
	copy(payer[:], pub)
	other := key("4Nd1mYt8mQuW5Uy7vFkZq2Rj9HhcXeL3sPbTgAoC6wDx")
	m, err := Compile(payer, []Instruction{{Program: SystemProgram, Data: []byte{2}, Accounts: []Meta{w(payer), w(other)}}}, key("11111111111111111111111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := m.Sign(priv)
	if err != nil {
		t.Fatal(err)
	}
	if tx[0] != 1 || !ed25519.Verify(pub, tx[65:], tx[1:65]) || !bytes.Equal(tx[65:], m.Bytes()) {
		t.Error("not one signature over the message by the payer")
	}
	if _, err := (&Message{Signers: 1, Keys: []solana.Key{other}}).Sign(priv); err == nil {
		t.Error("signed for a key it was not given")
	}
	if got := reason([]string{"Program log: AnchorError occurred. Error Code: ClaimClosed. Error Number: 6001. Error Message: claims are closed."}, "x"); got.Error() != "Claims are closed." {
		t.Errorf("reason %q", got)
	}
}

// Send against a chain that refuses in simulation, and one that lands it.
func TestSend(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	var payer solana.Key
	copy(payer[:], priv.Public().(ed25519.PublicKey))
	ixs := []Instruction{{Program: SystemProgram, Data: []byte{2}, Accounts: []Meta{ws(payer)}}}

	serve := func(simErr any, logs []string, statusErr any) (*Client, *[]string) {
		var calls []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			calls = append(calls, req.Method)
			var result any
			switch req.Method {
			case "getLatestBlockhash":
				result = map[string]any{"value": map[string]any{"blockhash": "11111111111111111111111111111111", "lastValidBlockHeight": 100}}
			case "simulateTransaction":
				var tx string
				json.Unmarshal(req.Params[0], &tx)
				raw, _ := base64.StdEncoding.DecodeString(tx)
				if !ed25519.Verify(priv.Public().(ed25519.PublicKey), raw[65:], raw[1:65]) {
					t.Error("simulated a transaction the payer did not sign")
				}
				result = map[string]any{"value": map[string]any{"err": simErr, "logs": logs}}
			case "sendTransaction":
				result = "5ig"
			case "getSignatureStatuses":
				result = map[string]any{"value": []any{map[string]any{"err": statusErr, "confirmationStatus": "confirmed", "slot": 4242}}}
			case "getBlockHeight":
				result = 50
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		}))
		t.Cleanup(srv.Close)
		r := solana.NewRPC(srv.URL)
		return NewClient(r, solana.NewProgram(r, SystemProgram)), &calls
	}

	c, calls := serve(map[string]any{"InstructionError": []any{0, "Custom"}}, []string{"Program log: Error Message: the window is closed."}, nil)
	if _, err := c.Send(context.Background(), priv, ixs); err == nil || err.Error() != "The window is closed." {
		t.Errorf("refused in simulation: %v", err)
	}
	if strings.Contains(strings.Join(*calls, " "), "sendTransaction") {
		t.Error("sent what the simulation refused")
	}

	c, _ = serve(nil, nil, nil)
	if landed, err := c.Send(context.Background(), priv, ixs); err != nil || landed.Signature != "5ig" || landed.Slot != 4242 {
		t.Errorf("landed: %+v %v", landed, err)
	}

	c, _ = serve(nil, nil, map[string]any{"InstructionError": []any{0, "Custom"}})
	if _, err := c.Send(context.Background(), priv, ixs); err == nil {
		t.Error("a transaction that failed on chain reported as landed")
	}
}
