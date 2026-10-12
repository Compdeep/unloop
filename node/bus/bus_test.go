package bus

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Compdeep/unloop/node/envelope"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Several nodes in one process, on loopback, speaking about the signed
// envelopes of protocol/vectors/envelopes.json.

const cluster = "0123456789abcdef"

var (
	claims    = Topic{Channel: Claims, Colony: "protein-design"}
	colony    = Topic{Channel: Colony, Colony: "protein-design"}
	directory = Topic{Channel: Directory}
)

// vector is a signed envelope from the vectors, as it goes on the wire, and
// its id.
func vector(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	b, err := os.ReadFile("../../protocol/vectors/envelopes.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs struct {
		Envelopes []struct {
			Name     string          `json:"name"`
			ID       string          `json:"id"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"envelopes"`
	}
	if err := json.Unmarshal(b, &vs); err != nil {
		t.Fatal(err)
	}
	for _, e := range vs.Envelopes {
		if e.Name == name {
			return e.Envelope, e.ID
		}
	}
	t.Fatalf("no vector %q", name)
	return nil, ""
}

// accountsFunc is the chain as a test wants it to answer.
type accountsFunc func(context.Context, *envelope.Message) error

func (f accountsFunc) Authorize(ctx context.Context, m *envelope.Message) error { return f(ctx, m) }

var anyone = accountsFunc(func(context.Context, *envelope.Message) error { return nil })

// inbox is what a node delivered.
type inbox struct {
	mu  sync.Mutex
	got []string
}

func (in *inbox) deliver(_ Topic, m *envelope.Message) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.got = append(in.got, m.ID())
}

func (in *inbox) ids() []string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return slices.Clone(in.got)
}

