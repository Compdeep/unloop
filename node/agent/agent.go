package agent

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sync"

	kaiju "github.com/Compdeep/kaiju/agent"
	"github.com/Compdeep/kaiju/agent/toolapi"
	"github.com/Compdeep/kaiju/tools"
	"github.com/Compdeep/kaiju/ui"
	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/chain"
	"github.com/Compdeep/unloop/node/solana"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The skill cards the agent ships: how Unloop works and how to find work in
// it, written for the model. Loaded over kaiju's own through its public
// PathConfig, so kaiju gains nothing.
//
//go:embed skills
var skillCards embed.FS

// Endpoints is each provider's API, for a model configured by provider alone.
var Endpoints = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"anthropic":  "https://api.anthropic.com/v1",
	"openrouter": "https://openrouter.ai/api/v1",
}

// Run starts the agent from its data directory and serves its chat until ctx
// ends. ready, if not nil, is called with the chat's address once it takes
// connections.
func Run(ctx context.Context, dir string, cfg Config, ready func(chat string)) error {
	a, err := assemble(ctx, dir, cfg)
	if err != nil {
		return err
	}
	defer a.node.Close()

	// The engine.
	cards, err := fs.Sub(skillCards, "skills")
	if err != nil {
		return err
	}
	endpoint := cfg.Model.Endpoint
	if endpoint == "" {
		endpoint = Endpoints[cfg.Model.Provider]
	}
	ag, err := kaiju.New(kaiju.Config{
		ModelConfig: kaiju.ModelConfig{
			LLMProvider: cfg.Model.Provider, LLMEndpoint: endpoint, LLMAPIKey: cfg.Model.Key, LLMModel: cfg.Model.Name, MaxTokens: 4096,
		},
		PathConfig: kaiju.PathConfig{
			DataDir: filepath.Join(dir, "kaiju"), MetadataDir: filepath.Join(dir, "kaiju"),
			Workspace: filepath.Join(dir, "workspace"), BuiltinSkills: cards,
		},
		IdentityConfig: kaiju.IdentityConfig{NodeID: "unloop-agent"},
		DAGConfig:      kaiju.DAGConfig{DAGEnabled: true, MaxNodes: 20, MaxLLMCalls: 20},
		RoutingConfig:  kaiju.RoutingConfig{ClassifierEnabled: true},
		Handlers:       kaiju.Handlers{DescribeTrigger: a.loop.describe},
	})
	if err != nil {
		return err
	}
	defer ag.Stop()
	// Kaiju's own tools — files, the web, a shell — for the research; then
	// Unloop's. A name taken twice is an error here rather than a tool
	// silently missing.
	if _, err := tools.Register(ag.Registry(), tools.Deps{
		Workspace: filepath.Join(dir, "workspace"), Memory: ag.Memory(), Executor: ag.ExecutorClient(),
	}); err != nil {
		return err
	}
	a.loop.ag, a.loop.begin = ag, a.loop.start
	for _, t := range a.tools() {
		if err := ag.Registry().Register(t); err != nil {
			return fmt.Errorf("registering %s: %w", t.Name(), err)
		}
	}
	go ag.Start(ctx)
	go a.loop.tick(ctx)

	// The chat beside the approvals and the work page. The chat may reach
	// every tool, because no tool signs: what would is approved on the
	// approvals page, which the model cannot reach.
	chatPage, err := ui.Handler(ui.Options{Agent: ag, DefaultIntent: toolapi.ImpactControl, Config: ui.Config{Brand: ui.Brand{Name: "Unloop agent"}}})
	if err != nil {
		return err
	}
	a.pages.Handle("/", chatPage)
	log.Printf("wallet %s · peer %s · cluster %s", a.me, a.node.ID(), a.cluster)
	return a.serve(ctx, ready)
}

// assembled is an agent without its engine: its wallet, its node on the bus,
// its inbox, approvals and tools, and the pages it serves on loopback. Run
// puts a model behind it; the scripted round (round_test.go) calls its tools
// itself, as a model would, and approves on its page, as a person does.
type assembled struct {
	cfg       Config
	me        solana.Key
	cluster   string
	node      *bus.Node
	loop      *loop
	readers   *readers
	work      *worker
	approvals *approvals
	pages     *http.ServeMux
}

