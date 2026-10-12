// Package bus is a node on the Unloop bus (docs/PLATFORM.md section 6;
// protocol/README.md, "On the wire"): a libp2p host, gossipsub over the
// protocol's topics, and the body protocol.
//
// Every message is judged once, by one function, whichever way it arrives —
// off the wire as gossipsub's topic validator, or handed to Publish:
//
//	reject   fails envelope.Check; a type on a channel it does not travel on;
//	         another colony's message; an author the account does not name.
//	         Gossipsub lowers the sending peer's score
//	ignore   no account behind it yet, after waiting Config.Wait for one; or
//	         the node could not read the chain. Dropped without blame
//	accept   everything else, delivered to Config.Deliver
//
// A message's id is envelope's, so one signed message relayed by two peers
// is one message, on each topic it travels on. The chain is asked through
// Accounts; the bus knows no Solana.
package bus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Compdeep/unloop/node/envelope"
	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	pb "github.com/libp2p/go-libp2p-pubsub/pb"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// Why a message was refused, for errors.Is, beside envelope.ErrSchema and
// envelope.ErrSignature. ErrNoAccount is the one that is nobody's fault.
var (
	ErrChannel   = errors.New("not a message this topic carries")
	ErrNotAuthor = errors.New("not signed by the address its account names")
	ErrNoAccount = errors.New("no account behind it yet")
)

// Accounts is the chain, asked the one question the bus cannot answer from
// the message: whether its author is the address the account it names says
// may sign it (docs/PLATFORM.md section 6, the author rule).
//
// Authorize returns nil if so; an error wrapping ErrNotAuthor if the account
// names somebody else; ErrNoAccount if there is no account yet, which the bus
// asks again until Config.Wait is up. Any other error is the node's own
// trouble reading the chain, and the message is ignored, not blamed.
type Accounts interface {
	Authorize(ctx context.Context, m *envelope.Message) error
}

// Config is a node. Key, Cluster and Accounts are required.
type Config struct {
	// Key is the peer identity: a network identity, never a message's author.
	Key crypto.PrivKey
	// Listen is where to listen, as multiaddrs.
	Listen []string
	// Announce, when set, is the addresses other nodes are told to dial in
	// place of the ones it listens on: a public name for a host behind NAT.
	Announce []string
	// Cluster is the topic names' <cluster> (Cluster).
	Cluster  string
	Accounts Accounts
	// Bodies is what this node serves over the body protocol; nil serves none.
	Bodies Bodies
	// Deliver is handed every message accepted on a joined topic, the node's
	// own publications included.
	Deliver func(Topic, *envelope.Message)
	// Wait is how long a message waits for its account before it is ignored;
	// five seconds if zero.
	Wait time.Duration
}

// Node is a running node.
type Node struct {
	cfg    Config
	host   host.Host
	ps     *pubsub.PubSub
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	topics map[Topic]*pubsub.Topic
	joined map[Topic]context.CancelFunc

	scores atomic.Pointer[map[peer.ID]float64]
}

// New starts a node listening on cfg.Listen. It follows no topic until Join.
func New(cfg Config) (n *Node, err error) {
	if cfg.Key == nil || cfg.Cluster == "" || cfg.Accounts == nil {
		return nil, errors.New("a node needs a key, a cluster and the accounts")
	}
	if cfg.Wait == 0 {
		cfg.Wait = 5 * time.Second
	}
	opts := []libp2p.Option{libp2p.Identity(cfg.Key), libp2p.ListenAddrStrings(cfg.Listen...)}
	if len(cfg.Announce) > 0 {
		announce := make([]ma.Multiaddr, len(cfg.Announce))
		for i, a := range cfg.Announce {
			if announce[i], err = ma.NewMultiaddr(a); err != nil {
				return nil, fmt.Errorf("announce %q: %w", a, err)
			}
		}
		opts = append(opts, libp2p.AddrsFactory(func([]ma.Multiaddr) []ma.Multiaddr { return announce }))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, err
	}
	n = &Node{cfg: cfg, host: h, topics: map[Topic]*pubsub.Topic{}, joined: map[Topic]context.CancelFunc{}}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.ps, err = pubsub.NewGossipSub(n.ctx, h,
		pubsub.WithMessageIdFn(messageID),
		// A node's own publications go to every peer on the topic, not only
		// its mesh, which forms a heartbeat after joining: what a wallet
		// signed is not lost to a node that has just started.
		pubsub.WithFloodPublish(true),
		pubsub.WithPeerScore(peerScore(), thresholds),
		pubsub.WithPeerScoreInspect(pubsub.PeerScoreInspectFn(func(s map[peer.ID]float64) { n.scores.Store(&s) }), time.Second),
	)
	if err != nil {
		h.Close()
		return nil, err
	}
	h.SetStreamHandler(BodyProtocol, n.serveBody)
	return n, nil
}

