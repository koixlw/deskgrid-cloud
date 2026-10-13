package internal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// PeerSync replicates worker state between gateway instances so a second
// gateway can take over when the first one fails. Set GATEWAY_PEERS to a
// comma-separated list of peer URLs (e.g. "http://gw-b:13000,http://gw-c:13000").
//
// On worker state changes (register/heartbeat update/offline/removed) we
// POST the change to every peer. Peers replay on their own store.
//
// Task state is ephemeral and NOT replicated — when a gateway dies, any
// in-flight task is lost. Workers re-register on failover and the new
// gateway starts with an empty task list. This is acceptable for a demo
// and matches the typical "stateless gateway" HA design.
type PeerSync struct {
	mu      sync.Mutex
	peers   []string
	client  *http.Client
	enabled bool
}

func NewPeerSync(peersCSV string) *PeerSync {
	if peersCSV == "" {
		return &PeerSync{}
	}
	peers := make([]string, 0)
	for _, p := range bytes.Split([]byte(peersCSV), []byte(",")) {
		s := string(bytes.TrimSpace(p))
		if s != "" {
			peers = append(peers, s)
		}
	}
	return &PeerSync{
		peers:   peers,
		client:  &http.Client{Timeout: 2 * time.Second},
		enabled: len(peers) > 0,
	}
}

// Enabled reports whether peer replication is configured.
func (p *PeerSync) Enabled() bool { return p != nil && p.enabled }

// BroadcastEvent sends an event payload to every configured peer. Best-effort:
// failures are logged and dropped (peer may be down).
func (p *PeerSync) BroadcastEvent(event string, payload interface{}) {
	if !p.Enabled() {
		return
	}
	body, err := json.Marshal(map[string]interface{}{
		"event":   event,
		"payload": payload,
		"ts":      time.Now().Unix(),
	})
	if err != nil {
		Warn("peer sync: marshal failed: %v", err)
		return
	}
	for _, peer := range p.peers {
		go p.sendTo(peer, body)
	}
}

func (p *PeerSync) sendTo(peer string, body []byte) {
	url := peer + "/api/peer-replicate"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		// peer may be down — silent retry on next event
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		Warn("peer sync: %s returned %d", url, resp.StatusCode)
	}
}