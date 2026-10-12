package solana

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/envelope"
	"github.com/mr-tron/base58"
)

var programID = must(ParseKey("3bPsoRTKT1J9U6V6Zua42VKHVBZ9XAuBPJq3RFFF3Qdz"))

func must(k Key, err error) Key {
	if err != nil {
		panic(err)
	}
	return k
}

func key(s string) Key { return must(ParseKey(s)) }

// Derived by @solana/web3.js's PublicKey.findProgramAddressSync, under the
// program's id. Two of the bumps are below 255, so the curve is consulted.
func TestFindAddressAsWeb3Does(t *testing.T) {
	a, b := key("4Nd1mYt8mQuW5Uy7vFkZq2Rj9HhcXeL3sPbTgAoC6wDx"), key("7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU")
	h, _ := hex.DecodeString("f6ff7c81f54c974ae13508d7b3172a1801248f12599b077090f8d1a5beb8f33e")
	cases := []struct {
		want  string
		seeds [][]byte
	}{
		{"7Zy3tRtMu5DPaQQEdEM1UjKZLZ32CsNjsoDqjznCf8CQ", [][]byte{[]byte("colony"), []byte("protein-design")}},
		{"Hi2NJhQRf2tKq8DucZei8mb8mFNfQecPrsgF3W58ZTer", [][]byte{[]byte("claim"), a[:], h, b[:]}},
		{"4cxFVDsczpHhvzGy14ELPWgnKi9zw7FXuz3UrBk6DjU8", [][]byte{[]byte("validation"), a[:], b[:]}},
		{"2sz8kf41u9FvmPkzbes2BzFeoW6h5jfJcBAAq2jY57sc", [][]byte{[]byte("validator"), a[:], b[:]}},
	}
	for _, c := range cases {
		if got, err := FindAddress(programID, c.seeds...); err != nil || got.String() != c.want {
			t.Errorf("%s: got %s %v", c.seeds[0], got, err)
		}
	}
}