// Close stops the node.
func (n *Node) Close() error {
	n.cancel()
	return n.host.Close()
}

// ID is the node's peer id: what an envelope it publishes names as `peer`.
func (n *Node) ID() peer.ID { return n.host.ID() }

// Info is where other nodes reach this one.
func (n *Node) Info() peer.AddrInfo {
	return peer.AddrInfo{ID: n.host.ID(), Addrs: n.host.Addrs()}
}

// Connect dials a peer.
func (n *Node) Connect(ctx context.Context, p peer.AddrInfo) error {
	return n.host.Connect(ctx, p)
}

// Peers is who this node knows to follow a topic.
func (n *Node) Peers(t Topic) []peer.ID {
	return n.ps.ListPeers(t.Name(n.cfg.Cluster))
}

// Score is a peer's gossipsub score as of the last second: below zero once it
// has sent what this node rejected.
func (n *Node) Score(p peer.ID) float64 {
	if s := n.scores.Load(); s != nil {
		return (*s)[p]
	}
	return 0
}

// Join follows a topic: every message accepted on it goes to Config.Deliver.
func (n *Node) Join(t Topic) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.joined[t] != nil {
		return nil
	}
	pt, err := n.topic(t)
	if err != nil {
		return err
	}
	sub, err := pt.Subscribe()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(n.ctx)
	n.joined[t] = func() { cancel(); sub.Cancel() }
	go func() {
		for {
			msg, err := sub.Next(ctx)
			if err != nil {
				return
			}
			if n.cfg.Deliver != nil {
				n.cfg.Deliver(t, msg.ValidatorData.(*envelope.Message))
			}
		}
	}()
	return nil
}

// Leave stops following a topic: nothing more on it is delivered. Publishing
// on it still works.
func (n *Node) Leave(t Topic) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if stop := n.joined[t]; stop != nil {
		stop()
		delete(n.joined, t)
	}
}

// Joined is the topics this node follows.
func (n *Node) Joined() []Topic {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Topic, 0, len(n.joined))
	for t := range n.joined {
		out = append(out, t)
	}
	return out
}

// Publish sends a signed envelope on a topic, judged exactly as one arriving
// on it would be; it returns the message's id, or why it was refused.
// Publishing does not need the topic joined.
func (n *Node) Publish(ctx context.Context, t Topic, data []byte) (string, error) {
	m, err := n.judge(ctx, t, data)
	if err != nil {
		return "", err
	}
	n.mu.Lock()
	pt, err := n.topic(t)
	n.mu.Unlock()
	if err != nil {
		return "", err
	}
	return m.ID(), pt.Publish(ctx, data)
}

// Send publishes a signed envelope on every topic its type travels on: its
// colony's channel, and the directory too for an announcement. It returns
// the message's id, or why it was refused.
func (n *Node) Send(ctx context.Context, data []byte) (string, error) {
	m, err := envelope.Check(data)
	if err != nil {
		return "", err
	}
	id := ""
	for _, ch := range carried[m.Type] {
		t := Topic{Channel: ch, Colony: m.Colony}
		if ch == Directory {
			t.Colony = ""
		}
		if id, err = n.Publish(ctx, t, data); err != nil {
			return "", err
		}
	}
	return id, nil
}

