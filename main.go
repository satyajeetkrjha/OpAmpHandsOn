package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	opampserver "github.com/open-telemetry/opamp-go/server"
	stypes "github.com/open-telemetry/opamp-go/server/types"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type AgentSnapshot struct {
	InstanceUID string    `json:"instance_uid"`
	LastSeen    time.Time `json:"last_seen"`

	AgentDescriptionJSON string `json:"agent_description_json,omitempty"`
	HealthJSON           string `json:"health_json,omitempty"`
	EffectiveConfigB64   string `json:"effective_config_b64,omitempty"`
	AvailableComponents  string `json:"available_components_json,omitempty"`
}

type Store struct {
	mu     sync.RWMutex
	agents map[string]*AgentSnapshot
}

func NewStore() *Store {
	return &Store{
		agents: make(map[string]*AgentSnapshot),
	}
}

func (s *Store) Upsert(uid string, fn func(a *AgentSnapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.agents[uid]
	if !ok {
		a = &AgentSnapshot{InstanceUID: uid}
		s.agents[uid] = a
	}
	fn(a)
}

func (s *Store) List() []*AgentSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*AgentSnapshot, 0, len(s.agents))
	for _, a := range s.agents {
		c := *a
		out = append(out, &c)
	}
	return out
}

func (s *Store) Get(uid string) (*AgentSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	a, ok := s.agents[uid]
	if !ok {
		return nil, false
	}
	c := *a
	return &c, true
}

func main() {
	store := NewStore()

	// ---------------- OpAMP callbacks ----------------

	connCallbacks := stypes.ConnectionCallbacks{
		OnConnected: func(ctx context.Context, conn stypes.Connection) {
			// HTTP transport = short lived
		},

		OnMessage: func(ctx context.Context, conn stypes.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
			rawUID := msg.GetInstanceUid()
			if len(rawUID) == 0 {
				return &protobufs.ServerToAgent{}
			}

			uid := hex.EncodeToString(rawUID)
			now := time.Now()

			store.Upsert(uid, func(a *AgentSnapshot) {
				a.LastSeen = now

				if ad := msg.GetAgentDescription(); ad != nil {
					a.AgentDescriptionJSON = mustJSON(ad)
				}

				if h := msg.GetHealth(); h != nil {
					a.HealthJSON = mustJSON(h)
				}

				if ec := msg.GetEffectiveConfig(); ec != nil {
					j := mustJSON(ec)
					if j != "" {
						a.EffectiveConfigB64 = base64.StdEncoding.EncodeToString([]byte(j))
					}
				}

				if ac := msg.GetAvailableComponents(); ac != nil {
					a.AvailableComponents = mustJSON(ac)
				}
			})

			// MVP: no remote config pushed yet
			return &protobufs.ServerToAgent{}
		},

		OnConnectionClose: func(conn stypes.Connection) {},
	}

	connCallbacks.SetDefaults()

	callbacks := stypes.Callbacks{
		OnConnecting: func(r *http.Request) stypes.ConnectionResponse {
			return stypes.ConnectionResponse{
				Accept:              true,
				ConnectionCallbacks: connCallbacks,
			}
		},
	}

	callbacks.SetDefaults()

	opSrv := opampserver.New(nil)

	mux := http.NewServeMux()

	opHandler, connContext, err := opSrv.Attach(opampserver.Settings{
		Callbacks: callbacks,
	})
	if err != nil {
		log.Fatalf("failed to attach OpAMP server: %v", err)
	}

	// -------- OpAMP endpoint --------
	mux.HandleFunc("/v1/opamp", opHandler)

	// -------- REST API --------

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, store.List())
	})

	mux.HandleFunc("/api/agents/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		uid := r.URL.Path[len("/api/agents/"):]
		if uid == "" {
			http.Error(w, "missing instance_uid", http.StatusBadRequest)
			return
		}

		a, ok := store.Get(uid)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		writeJSON(w, a)
	})

	// -------- HTTPS server --------

	srv := &http.Server{
		Addr:        ":4320",
		Handler:     mux,
		ConnContext: connContext,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	log.Println("OpAMP MVP listening on https://0.0.0.0:4320")
	log.Fatal(srv.ListenAndServeTLS("server.crt", "server.key"))
}

func mustJSON(m proto.Message) string {
	b, err := protojson.MarshalOptions{
		Multiline:       false,
		EmitUnpopulated: false,
	}.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
