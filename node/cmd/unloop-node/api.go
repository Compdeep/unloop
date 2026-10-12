package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/Compdeep/unloop/node/bus"
	"github.com/libp2p/go-libp2p/core/peer"
)

// serveAPI is the node's own HTTP, for the server beside it and bound to
// loopback:
//
//	GET  /info     the peer id, the addresses to dial it at, the cluster
//	POST /publish  a signed envelope, judged as any arriving message is and
//	               then published on every topic its type travels on:
//	               200 {id}, or 400 {error, blamed} — blamed when it is the
//	               sender's fault, where the other refusals may pass later
func serveAPI(addr string, node *bus.Node, cluster string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		addrs, _ := peer.AddrInfoToP2pAddrs(&peer.AddrInfo{ID: node.ID(), Addrs: node.Info().Addrs})
		out := make([]string, len(addrs))
		for i, a := range addrs {
			out[i] = a.String()
		}
		reply(w, http.StatusOK, map[string]any{"peer": node.ID().String(), "addrs": out, "cluster": cluster})
	})
	mux.HandleFunc("POST /publish", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			reply(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "the message is over 1 MB", "blamed": true})
			return
		}
		id, err := node.Send(r.Context(), data)
		if err != nil {
			reply(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "blamed": bus.Blamed(err)})
			return
		}
		reply(w, http.StatusOK, map[string]any{"id": id})
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("api %s: %v", addr, err)
		}
	}()
	return srv
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
