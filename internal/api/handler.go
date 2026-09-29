package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type Dependencies struct {
	Service   *investigation.Service
	AuthToken string
	Logger    *slog.Logger
}

type Handler struct {
	mux       *http.ServeMux
	service   *investigation.Service
	authToken string
	logger    *slog.Logger
}

func NewHandler(deps Dependencies) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		mux:       http.NewServeMux(),
		service:   deps.Service,
		authToken: deps.AuthToken,
		logger:    logger,
	}
	h.routes()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.Handle("POST /ask", h.requireAuth(http.HandlerFunc(h.ask)))
	h.mux.Handle("POST /alert", h.requireAuth(http.HandlerFunc(h.alert)))
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	var req investigation.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Source = "http.ask"

	ctx, cancel := contextWithDefaultTimeout(r, 30*time.Second)
	defer cancel()

	finding, err := h.service.Ask(ctx, req)
	if err != nil {
		h.logger.Warn("ask failed", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, finding)
}

func (h *Handler) alert(w http.ResponseWriter, r *http.Request) {
	// v1 will validate Alertmanager payloads and durably accept via DBOS before
	// returning 202. This scaffold only validates the webhook shape; it does not
	// claim durable acceptance.
	defer r.Body.Close()

	var payload alertmanagerWebhook
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(payload.Alerts) == 0 {
		writeError(w, http.StatusBadRequest, "alert webhook contains no alerts")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "validated",
		"durable":  false,
		"receiver": payload.Receiver,
		"alerts":   len(payload.Alerts),
	})
}

type alertmanagerWebhook struct {
	Receiver string              `json:"receiver"`
	Status   string              `json:"status"`
	Alerts   []alertmanagerAlert `json:"alerts"`
}

type alertmanagerAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

func (h *Handler) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.authToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != h.authToken {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func contextWithDefaultTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
