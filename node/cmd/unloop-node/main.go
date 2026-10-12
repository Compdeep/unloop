// Command unloop-node is the platform's node (docs/PLATFORM.md section 6, "The
// platform's node"): the peer that was listening. It follows the directory
// and both topics of every colony, posts every message it accepts to the
// platform's server, and publishes, under its own peer id, what a wallet
// signed and the server handed it.
//
// It holds one key, its peer identity, which can sign as no message's author
// and move no money. Configured by environment, as the server is:
//
//	SOLANA_RPC_URL           the chain it checks authors against
//	UNLOOP_PROGRAM_ID        the program
//	UNLOOP_NODE_KEY          the peer key's file, made on first start (node/.data/peer.key)
//	UNLOOP_NODE_LISTEN       multiaddrs, comma separated (TCP and QUIC on 4001)
//	UNLOOP_NODE_ANNOUNCE     what to tell other nodes to dial, when not what it listens on
//	UNLOOP_NODE_PEERS        nodes to dial at start, comma separated
//	UNLOOP_NODE_API          its own HTTP, loopback only (127.0.0.1:5336)
//	UNLOOP_PLATFORM_INTERNAL the server's loopback-only listener (http://127.0.0.1:5337)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/solana"
	"github.com/libp2p/go-libp2p/core/peer"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[unloop-node] ")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func list(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func run(ctx context.Context) error {
	rpc := solana.NewRPC(env("SOLANA_RPC_URL", "http://127.0.0.1:8899"))
	programID, err := solana.ParseKey(env("UNLOOP_PROGRAM_ID", "3bPsoRTKT1J9U6V6Zua42VKHVBZ9XAuBPJq3RFFF3Qdz"))
	if err != nil {
		return err
	}
	program := solana.NewProgram(rpc, programID)
	genesis, err := rpc.GenesisHash(ctx)
	if err != nil {
		return err
	}
	cluster, err := bus.Cluster(genesis)
	if err != nil {
		return err
	}
	key, err := bus.Identity(env("UNLOOP_NODE_KEY", "node/.data/peer.key"))
	if err != nil {
		return err
	}
	platform := newPlatform(env("UNLOOP_PLATFORM_INTERNAL", "http://127.0.0.1:5337"))

	node, err := bus.New(bus.Config{
		Key:      key,
		Listen:   list(env("UNLOOP_NODE_LISTEN", "/ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1")),
		Announce: list(env("UNLOOP_NODE_ANNOUNCE", "")),
		Cluster:  cluster,
		Accounts: program,
		Bodies:   platform,
		Deliver:  platform.deliver(ctx),
	})
	if err != nil {
		return err
	}
	defer node.Close()
	platform.node = node
	log.Printf("peer %s on cluster %s", node.ID(), cluster)
	for _, a := range node.Info().Addrs {
		log.Printf("listening %s", a)
	}

	for _, s := range list(env("UNLOOP_NODE_PEERS", "")) {
		info, err := peer.AddrInfoFromString(s)
		if err == nil {
			err = node.Connect(ctx, *info)
		}
		if err != nil {
			log.Printf("peer %s: %v", s, err)
		}
	}

	if err := node.Join(bus.Topic{Channel: bus.Directory}); err != nil {
		return err
	}
	go follow(ctx, node, program)

	api := serveAPI(env("UNLOOP_NODE_API", "127.0.0.1:5336"), node, cluster)
	defer api.Close()
	<-ctx.Done()
	log.Print("stopping")
	return nil
}

// follow joins both topics of every colony, and of each new one within a
// minute of its account appearing. The archive cannot pick and choose.
func follow(ctx context.Context, node *bus.Node, program *solana.Program) {
	joined := map[string]bool{}
	for {
		slugs, err := program.Colonies(ctx)
		if err != nil {
			log.Printf("colonies: %v", err)
		}
		for _, slug := range slugs {
			if joined[slug] {
				continue
			}
			for _, ch := range []bus.Channel{bus.Colony, bus.Claims} {
				if err := node.Join(bus.Topic{Channel: ch, Colony: slug}); err != nil {
					log.Printf("join %s/%s: %v", ch, slug, err)
				}
			}
			joined[slug] = true
			log.Printf("following %s", slug)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}
