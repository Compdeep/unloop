package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/envelope"
)

func TestTheChatIsLoopbackOnly(t *testing.T) {
	dir := t.TempDir()
	c := Defaults()
	if err := c.Save(dir); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"0.0.0.0:7777", "192.168.1.5:7777", ":7777", "unloop.fun:7777"} {
		c.Chat = addr
		if err := c.Save(dir); err == nil {
			t.Errorf("served the chat on %s", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:7777", "localhost:9000", "[::1]:7777"} {
		c.Chat = addr
		if err := c.Save(dir); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	c.Chat, c.Autonomy = "127.0.0.1:7777", "everything"
	if err := c.Save(dir); err == nil {
		t.Error("an autonomy that is not one of the three")
	}
	c.Autonomy, c.DailyLimit = "act", "-5"
	if err := c.Save(dir); err == nil {
		t.Error("a negative daily limit")
	}
	c.DailyLimit = "2.5"
	if err := c.Save(dir); err != nil {
		t.Error(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "config.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("the config, which holds a model key, is %v", fi.Mode().Perm())
	}
	if _, err := Load(t.TempDir()); err != ErrNotSetUp {
		t.Errorf("an empty directory: %v", err)
	}
}

func TestTheWallet(t *testing.T) {
	dir := t.TempDir()
	key, err := NewWallet(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	read, err := Wallet(dir)
	if err != nil || !read.Equal(key) {
		t.Fatalf("read back %v", err)
	}
	if _, err := NewWallet(dir, ""); err == nil {
		t.Error("replaced a wallet")
	}
	if fi, _ := os.Stat(walletPath(dir)); fi.Mode().Perm() != 0o600 {
		t.Errorf("the wallet is %v", fi.Mode().Perm())
	}
	// Imported from the Solana CLI's file, it is the same keypair.
	other := t.TempDir()
	imported, err := NewWallet(other, walletPath(dir))
	if err != nil || Address(imported) != Address(key) {
		t.Errorf("imported %v", err)
	}
	// A file whose halves are two different keys is not a keypair.
	_, k2, _ := ed25519.GenerateKey(nil)
	mixed := append(append([]byte{}, key[:32]...), k2[32:]...)
	ints := make([]int, 64)
	for i, b := range mixed {
		ints[i] = int(b)
	}
	b, _ := json.Marshal(ints)
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, b, 0o600)
	if _, err := NewWallet(t.TempDir(), bad); err == nil {
		t.Error("imported a keypair whose halves disagree")
	}
}

func message(t *testing.T, name string) *envelope.Message {
	t.Helper()
	b, _ := os.ReadFile("../../protocol/vectors/envelopes.json")
	var v struct {
		Envelopes []struct {
			Name     string          `json:"name"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"envelopes"`
	}
	json.Unmarshal(b, &v)
	for _, e := range v.Envelopes {
		if e.Name == name {
			m, err := envelope.Check(e.Envelope)
			if err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no vector %s", name)
	return nil
}

func TestTheInboxKeepsEachMessageOnceAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	in := newInbox(dir)
	m := message(t, "objective.publish")
	in.deliver(bus.Topic{Channel: bus.Colony, Colony: "protein-design"}, m)
	in.deliver(bus.Topic{Channel: bus.Directory}, m) // the same message on its second topic
	in.deliver(bus.Topic{Channel: bus.Claims, Colony: "protein-design"}, message(t, "claim.publish"))
	if got := in.since(time.Time{}, 10); len(got) != 2 || got[0].Type != "objective.publish" {
		t.Fatalf("heard %v", got)
	}
	again := newInbox(dir)
	if got := again.since(time.Time{}, 10); len(got) != 2 || got[1].Type != "claim.publish" {
		t.Errorf("after a restart, %v", got)
	}
	if got := again.since(time.Now().Add(time.Hour), 10); len(got) != 0 {
		t.Errorf("heard in the future: %v", got)
	}
}

// A platform with one objective, its claim and two colonies.
func site(t *testing.T) *platform {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/objectives", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "nothing" {
			w.Write([]byte(`[]`))
			return
		}
		w.Write([]byte(`[{"address":"HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu","index":"0","title":"Thermostable binders","slug":"protein-design","phase":"open","payout":"prize","offer":"400000000","pool":"1000000000","decimals":6,"claims":1,"claimDeadline":"1792222070","verified":true},
{"address":"4rPuCNpWz6h2NVjzhvuMk9gAYA7hWQ9jnSNCfA6vQLvP","index":"1","title":null,"slug":"protein-design","phase":"open","payout":"rate","offer":"5","pool":"5","decimals":0,"claims":0,"claimDeadline":"0","verified":false}]`))
	})
	mux.HandleFunc("/api/objectives/HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"objective":{"status":"open","payout":"prize","claimDeadline":"1792222070","validatorsPerClaim":3},
"colony":{"slug":"protein-design","leader":"L"},"spec":{"verified":false,"body":{"title":"Thermostable binders"}},
"live":{"decimals":6,"funds":{"vault":"1000000000","encumbered":"400000000","withdrawable":"600000000"}},
"claims":[{"address":"7v3P6TB4VSyWjLdDsqXZ8rqqMqYZR5i5AAqnwz3kH1XD","claimSlot":"9","status":"pending","agent":"A","extra":"dropped"}]}`))
	})
	mux.HandleFunc("/api/colonies", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"slug":"protein-design","address":"C1","leader":"L","admission":"public","validationFee":"0","bond":"0","eligible":3},{"slug":"other","address":"C2"}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return newPlatform(srv.URL + "/")
}

func call(t *testing.T, tools []toolapi.Tool, name string, p map[string]any) toolapi.ToolMessage {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			if tl.Impact(p) != toolapi.ImpactObserve {
				t.Errorf("%s can do more than look", name)
			}
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

func TestTheReadingTools(t *testing.T) {
	var follows []string
	r := &readers{platform: site(t), inbox: newInbox(t.TempDir()),
		follow:   func(c string, on bool) error { follows = append(follows, c); return nil },
		followed: func() []string { return follows }}
	tools := r.tools()

	m := call(t, tools, "unloop_objectives", map[string]any{"query": "binders"})
	if m.Status != toolapi.StatusOK || !strings.Contains(m.Content, `"offer": "400"`) || !strings.Contains(m.Content, "terms not verified") {
		t.Errorf("objectives: %s %s", m.Status, m.Content)
	}
	if m := call(t, tools, "unloop_objectives", map[string]any{"query": "nothing"}); m.Status != toolapi.StatusEmpty {
		t.Errorf("no match reads as %s", m.Status)
	}
	m = call(t, tools, "unloop_objective", map[string]any{"address": "HEEY4mnKL1PTt4HLoLmutNzPhnowADak47YKbSBPYTsu"})
	if !strings.Contains(m.Content, "do not rely on them") || !strings.Contains(m.Content, `"free": "600"`) || strings.Contains(m.Content, "dropped") {
		t.Errorf("objective: %s", m.Content)
	}
	if m := call(t, tools, "unloop_objective", map[string]any{"address": "not an address"}); m.Status != toolapi.StatusError {
		t.Errorf("a bad address reads as %s", m.Status)
	}
	if m := call(t, tools, "unloop_follow", map[string]any{"colony": "no-such"}); m.Status != toolapi.StatusError || len(follows) != 0 {
		t.Errorf("followed a colony that does not exist: %s", m.Content)
	}
	call(t, tools, "unloop_follow", map[string]any{"colony": "protein-design"})
	m = call(t, tools, "unloop_colonies", nil)
	if !strings.Contains(m.Content, `"following": true`) || len(follows) != 1 {
		t.Errorf("colonies: %s", m.Content)
	}
	if m := call(t, tools, "unloop_heard", nil); m.Status != toolapi.StatusEmpty {
		t.Errorf("an empty inbox reads as %s", m.Status)
	}
}
