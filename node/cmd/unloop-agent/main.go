// Command unloop-agent is a person's own Unloop agent (docs/PLATFORM.md
// section 7; docs/AGENT.md):
//
//	unloop-agent init     set it up: the model, the cluster, the platform, the wallet
//	unloop-agent          start it, and chat with it at http://127.0.0.1:7777
//	unloop-agent address  print the wallet's address
//	unloop-agent version  print which build this is
//
// Everything it keeps is in $UNLOOP_AGENT_HOME, or ~/.unloop-agent.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/Compdeep/unloop/node/agent"
	"golang.org/x/term"
)

// version is set by node/release.sh.
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[unloop-agent] ")
	dir, err := agent.Home()
	if err != nil {
		log.Fatal(err)
	}
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "init":
		err = initialise(dir)
	case "address":
		var key []byte
		if key, err = agent.Wallet(dir); err == nil {
			fmt.Println(agent.Address(key))
		}
	case "version":
		fmt.Println("unloop-agent", version)
	case "", "run":
		var cfg agent.Config
		if cfg, err = agent.Load(dir); err == nil {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			var ready func(string)
			if interactive() {
				ready = browse
			}
			err = agent.Run(ctx, dir, cfg, ready)
		}
	default:
		err = fmt.Errorf("unknown command %q: init, address, version, or none to start", cmd)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// initialise asks four things and writes them down. Run again, it keeps
// what was set, offering each as the default.
func initialise(dir string) error {
	cfg, err := agent.Load(dir)
	if err != nil && !errors.Is(err, agent.ErrNotSetUp) {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	ask := func(q, current string) string {
		if current != "" {
			fmt.Printf("%s [%s]: ", q, current)
		} else {
			fmt.Printf("%s: ", q)
		}
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		return current
	}
	fmt.Printf("Setting up the Unloop agent in %s.\n\n", dir)
	cfg.Model.Provider = ask("Model provider (openai, anthropic, openrouter)", cfg.Model.Provider)
	if _, ok := agent.Endpoints[cfg.Model.Provider]; !ok {
		cfg.Model.Endpoint = ask("Its endpoint (an OpenAI-compatible API)", cfg.Model.Endpoint)
	}
	if key := secret("API key", cfg.Model.Key != "", ask); key != "" {
		cfg.Model.Key = key
	}
	cfg.Model.Name = ask("Model", cfg.Model.Name)
	cfg.RPC = ask("Solana RPC (devnet to start)", cfg.RPC)
	cfg.Platform = ask("Platform to browse through", cfg.Platform)
	fmt.Println("\nHow far the agent goes without asking you on its approvals page:")
	fmt.Println("  tell    nothing; you approve every message and transaction")
	fmt.Println("  answer  it also answers the thread of a claim it filed")
	fmt.Println("  act     it also files, validates and collects, within a daily limit")
	cfg.Autonomy = ask("Autonomy", cfg.Autonomy)
	if cfg.Autonomy == "act" {
		cfg.DailyLimit = ask("Daily limit, in whole protocol tokens", cfg.DailyLimit)
	}
	if err := cfg.Save(dir); err != nil {
		return err
	}

	key, err := agent.Wallet(dir)
	if err != nil {
		from := ask("Wallet: a Solana keypair file to import, or nothing for a new one", "")
		if key, err = agent.NewWallet(dir, from); err != nil {
			return err
		}
	}
	address := agent.Address(key)
	fmt.Printf("\nThe agent's address is %s.\n", address)
	fmt.Println("On devnet, fund it for transaction fees: solana airdrop 1", address, "-u devnet")
	fmt.Println("or at https://faucet.solana.com. Its balances, and whether they are enough:")
	fmt.Printf("  %s/start?agent=%s\n", strings.TrimRight(cfg.Platform, "/"), address)
	fmt.Println("Then run `unloop-agent`, and chat with it at http://" + cfg.Chat)
	return nil
}

// secret asks for a key without echoing it where the input is a terminal, and
// returns "" to keep the one set. Piped input is read as a line.
func secret(q string, set bool, ask func(q, current string) string) string {
	if !interactive() {
		if set {
			return ask(q+" (enter keeps the one set)", "")
		}
		return ask(q, "")
	}
	if set {
		q += " (enter keeps the one set)"
	}
	fmt.Printf("%s: ", q)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func interactive() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// browse opens the chat in the person's browser; failing that, the address is
// in the log.
func browse(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
}
