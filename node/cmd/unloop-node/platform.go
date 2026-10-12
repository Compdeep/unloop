package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Compdeep/unloop/node/bus"
	"github.com/Compdeep/unloop/node/envelope"
	"github.com/libp2p/go-libp2p/core/peer"
)

// platform is the server, over its loopback-only listener: where accepted
// messages are kept, and the bodies this node serves.
type platform struct {
	url  string
	http *http.Client
	node *bus.Node
	// At most this many messages on their way to the server at once; more
	// wait, rather than the node holding an unbounded backlog in goroutines.
	slots chan struct{}
}

func newPlatform(url string) *platform {
	return &platform{url: url, http: &http.Client{Timeout: 30 * time.Second}, slots: make(chan struct{}, 64)}
}

// delivered is what the server is posted for each message.
type delivered struct {
	Topic    bus.Topic       `json:"topic"`
	ID       string          `json:"id"`
	Envelope json.RawMessage `json:"envelope"`
	// Body is a body_ref's body, when this node could fetch it from the
	// peer that published the message.
	Body []byte `json:"body,omitempty"`
}

// The server is told again when it answers that it has not caught up — its
// index behind the chain this node checked against — and not otherwise.
var retries = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

func (p *platform) deliver(ctx context.Context) func(bus.Topic, *envelope.Message) {
	return func(t bus.Topic, m *envelope.Message) {
		p.slots <- struct{}{}
		go func() {
			defer func() { <-p.slots }()
			raw, err := json.Marshal(m.Raw())
			if err != nil {
				log.Printf("%s: %v", m.ID(), err)
				return
			}
			d := delivered{Topic: t, ID: m.ID(), Envelope: raw, Body: p.fetchRef(ctx, m)}
			for i := 0; ; i++ {
				status, err := p.post(ctx, "/bus/message", d)
				if err == nil && status < 300 {
					return
				}
				if status != http.StatusConflict || i == len(retries) {
					log.Printf("%s %s: the server kept nothing (%d %v)", m.Type, m.ID(), status, err)
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(retries[i]):
				}
			}
		}()
	}
}

// fetchRef fetches a body carried by reference from the peer that published
// it; nil when there is none to fetch, or it could not be had.
func (p *platform) fetchRef(ctx context.Context, m *envelope.Message) []byte {
	ref, _ := m.Raw()["body_ref"].(map[string]any)
	hash, _ := ref["hash"].(string)
	if hash == "" {
		return nil
	}
	from, err := peer.Decode(m.Peer)
	if err != nil || from == p.node.ID() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	body, err := p.node.Fetch(ctx, from, hash)
	if err != nil {
		log.Printf("%s: body %s from %s: %v", m.ID(), hash, from, err)
		return nil
	}
	return body
}

func (p *platform) post(ctx context.Context, path string, v any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := p.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return res.StatusCode, fmt.Errorf("%s", bytes.TrimSpace(msg))
	}
	return res.StatusCode, nil
}

// Body is bus.Bodies: what the server's archive holds under a hash.
func (p *platform) Body(hash string) ([]byte, bool) {
	res, err := p.http.Get(p.url + "/bus/bodies/" + hash)
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, bus.MaxBody+1))
	if err != nil || len(b) > bus.MaxBody {
		return nil, false
	}
	return b, true
}