func assemble(ctx context.Context, dir string, cfg Config) (*assembled, error) {
	key, err := Wallet(dir)
	if err != nil {
		return nil, fmt.Errorf("the wallet: %w (run `unloop-agent init`)", err)
	}
	me := Address(key)
	rpc := solana.NewRPC(cfg.RPC)
	programID, err := solana.ParseKey(cfg.Program)
	if err != nil {
		return nil, err
	}
	program := solana.NewProgram(rpc, programID)
	genesis, err := rpc.GenesisHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("the chain at %s: %w", cfg.RPC, err)
	}
	cluster, err := bus.Cluster(genesis)
	if err != nil {
		return nil, err
	}
	site := newPlatform(cfg.Platform)
	in := newInbox(dir)
	acts := newApprovals(ctx, dir, cfg.Autonomy)
	lp := &loop{inbox: in, platform: site, autonomy: cfg.Autonomy, noticed: map[string]bool{}, begin: func(string, any) {}}

	// The bus: this agent's own node, dialling out to the platform's to find
	// the rest. It need not be reachable itself.
	identity, err := bus.Identity(filepath.Join(dir, "peer.key"))
	if err != nil {
		return nil, err
	}
	node, err := bus.New(bus.Config{
		Key: identity, Listen: []string{"/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"},
		Cluster: cluster, Accounts: program, Deliver: lp.deliver,
	})
	if err != nil {
		return nil, err
	}
	bootstrap(ctx, site, node)

	var mu sync.Mutex
	follow := func(colony string, on bool) error {
		mu.Lock()
		defer mu.Unlock()
		for _, ch := range []bus.Channel{bus.Colony, bus.Claims} {
			t := bus.Topic{Channel: ch, Colony: colony}
			if !on {
				node.Leave(t)
			} else if err := node.Join(t); err != nil {
				return err
			}
		}
		cfg.Follow = slices.DeleteFunc(cfg.Follow, func(s string) bool { return s == colony })
		if on {
			cfg.Follow = append(cfg.Follow, colony)
		}
		return cfg.Save(dir)
	}
	followed := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(cfg.Follow) }
	if err := node.Join(bus.Topic{Channel: bus.Directory}); err != nil {
		node.Close()
		return nil, err
	}
	for _, colony := range followed() {
		if err := follow(colony, true); err != nil {
			log.Printf("follow %s: %v", colony, err)
		}
	}

	readers := &readers{platform: site, inbox: in, node: node, rpc: rpc, me: me, follow: follow, followed: followed}
	work := &worker{dir: dir, key: key, me: me, program: program, chain: chain.NewClient(rpc, program), node: node,
		platform: site, approvals: acts, chat: cfg.Chat, decimals: -1}
	acts.limit, acts.run, acts.done = work.limit(cfg.DailyLimit), work.run, lp.done
	lp.worker = work

	// The approvals and the page the site links to, on loopback: neither has
	// a sign-in, and the agent holds a wallet.
	pages := http.NewServeMux()
	approvalsPage, workOn := acts.handler(cfg.Chat, work.shown), workHandler(site, follow, followed, cfg.Chat)
	pages.Handle("/unloop/approvals", approvalsPage)
	pages.Handle("/unloop/approvals/", approvalsPage)
	pages.Handle("/unloop/work", workOn)
	pages.Handle("/unloop/work/", workOn)
	return &assembled{cfg: cfg, me: me, cluster: cluster, node: node, loop: lp, readers: readers, work: work, approvals: acts, pages: pages}, nil
}

// tools is every Unloop tool the agent has: the readers, and the working tools.
func (a *assembled) tools() []toolapi.Tool { return append(a.readers.tools(), a.work.all()...) }

// serve serves the agent's pages on its chat address until ctx ends.
func (a *assembled) serve(ctx context.Context, ready func(chat string)) error {
	listener, err := net.Listen("tcp", a.cfg.Chat)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: loopbackHost(a.cfg.Chat, a.pages)}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Printf("following %v; autonomy %s; chat at http://%s, approvals at http://%s/unloop/approvals", a.readers.followed(), a.cfg.Autonomy, listener.Addr(), listener.Addr())
	if ready != nil {
		ready("http://" + listener.Addr().String())
	}
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// bootstrap dials the platform's node, which /api/chain names, to find the
// mesh. Without it the agent reads and follows nothing on the bus, and says so.
func bootstrap(ctx context.Context, site *platform, node *bus.Node) {
	var chain struct {
		Node *struct {
			Addrs []string `json:"addrs"`
		} `json:"node"`
	}
	if err := site.get(ctx, "/api/chain", url.Values{}, &chain); err != nil || chain.Node == nil {
		log.Printf("no platform node to dial (%v); the bus is quiet until a peer finds this one", err)
		return
	}
	for _, a := range chain.Node.Addrs {
		info, err := peer.AddrInfoFromString(a)
		if err == nil {
			if err = node.Connect(ctx, *info); err == nil {
				log.Printf("on the bus through %s", a)
				return
			}
		}
		log.Printf("dial %s: %v", a, err)
	}
}
