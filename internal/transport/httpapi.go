package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"pregravity/internal/assistant"
	"pregravity/internal/config"
	"pregravity/internal/store"
)

// HTTPServer exposes an optional loopback-only JSON control API protected by a bearer token.
type HTTPServer struct {
	addr  string
	token string
	cfg   *config.Config
	st    *store.Store
	asst  *assistant.Assistant
	srv   *http.Server
}

// NewHTTPServer builds a hardened loopback HTTPServer.
func NewHTTPServer(addr, token string, cfg *config.Config, st *store.Store, asst *assistant.Assistant) (*HTTPServer, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("PREGRAVITY_HTTP_TOKEN must be set when http.enabled is true")
	}
	h := &HTTPServer{
		addr:  addr,
		token: token,
		cfg:   cfg,
		st:    st,
		asst:  asst,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.handleHealth)
	mux.HandleFunc("GET /tasks", h.requireAuth(h.handleListTasks))
	mux.HandleFunc("GET /handoffs", h.requireAuth(h.handleListHandoffs))
	mux.HandleFunc("GET /quota", h.requireAuth(h.handleQuota))
	mux.HandleFunc("POST /chat", h.requireAuth(h.handleChat))

	h.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return h, nil
}

// Handler returns the configured http.Handler (useful for httptest).
func (h *HTTPServer) Handler() http.Handler { return h.srv.Handler }

// Run starts listening and shuts down cleanly when ctx is canceled.
func (h *HTTPServer) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := h.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

func (h *HTTPServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(h.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (h *HTTPServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *HTTPServer) handleListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	tasks, err := h.st.ListTasks(status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (h *HTTPServer) handleListHandoffs(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	hs, err := h.st.ListHandoffs(status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handoffs": hs})
}

func (h *HTTPServer) handleQuota(w http.ResponseWriter, _ *http.Request) {
	model := ""
	limit := 0
	if h.cfg != nil {
		model = h.cfg.Model
		limit = h.cfg.Quota.LimitForModel(model)
	}
	summary, err := h.st.GetUsageSummary(time.Now(), model, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (h *HTTPServer) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
		Message   string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if body.SessionID == "" {
		body.SessionID = "http-default"
	}
	reply, err := h.asst.HandleMessage(r.Context(), body.SessionID, body.Message)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reply":      reply.Text,
		"tool_calls": reply.ToolCalls,
		"handoffs":   reply.Handoffs,
		"tokens":     reply.Tokens,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
