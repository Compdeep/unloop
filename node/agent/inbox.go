package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/envelope"
)

// heard is one message the agent's node accepted.
type heard struct {
	At     time.Time      `json:"at"`
	Topic  string         `json:"topic"`
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Colony string         `json:"colony"`
	Author string         `json:"author"`
	Body   map[string]any `json:"body,omitempty"`
}

// inbox keeps what the node hears, so the person can ask what is new. Kept
// in a file as well, one message a line, so a restart loses nothing; read
// back from the end, so it never grows past what a question needs.
type inbox struct {
	mu     sync.Mutex
	path   string
	recent []heard
}

const inboxKeeps = 500

func newInbox(dir string) *inbox {
	in := &inbox{path: filepath.Join(dir, "heard.jsonl")}
	if f, err := os.Open(in.path); err == nil {
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 1<<20), 8<<20)
		for s.Scan() {
			var h heard
			if json.Unmarshal(s.Bytes(), &h) == nil {
				in.keep(h)
			}
		}
		f.Close()
	}
	return in
}

func (in *inbox) keep(h heard) {
	in.recent = append(in.recent, h)
	if len(in.recent) > inboxKeeps {
		in.recent = in.recent[len(in.recent)-inboxKeeps:]
	}
}

// deliver is bus.Config.Deliver.
func (in *inbox) deliver(t bus.Topic, m *envelope.Message) {
	h := heard{At: time.Now().UTC(), Topic: string(t.Channel), ID: m.ID(), Type: m.Type, Colony: m.Colony, Author: m.Author, Body: m.Body}
	in.mu.Lock()
	defer in.mu.Unlock()
	// One message on two topics is heard twice; it is kept once.
	for _, r := range in.recent {
		if r.ID == h.ID {
			return
		}
	}
	in.keep(h)
	if f, err := os.OpenFile(in.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		json.NewEncoder(f).Encode(h)
		f.Close()
	}
}

// since is what was heard after a time, newest last, at most n.
func (in *inbox) since(t time.Time, n int) []heard {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []heard
	for _, h := range in.recent {
		if h.At.After(t) {
			out = append(out, h)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// note keeps something the agent itself concluded or noticed, once per id,
// beside what it heard.
func (in *inbox) note(id, about, text string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, r := range in.recent {
		if r.ID == id {
			return
		}
	}
	h := heard{At: time.Now().UTC(), ID: id, Type: "note", Body: map[string]any{"about": about, "text": text}}
	in.keep(h)
	if f, err := os.OpenFile(in.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		json.NewEncoder(f).Encode(h)
		f.Close()
	}
}