// The offsets Program reads at, held to the program's IDL: the discriminator
// and every field up to the last one read. The IDL is generated
// (chain/build-idl.sh), so a checkout that has not built it skips this.
func TestLayoutsMatchTheIDL(t *testing.T) {
	b, err := os.ReadFile("../../chain/target/idl/unloop.json")
	if err != nil {
		t.Skip("no IDL; chain/build-idl.sh makes it")
	}
	var idl struct {
		Types []struct {
			Name string `json:"name"`
			Type struct {
				Fields []struct {
					Name string          `json:"name"`
					Type json.RawMessage `json:"type"`
				} `json:"fields"`
				// An enum's variants, whose order is its encoding.
				Variants []struct {
					Name string `json:"name"`
				} `json:"variants"`
			} `json:"type"`
		} `json:"types"`
	}
	if err := json.Unmarshal(b, &idl); err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Accounts []struct {
			Name          string `json:"name"`
			Discriminator []int  `json:"discriminator"`
		} `json:"accounts"`
	}
	json.Unmarshal(b, &raw)
	for _, name := range []string{"Colony", "Objective", "Claim", "Validation", "Validator", "Config", "TokenConfig"} {
		found := false
		for _, a := range raw.Accounts {
			if a.Name == name {
				found = true
				got := make([]byte, len(a.Discriminator))
				for i, x := range a.Discriminator {
					got[i] = byte(x)
				}
				if !reflect.DeepEqual(got, discriminator(name)) {
					t.Errorf("%s: discriminator %v, the IDL's %v", name, discriminator(name), got)
				}
			}
		}
		if !found {
			t.Errorf("no %s in the IDL", name)
		}
	}
	want := map[string][]string{
		"Colony": {"leader:pubkey", `slug:string`, "payment_mint:pubkey", `consensus:{"defined":{"name":"ConsensusPolicy"}}`,
			`admission:{"defined":{"name":"Admission"}}`, "admission_authority:pubkey", "entry_proof_required:bool",
			"bond:u64", "probation_validations:u32", "eligible:u32", "trusted:u32", "validation_fee:u64", "objective_count:u64"},
		"Objective": {"colony:pubkey", "settlement_authority:pubkey", "payment_mint:pubkey", `consensus:{"defined":{"name":"ConsensusPolicy"}}`,
			"index:u64", `payout:{"defined":{"name":"Payout"}}`, `status:{"defined":{"name":"ObjectiveStatus"}}`,
			`criteria_hash:{"array":["u8",32]}`, `claim_form_hash:{"array":["u8",32]}`, `entry_proof_root:{"array":["u8",32]}`,
			"entry_proof_count:u32", `awards:{"array":[{"defined":{"name":"Award"}},3]}`, "award_count:u8", "pool:u64",
			"pending_claims:u64", "validators_per_claim:u8", "claim_deadline:i64", "commit_deadline:i64", "validation_deadline:i64"},
		"Award": {`mode:{"defined":{"name":"AwardMode"}}`, "allocation:u64", "awarded:u64", "pending:u64", "awaiting:u64",
			"closed:bool", "first_slot:u64", `first_hash:{"array":["u8",32]}`},
		"Claim": {"objective:pubkey", "agent:pubkey", `hash:{"array":["u8",32]}`, "award_index:u8", "claim_slot:u64",
			`status:{"defined":{"name":"ClaimStatus"}}`, `roots:{"array":[{"array":["u8",32]},8]}`, "root_count:u8",
			"roots_confirmed:u8", "roots_paid:u8", "validation_fee:u64", "selection_seed:u64", "selection_pool:u32",
			"commitments:u16", "reveals:u16", "validations:u16", "novelty_sum:u32", "score_sum:u32", "passes:u16",
			"novelty_lo:u16", "novelty_hi:u16", "score_lo:u16", "score_hi:u16", "novelty_bps:u16", "score_bps:u16", "bump:u8",
			`decided_by:{"option":{"defined":{"name":"DecidedBy"}}}`, `awarded:{"option":"u64"}`},
		"ClaimStatus":     {"Pending", "Admitted", "Rejected"},
		"Config":          {"authority:pubkey", "treasury:pubkey"},
		"TokenConfig":     {"authority:pubkey", "mint:pubkey"},
		"ConsensusPolicy": {"min_votes:u16", "accept_threshold_bps:u16", "reject_threshold_bps:u16"},
		"Payout":          {"Prize", "Rate"},
		"AwardMode":       {"WinnerTakeAll", "Graded"},
		"Admission":       {"Public", "Keyed", "Private"},
	}
	for _, ty := range idl.Types {
		fields, ok := want[ty.Name]
		if !ok {
			continue
		}
		if v := ty.Type.Variants; len(v) > 0 {
			for i, f := range fields {
				if i >= len(v) || v[i].Name != f {
					t.Errorf("%s variant %d is not %s", ty.Name, i, f)
				}
			}
			delete(want, ty.Name)
			continue
		}
		for i, f := range fields {
			var typ string
			if err := json.Unmarshal(ty.Type.Fields[i].Type, &typ); err != nil {
				var compact bytes.Buffer
				json.Compact(&compact, ty.Type.Fields[i].Type)
				typ = compact.String()
			}
			if got := ty.Type.Fields[i].Name + ":" + typ; got != f {
				t.Errorf("%s field %d is %s, read as %s", ty.Name, i, got, f)
			}
		}
		delete(want, ty.Name)
	}
	if len(want) > 0 {
		t.Errorf("not in the IDL: %v", want)
	}
}

// chain is a fake RPC over a handful of accounts.
type chain struct{ accounts map[Key]Account }

func (c *chain) put(address Key, kind string, fields ...[]byte) {
	data := append([]byte{}, discriminator(kind)...)
	for _, f := range fields {
		data = append(data, f...)
	}
	data = append(data, make([]byte, 600)...) // the fields this never reads
	c.accounts[address] = Account{Address: address, Owner: programID, Data: data}
}

func (c *chain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	encode := func(a Account) map[string]any {
		return map[string]any{"owner": a.Owner.String(), "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}}
	}
	var result any
	switch req.Method {
	case "getAccountInfo":
		var addr string
		json.Unmarshal(req.Params[0], &addr)
		if a, ok := c.accounts[key(addr)]; ok {
			result = map[string]any{"value": encode(a)}
		} else {
			result = map[string]any{"value": nil}
		}
	case "getProgramAccounts":
		var opts struct {
			Filters []struct {
				Memcmp struct {
					Offset int    `json:"offset"`
					Bytes  string `json:"bytes"`
				} `json:"memcmp"`
			} `json:"filters"`
		}
		json.Unmarshal(req.Params[1], &opts)
		list := []any{}
	next:
		for k, a := range c.accounts {
			for _, f := range opts.Filters {
				want, _ := base58.Decode(f.Memcmp.Bytes)
				if len(a.Data) < f.Memcmp.Offset+len(want) || string(a.Data[f.Memcmp.Offset:f.Memcmp.Offset+len(want)]) != string(want) {
					continue next
				}
			}
			list = append(list, map[string]any{"pubkey": k.String(), "account": encode(a)})
		}
		result = list
	}
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
}

func address(seeds ...[]byte) Key { return must(FindAddress(programID, seeds...)) }

func addr(n byte) Key { // an address nobody derived
	var k Key
	k[0], k[31] = 1, n
	return k
}

func slug(s string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(s)))
	return append(b, s...)
}