func key(t *testing.T) crypto.PrivKey {
	t.Helper()
	k, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func start(t *testing.T, cfg Config) *Node {
	t.Helper()
	cfg.Key = key(t)
	cfg.Listen = []string{"/ip4/127.0.0.1/tcp/0"}
	cfg.Cluster = cluster
	if cfg.Accounts == nil {
		cfg.Accounts = anyone
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func connect(t *testing.T, a interface {
	Connect(context.Context, peer.AddrInfo) error
}, b peer.AddrInfo) {
	t.Helper()
	if err := a.Connect(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

func TestTopicNames(t *testing.T) {
	if got := claims.Name(cluster); got != "/unloop/"+cluster+"/claims/protein-design" {
		t.Error(got)
	}
	if got := directory.Name(cluster); got != "/unloop/"+cluster+"/directory" {
		t.Error(got)
	}
	// Devnet's genesis hash.
	if got, err := Cluster("EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"); err != nil || len(got) != 16 {
		t.Errorf("%q %v", got, err)
	}
	if _, err := Cluster("not a hash"); err == nil {
		t.Error("a cluster from nothing")
	}
	n := start(t, Config{})
	for _, bad := range []Topic{{Channel: Claims}, {Channel: Directory, Colony: "x"}, {Channel: "chat", Colony: "x"}} {
		if err := n.Join(bad); err == nil {
			t.Errorf("joined %+v", bad)
		}
	}
}

// What a node refuses on its own, before any peer is involved, and gossipsub's
// word for each refusal.
func TestJudge(t *testing.T) {
	ctx := context.Background()
	var answer atomic.Pointer[error]
	set := func(err error) { answer.Store(&err) }
	set(nil)
	n := start(t, Config{Wait: 200 * time.Millisecond, Accounts: accountsFunc(func(context.Context, *envelope.Message) error {
		return *answer.Load()
	})})
	claim, id := vector(t, "claim.publish")
	objective, _ := vector(t, "objective.publish")

	if got, err := n.Publish(ctx, claims, claim); err != nil || got != id {
		t.Fatalf("a good claim: %s %v", got, err)
	}
	if _, err := n.Publish(ctx, directory, objective); err != nil {
		t.Errorf("an objective on the directory: %v", err)
	}
	refusals := []struct {
		name  string
		topic Topic
		data  []byte
		want  error
	}{
		{"a claim on the colony channel", colony, claim, ErrChannel},
		{"a claim on the directory", directory, claim, ErrChannel},
		{"another colony's claim", Topic{Channel: Claims, Colony: "other-colony"}, claim, ErrChannel},
		{"a bad signature", claims, first(vector(t, "signed by another key")), envelope.ErrSignature},
		{"a bad schema", claims, first(vector(t, "invalid-claim-without-chain")), envelope.ErrSchema},
		{"not JSON", claims, []byte("{"), envelope.ErrSchema},
	}
	for _, r := range refusals {
		if _, err := n.Publish(ctx, r.topic, r.data); !errors.Is(err, r.want) {
			t.Errorf("%s: %v, want %v", r.name, err, r.want)
		} else if verdict(err) != pubsub.ValidationReject {
			t.Errorf("%s: not rejected", r.name)
		}
	}

	set(ErrNotAuthor)
	if _, err := n.Publish(ctx, claims, claim); !errors.Is(err, ErrNotAuthor) || verdict(err) != pubsub.ValidationReject {
		t.Errorf("an author the account does not name: %v", err)
	}
	set(ErrNoAccount)
	start := time.Now()
	if _, err := n.Publish(ctx, claims, claim); !errors.Is(err, ErrNoAccount) || verdict(err) != pubsub.ValidationIgnore {
		t.Errorf("no account: %v", err)
	}
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Errorf("gave up on the account after %s", waited)
	}
	set(errors.New("the RPC is down"))
	if _, err := n.Publish(ctx, claims, claim); err == nil || verdict(err) != pubsub.ValidationIgnore {
		t.Errorf("the node's own trouble blamed on the sender: %v", err)
	}
}

func first(b []byte, _ string) []byte { return b }

// One signed message, published by two nodes, reaches a third once, under
// the id the browser computed.
func TestOneMessageWhoeverRelaysIt(t *testing.T) {
	ctx := context.Background()
	var inA, inB, inC inbox
	a, b := start(t, Config{Deliver: inA.deliver}), start(t, Config{Deliver: inB.deliver})
	c := start(t, Config{Deliver: inC.deliver})
	connect(t, a, c.Info())
	connect(t, b, c.Info())
	for _, n := range []*Node{a, b, c} {
		if err := n.Join(claims); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "a and b see c on the topic", func() bool {
		return slices.Contains(a.Peers(claims), c.ID()) && slices.Contains(b.Peers(claims), c.ID())
	})

	claim, id := vector(t, "claim.publish")
	if _, err := a.Publish(ctx, claims, claim); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, claims, claim); err != nil {
		t.Fatal(err)
	}
	eventually(t, "c hears it", func() bool { return len(inC.ids()) > 0 })
	eventually(t, "b hears it", func() bool { return len(inB.ids()) > 0 })
	time.Sleep(1500 * time.Millisecond) // a heartbeat, for any second copy to arrive
	for name, in := range map[string]*inbox{"a": &inA, "b": &inB, "c": &inC} {
		if got := in.ids(); len(got) != 1 || got[0] != id {
			t.Errorf("%s delivered %v, want once %s", name, got, id)
		}
	}
}

// A peer that sends what fails its signature is rejected, and its score falls.
func TestRejectLowersTheSendersScore(t *testing.T) {
	var in inbox
	honest := start(t, Config{Deliver: in.deliver})
	if err := honest.Join(claims); err != nil {
		t.Fatal(err)
	}

	// A peer with no validators of its own, as a careless or hostile node is.
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ps, err := pubsub.NewGossipSub(context.Background(), h, pubsub.WithFloodPublish(true))
	if err != nil {
		t.Fatal(err)
	}
	topic, err := ps.Join(claims.Name(cluster))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := topic.Subscribe(); err != nil {
		t.Fatal(err)
	}
	connect(t, h, honest.Info())
	eventually(t, "the hostile peer sees the honest one", func() bool { return slices.Contains(topic.ListPeers(), honest.ID()) })

	forged, _ := vector(t, "signed by another key")
	if err := topic.Publish(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the sender's score falls", func() bool { return honest.Score(h.ID()) < 0 })
	if got := in.ids(); len(got) != 0 {
		t.Errorf("delivered a forgery: %v", got)
	}
}

// A node whose RPC is behind ignores a message it has no account for yet,
// without blaming the peer that was ahead of it; one that finds the account
// within the wait accepts it.
func TestNoAccountYetIsNobodysFault(t *testing.T) {
	ctx := context.Background()
	claim, id := vector(t, "claim.publish")

	t.Run("ignored", func(t *testing.T) {
		var in inbox
		behind := start(t, Config{Deliver: in.deliver, Wait: 300 * time.Millisecond,
			Accounts: accountsFunc(func(context.Context, *envelope.Message) error { return ErrNoAccount })})
		ahead := start(t, Config{})
		connect(t, ahead, behind.Info())
		behind.Join(claims)
		ahead.Join(claims)
		eventually(t, "ahead sees behind", func() bool { return slices.Contains(ahead.Peers(claims), behind.ID()) })
		if _, err := ahead.Publish(ctx, claims, claim); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2500 * time.Millisecond) // the wait, and a score inspection after it
		if got := in.ids(); len(got) != 0 {
			t.Errorf("delivered with no account: %v", got)
		}
		if s := behind.Score(ahead.ID()); s < 0 {
			t.Errorf("the peer that was ahead scored %v", s)
		}
	})

	t.Run("found within the wait", func(t *testing.T) {
		var in inbox
		var asked atomic.Int32
		behind := start(t, Config{Deliver: in.deliver, Wait: 3 * time.Second,
			Accounts: accountsFunc(func(context.Context, *envelope.Message) error {
				if asked.Add(1) < 3 {
					return ErrNoAccount
				}
				return nil
			})})
		ahead := start(t, Config{})
		connect(t, ahead, behind.Info())
		behind.Join(claims)
		ahead.Join(claims)
		eventually(t, "ahead sees behind", func() bool { return slices.Contains(ahead.Peers(claims), behind.ID()) })
		if _, err := ahead.Publish(ctx, claims, claim); err != nil {
			t.Fatal(err)
		}
		eventually(t, "behind accepts it once the account appears", func() bool { return len(in.ids()) == 1 })
		if got := in.ids()[0]; got != id {
			t.Errorf("delivered %s, want %s", got, id)
		}
	})
}

// bodies serves what it holds, under whatever hash it is asked by.
type bodies map[string][]byte

func (b bodies) Body(hash string) ([]byte, bool) { v, ok := b[hash]; return v, ok }

func hashOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestBodies(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"assertion":"the protein folds"}`)
	other := []byte(`{"assertion":"something else"}`)
	holder := start(t, Config{Bodies: bodies{hashOf(body): body}})
	liar := start(t, Config{Bodies: bodies{hashOf(body): other}})
	empty := start(t, Config{})
	reader := start(t, Config{})
	for _, n := range []*Node{holder, liar, empty} {
		connect(t, reader, n.Info())
	}

	if got, err := reader.Fetch(ctx, holder.ID(), hashOf(body)); err != nil || string(got) != string(body) {
		t.Errorf("fetched %q, %v", got, err)
	}
	if _, err := reader.Fetch(ctx, liar.ID(), hashOf(body)); !errors.Is(err, ErrWrongBody) {
		t.Errorf("a wrong body: %v", err)
	}
	if _, err := reader.Fetch(ctx, holder.ID(), hashOf(other)); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a body not held: %v", err)
	}
	if _, err := reader.Fetch(ctx, empty.ID(), hashOf(body)); !errors.Is(err, ErrNotHeld) {
		t.Errorf("a node serving nothing: %v", err)
	}
	if _, err := reader.Fetch(ctx, holder.ID(), "abc"); err == nil {
		t.Error("fetched by a hash that is not one")
	}
}

// Send puts a message on every topic its type travels on, and only those.
func TestSendFollowsTheType(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	heard := map[Topic][]string{}
	listener := start(t, Config{Deliver: func(t Topic, m *envelope.Message) {
		mu.Lock()
		defer mu.Unlock()
		heard[t] = append(heard[t], m.Type)
	}})
	sender := start(t, Config{})
	connect(t, sender, listener.Info())
	for _, topic := range []Topic{directory, colony, claims} {
		listener.Join(topic)
	}
	eventually(t, "the sender sees the listener on all three", func() bool {
		return slices.Contains(sender.ps.ListPeers(directory.Name(cluster)), listener.ID()) &&
			slices.Contains(sender.ps.ListPeers(colony.Name(cluster)), listener.ID()) &&
			slices.Contains(sender.ps.ListPeers(claims.Name(cluster)), listener.ID())
	})
	objective, _ := vector(t, "objective.publish")
	claim, _ := vector(t, "claim.publish")
	for _, data := range [][]byte{objective, claim} {
		if _, err := sender.Send(ctx, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sender.Send(ctx, first(vector(t, "signed by another key"))); !errors.Is(err, envelope.ErrSignature) {
		t.Errorf("sent a forgery: %v", err)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(heard[directory]) + len(heard[colony]) + len(heard[claims])
	}
	eventually(t, "three deliveries", func() bool { return count() >= 3 })
	time.Sleep(500 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	want := map[Topic][]string{directory: {"objective.publish"}, colony: {"objective.publish"}, claims: {"claim.publish"}}
	for topic, types := range want {
		if !slices.Equal(heard[topic], types) {
			t.Errorf("%s heard %v, want %v", topic.Name(cluster), heard[topic], types)
		}
	}
}

// A node that leaves a topic hears nothing more on it.
func TestLeave(t *testing.T) {
	ctx := context.Background()
	var in inbox
	listener := start(t, Config{Deliver: in.deliver})
	sender := start(t, Config{})
	connect(t, sender, listener.Info())
	listener.Join(claims)
	eventually(t, "the sender sees the listener", func() bool { return slices.Contains(sender.Peers(claims), listener.ID()) })
	claim, _ := vector(t, "claim.publish")
	sender.Publish(ctx, claims, claim)
	eventually(t, "heard while joined", func() bool { return len(in.ids()) == 1 })
	listener.Leave(claims)
	if len(listener.Joined()) != 0 {
		t.Errorf("still joined to %v", listener.Joined())
	}
	other, _ := vector(t, "amendment")
	sender.Publish(ctx, claims, other)
	time.Sleep(1500 * time.Millisecond)
	if got := in.ids(); len(got) != 1 {
		t.Errorf("heard after leaving: %v", got)
	}
}
