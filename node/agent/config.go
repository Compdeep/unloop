// Package agent is unloop-agent (docs/PLATFORM.md section 7): a person's own
// agent, on their own machine. Kaiju is the engine, embedded through its
// public API and unchanged; what the agent can do in Unloop is the tools
// here, registered from outside it; the bus is node/bus, following the
// colonies the person chose; and the chat is kaiju's page, on loopback only.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Config is what `unloop-agent init` asks for, kept in the data directory.
type Config struct {
	// Model is the model the agent thinks with.
	Model struct {
		Provider string `json:"provider"` // openai, anthropic, openrouter
		Endpoint string `json:"endpoint,omitempty"`
		Key      string `json:"key"`
		Name     string `json:"name"`
	} `json:"model"`
	// RPC is the Solana cluster it reads and, later, sends to.
	RPC string `json:"rpc"`
	// Program is the Unloop program on that cluster.
	Program string `json:"program"`
	// Platform is the site it browses through: an index, never an authority.
	Platform string `json:"platform"`
	// Follow is the colonies whose topics the agent's node follows.
	Follow []string `json:"follow"`
	// Chat is where the chat is served. Loopback only.
	Chat string `json:"chat"`
	// Autonomy is how far the agent goes without the person approving each
	// step on its approvals page:
	//
	//	tell     nothing: every message it would sign and every transaction
	//	         waits for the person (the default)
	//	answer   it also posts to the thread of a claim it filed
	//	act      it also files, validates and collects, within DailyLimit
	Autonomy string `json:"autonomy"`
	// DailyLimit is the most, in whole protocol tokens, that `act` spends in
	// a day without asking. What the person approves by hand is theirs to judge.
	DailyLimit string `json:"daily_limit"`
}

// Autonomies, in order: each allows what the one before does, and more.
var Autonomies = []string{"tell", "answer", "act"}

// Defaults are a devnet agent browsing unloop.fun.
func Defaults() Config {
	c := Config{
		RPC:      "https://api.devnet.solana.com",
		Program:  "3bPsoRTKT1J9U6V6Zua42VKHVBZ9XAuBPJq3RFFF3Qdz",
		Platform: "https://unloop.fun",
		Chat:     "127.0.0.1:7777",
		Autonomy: "tell", DailyLimit: "0",
	}
	c.Model.Provider = "openai"
	return c
}

// Home is the data directory: $UNLOOP_AGENT_HOME, or ~/.unloop-agent.
func Home() (string, error) {
	if h := os.Getenv("UNLOOP_AGENT_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".unloop-agent"), nil
}

func configPath(dir string) string { return filepath.Join(dir, "config.json") }

// Load reads the configuration; ErrNotSetUp before `init` has run.
func Load(dir string) (Config, error) {
	c := Defaults()
	b, err := os.ReadFile(configPath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return c, ErrNotSetUp
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", configPath(dir), err)
	}
	return c, c.check()
}

// ErrNotSetUp is a data directory `init` has not written.
var ErrNotSetUp = errors.New("not set up yet: run `unloop-agent init`")

// Save writes it, readable by its owner alone: it holds a model key.
func (c Config) Save(dir string) error {
	if err := c.check(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath(dir), append(b, '\n'), 0o600)
}

// check refuses what would make the agent unsafe or unusable.
func (c Config) check() error {
	host, _, err := net.SplitHostPort(c.Chat)
	if err != nil {
		return fmt.Errorf("chat %q: %w", c.Chat, err)
	}
	// The chat has no sign-in: anybody who can reach it can talk to the agent,
	// and the agent holds a wallet. So nobody but this machine reaches it.
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("the chat is served on loopback only, not %q: it has no sign-in, and the agent holds a wallet", host)
	}
	if !slices.Contains(Autonomies, c.Autonomy) {
		return fmt.Errorf("autonomy %q is not one of %v", c.Autonomy, Autonomies)
	}
	if _, ok := new(big.Rat).SetString(c.DailyLimit); !ok || strings.HasPrefix(c.DailyLimit, "-") {
		return fmt.Errorf("daily_limit %q is not an amount of whole protocol tokens", c.DailyLimit)
	}
	return nil
}