func TestTheAuthorRule(t *testing.T) {
	leader, other, settlement, agent, validator, stranger := addr(1), addr(2), addr(3), addr(4), addr(5), addr(6)
	colony := address([]byte("colony"), []byte("protein-design"))
	elsewhere := address([]byte("colony"), []byte("other-colony"))
	objective, foreign := addr(10), addr(11)
	hash := sha256.Sum256([]byte("a claim"))
	foreignHash := sha256.Sum256([]byte("another colony's claim"))
	claim := address([]byte("claim"), objective[:], hash[:], agent[:])
	foreignClaim := address([]byte("claim"), foreign[:], foreignHash[:], agent[:])

	c := &chain{accounts: map[Key]Account{}}
	c.put(colony, "Colony", leader[:], slug("protein-design"))
	c.put(elsewhere, "Colony", other[:], slug("other-colony"))
	c.put(objective, "Objective", colony[:], settlement[:])
	c.put(foreign, "Objective", elsewhere[:], settlement[:])
	c.put(claim, "Claim", objective[:], agent[:], hash[:])
	c.put(foreignClaim, "Claim", foreign[:], agent[:], foreignHash[:])
	c.put(address([]byte("validation"), claim[:], validator[:]), "Validation", claim[:], validator[:])
	c.put(address([]byte("validator"), colony[:], validator[:]), "Validator", colony[:], validator[:])
	srv := httptest.NewServer(c)
	defer srv.Close()
	p := NewProgram(NewRPC(srv.URL), programID)

	h, fh, missing := hex.EncodeToString(hash[:]), hex.EncodeToString(foreignHash[:]), hex.EncodeToString(make([]byte, 32))
	o, fo := objective.String(), foreign.String()
	cases := []struct {
		name   string
		typ    string
		author Key
		body   map[string]any
		want   error
	}{
		{"the leader announces the colony", "colony.announce", leader, map[string]any{}, nil},
		{"another announces it", "colony.announce", stranger, map[string]any{}, bus.ErrNotAuthor},
		{"the leader publishes an objective", "objective.publish", leader, map[string]any{"id": o}, nil},
		{"the leader updates it", "objective.update", leader, map[string]any{"id": o}, nil},
		{"the settlement authority publishes it", "objective.publish", settlement, map[string]any{"id": o}, bus.ErrNotAuthor},
		{"an objective not yet seen", "objective.publish", leader, map[string]any{"id": addr(99).String()}, bus.ErrNoAccount},
		{"another colony's objective", "objective.publish", leader, map[string]any{"id": fo}, bus.ErrChannel},
		{"the agent publishes its claim", "claim.publish", agent, map[string]any{"objective": o, "claimHash": h}, nil},
		{"the agent's notice", "claim.notice", agent, map[string]any{"objective": o, "claimHash": h}, nil},
		{"another publishes the agent's claim", "claim.publish", stranger, map[string]any{"objective": o, "claimHash": h}, bus.ErrNoAccount},
		{"a claim on another colony's objective", "claim.publish", agent, map[string]any{"objective": fo, "claimHash": fh}, bus.ErrChannel},
		{"the settlement authority resolves", "claim.resolved", settlement, map[string]any{"objective": o, "claimHash": h}, nil},
		{"the leader resolves", "claim.resolved", leader, map[string]any{"objective": o, "claimHash": h}, bus.ErrNotAuthor},
		{"anybody says a claim was paid", "claim.paid", stranger, map[string]any{"objective": o, "claimHash": h}, nil},
		{"paid, for no claim", "claim.paid", stranger, map[string]any{"objective": o, "claimHash": missing}, bus.ErrNoAccount},
		{"a validator with a commitment publishes", "validation.publish", validator, map[string]any{"claimHash": h}, nil},
		{"the agent publishes a validation", "validation.publish", agent, map[string]any{"claimHash": h}, bus.ErrNotAuthor},
		{"a validation of a claim not yet seen", "validation.publish", validator, map[string]any{"claimHash": missing}, bus.ErrNoAccount},
		{"a validation of another colony's claim", "validation.publish", validator, map[string]any{"claimHash": fh}, bus.ErrChannel},
		{"the agent amends", "amendment", agent, map[string]any{"claimHash": h}, nil},
		{"a validator amends", "amendment", validator, map[string]any{"claimHash": h}, bus.ErrNotAuthor},
		{"a validator cannot verify", "cannot_verify", validator, map[string]any{"claimHash": h}, nil},
		{"the agent clarifies", "clarification", agent, map[string]any{"claimHash": h}, nil},
		{"a stranger clarifies", "clarification", stranger, map[string]any{"claimHash": h}, bus.ErrNotAuthor},
		{"a validator says it was selected", "validators.selected", validator, map[string]any{"claim": h}, nil},
		{"a validator in the pool announces", "validator.announce", validator, map[string]any{}, nil},
		{"an address not in the pool announces", "validator.announce", stranger, map[string]any{}, bus.ErrNoAccount},
	}
	for _, c := range cases {
		m := &envelope.Message{Type: c.typ, Colony: "protein-design", Author: c.author.String(), Body: c.body}
		err := p.Authorize(context.Background(), m)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}

	slugs, err := p.Colonies(context.Background())
	if err != nil || len(slugs) != 2 {
		t.Fatalf("colonies %v %v", slugs, err)
	}
	if !(slugs[0] == "protein-design" && slugs[1] == "other-colony" || slugs[1] == "protein-design" && slugs[0] == "other-colony") {
		t.Errorf("colonies %v", slugs)
	}
}