// topic is the handle on a topic, opened once with its validator and score.
// n.mu is held.
func (n *Node) topic(t Topic) (*pubsub.Topic, error) {
	if pt, ok := n.topics[t]; ok {
		return pt, nil
	}
	if err := t.valid(); err != nil {
		return nil, err
	}
	name := t.Name(n.cfg.Cluster)
	validate := func(ctx context.Context, _ peer.ID, msg *pubsub.Message) pubsub.ValidationResult {
		m, err := n.judge(ctx, t, msg.Data)
		if err != nil {
			return verdict(err)
		}
		msg.ValidatorData = m
		return pubsub.ValidationAccept
	}
	if err := n.ps.RegisterTopicValidator(name, validate); err != nil {
		return nil, err
	}
	pt, err := n.ps.Join(name)
	if err != nil {
		return nil, err
	}
	if err := pt.SetScoreParams(topicScore); err != nil {
		return nil, err
	}
	n.topics[t] = pt
	return pt, nil
}

// judge is the whole of what the bus asks of a message on a topic.
func (n *Node) judge(ctx context.Context, t Topic, data []byte) (*envelope.Message, error) {
	m, err := envelope.Check(data)
	if err != nil {
		return nil, err
	}
	if err := t.carries(m); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(n.cfg.Wait)
	poll := min(n.cfg.Wait/4, 500*time.Millisecond)
	for {
		err := n.cfg.Accounts.Authorize(ctx, m)
		if err == nil {
			return m, nil
		}
		if !errors.Is(err, ErrNoAccount) || time.Now().Add(poll).After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrNoAccount, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// Blamed is whether a refusal is the sender's fault: rejected, where
// anything else is ignored.
func Blamed(err error) bool {
	for _, fault := range []error{envelope.ErrSchema, envelope.ErrSignature, ErrChannel, ErrNotAuthor} {
		if errors.Is(err, fault) {
			return true
		}
	}
	return false
}

// verdict is gossipsub's word for a refusal.
func verdict(err error) pubsub.ValidationResult {
	if Blamed(err) {
		return pubsub.ValidationReject
	}
	return pubsub.ValidationIgnore
}

// messageID is envelope's id on its topic. Gossipsub drops a message whose
// id it has seen on any topic, and an announcement travels on its colony's
// topic and the directory both: under the bare id, a node following both
// would drop the second copy and never relay it to nodes following only the
// other. What has no id is not an envelope and will be rejected; until then
// it is told apart by its bytes.
func messageID(m *pb.Message) string {
	if id, err := envelope.IDOf(m.Data); err == nil {
		return m.GetTopic() + " " + id
	}
	return m.GetTopic() + " raw:" + string(m.Data)
}

// Scoring is gossipsub's, with one thing counted: messages a peer sent that
// were rejected, which sink it toward being ignored and then cut off.
// Everything else gossipsub can score is left at zero (docs/PLATFORM.md
// section 6, "Not in this facet").
// The peer parameters are made for each node: gossipsub keeps each topic's
// parameters in their map.
func peerScore() *pubsub.PeerScoreParams {
	return &pubsub.PeerScoreParams{
		SkipAtomicValidation: true,
		Topics:               map[string]*pubsub.TopicScoreParams{},
		AppSpecificScore:     func(peer.ID) float64 { return 0 },
		DecayInterval:        time.Second,
		DecayToZero:          0.01,
		RetainScore:          10 * time.Minute,
	}
}

var (
	// One rejection is -100, and they count squared.
	topicScore = &pubsub.TopicScoreParams{
		SkipAtomicValidation:           true,
		TopicWeight:                    1,
		TimeInMeshQuantum:              time.Second, // unweighted; gossipsub divides by it
		InvalidMessageDeliveriesWeight: -100,
		InvalidMessageDeliveriesDecay:  0.99,
	}
	thresholds = &pubsub.PeerScoreThresholds{
		GossipThreshold:   -100,
		PublishThreshold:  -500,
		GraylistThreshold: -1000,
	}
)
