package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The body protocol: a body_ref's body, fetched point to point by its hash
// from a node that holds it (docs/PLATFORM.md section 6).
//
//	request    the 32-byte SHA-256, then the write side closed
//	response   one byte, 0 held or 1 not held; if held, the body to EOF
//
// The reader checks the hash of whatever comes back; a server is not trusted
// to hold what it says.
const BodyProtocol = "/unloop/body/1"

// MaxBody is the most a body_ref may name (common.schema.json, bodyRef.size).
const MaxBody = 8 << 20

const (
	held    byte = 0
	notHeld byte = 1
)

// How long one exchange may take when the caller sets no deadline.
const bodyTimeout = time.Minute

var (
	ErrNotHeld   = errors.New("the peer does not hold that body")
	ErrWrongBody = errors.New("the body does not match its hash")
	ErrTooLarge  = errors.New("the body is over the limit")
)

// Bodies is what a node serves by hash: lowercase hex SHA-256 in, the body
// out, or false when it holds none.
type Bodies interface {
	Body(hash string) ([]byte, bool)
}

// Fetch asks a peer for a body by its hash and returns it once it matches.
func (n *Node) Fetch(ctx context.Context, from peer.ID, hash string) ([]byte, error) {
	want, err := hex.DecodeString(hash)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("%q is not a SHA-256", hash)
	}
	s, err := n.host.NewStream(ctx, from, BodyProtocol)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(bodyTimeout)
	}
	s.SetDeadline(deadline)

	if _, err := s.Write(want); err != nil {
		s.Reset()
		return nil, err
	}
	s.CloseWrite()
	var status [1]byte
	if _, err := io.ReadFull(s, status[:]); err != nil {
		return nil, err
	}
	if status[0] != held {
		return nil, ErrNotHeld
	}
	body, err := io.ReadAll(io.LimitReader(s, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBody {
		s.Reset()
		return nil, ErrTooLarge
	}
	if got := sha256.Sum256(body); hex.EncodeToString(got[:]) != hash {
		return nil, ErrWrongBody
	}
	return body, nil
}

// serveBody answers one request for a body.
func (n *Node) serveBody(s network.Stream) {
	defer s.Close()
	s.SetDeadline(time.Now().Add(bodyTimeout))
	var h [sha256.Size]byte
	if _, err := io.ReadFull(s, h[:]); err != nil {
		s.Reset()
		return
	}
	var body []byte
	ok := false
	if n.cfg.Bodies != nil {
		body, ok = n.cfg.Bodies.Body(hex.EncodeToString(h[:]))
	}
	if !ok || len(body) > MaxBody {
		s.Write([]byte{notHeld})
		return
	}
	if _, err := s.Write([]byte{held}); err != nil {
		s.Reset()
		return
	}
	s.Write(body)
}