// A claim's settling fields, wherever its Options leave them: awarded sits
// a byte further on once decided_by is filled in.
func TestClaimSettlingFields(t *testing.T) {
	objective, agent, root := addr(10), addr(4), addr(20)
	hash := sha256.Sum256([]byte("a claim"))
	head := func(status byte, confirmed, paid byte) [][]byte {
		roots := make([]byte, 8*32)
		copy(roots, root[:])
		fixed := make([]byte, claimDecidedBy-claimRootCount-3)
		return [][]byte{objective[:], agent[:], hash[:], {0}, make([]byte, 8), {status}, roots, {1, confirmed, paid}, fixed}
	}
	c := &chain{accounts: map[Key]Account{}}
	pending, paid, unpaid := addr(30), addr(31), addr(32)
	c.put(pending, "Claim", append(head(0, 0, 0), []byte{0, 0})...)
	c.put(paid, "Claim", append(head(1, 1, 0), []byte{1, 0, 1}, make([]byte, 8))...)
	c.put(unpaid, "Claim", append(head(1, 1, 1), []byte{1, 1, 0})...)
	srv := httptest.NewServer(c)
	defer srv.Close()
	p := NewProgram(NewRPC(srv.URL), programID)
	for _, tc := range []struct {
		address           Key
		status, decidedBy string
		awarded, payable  bool
	}{{pending, "pending", "", false, false}, {paid, "admitted", "consensus", true, true}, {unpaid, "admitted", "leader", false, false}} {
		got, err := p.Claim(context.Background(), tc.address)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.status || got.DecidedBy != tc.decidedBy || got.Awarded != tc.awarded || got.Payable(0) != tc.payable || len(got.Roots) != 1 || got.Roots[0] != root {
			t.Errorf("%s: %s awarded %v payable %v roots %v", tc.status, got.Status, got.Awarded, got.Payable(0), got.Roots)
		}
	}
}

// A public RPC turns an address away when it asks too often; the call waits
// and asks again rather than failing, and gives up once its waits run out.
func TestRPCWaitsWhenBusy(t *testing.T) {
	saved := busyWaits
	busyWaits = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { busyWaits = saved }()
	refusals := 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refusals > 0 {
			refusals--
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":42}}`))
	}))
	defer srv.Close()
	if got, err := NewRPC(srv.URL).Balance(context.Background(), Key{}); err != nil || got != 42 {
		t.Fatalf("after two refusals: %d, %v", got, err)
	}
	refusals = 3
	if _, err := NewRPC(srv.URL).Balance(context.Background(), Key{}); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("after more refusals than waits: %v", err)
	}
}
