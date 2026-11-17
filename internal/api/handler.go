package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mvaldes14/nerv/internal/detection"
	"github.com/mvaldes14/nerv/internal/investigation"
)

type investigationService interface {
	Ready(context.Context) error
	Ask(context.Context, investigation.Request) (investigation.Finding, error)
	Accept(context.Context, investigation.Request) (investigation.Accepted, error)
	Get(context.Context, string) (investigation.Investigation, error)
	List(context.Context, investigation.IncidentFilter) (investigation.IncidentPage, error)
}

type Dependencies struct {
	Service   investigationService
	AuthToken string
	Logger    *slog.Logger
}

type Handler struct {
	mux       *http.ServeMux
	service   investigationService
	authToken string
	logger    *slog.Logger
}

func NewHandler(deps Dependencies) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{mux: http.NewServeMux(), service: deps.Service, authToken: deps.AuthToken, logger: logger}
	h.routes()
	return otelhttp.NewHandler(h, "nerv.http")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) routes() {
	h.mux.HandleFunc("GET /healthz", h.health)
	h.mux.HandleFunc("GET /readyz", h.ready)
	h.mux.HandleFunc("GET /ui", h.redirectUI)
	h.mux.Handle("GET /ui/", h.uiHandler())
	h.mux.Handle("POST /ask", h.requireAuth(http.HandlerFunc(h.ask)))
	h.mux.Handle("POST /alert", h.requireAuth(http.HandlerFunc(h.alert)))
	h.mux.Handle("GET /investigations", h.requireAuth(http.HandlerFunc(h.listInvestigations)))
	h.mux.Handle("GET /investigations/{id}", h.requireAuth(http.HandlerFunc(h.getInvestigation)))
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.service.Ready(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req investigation.Request
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Source = "http.ask"
	if strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}
	ctx, cancel := contextWithDefaultTimeout(r, 60*time.Second)
	defer cancel()
	finding, err := h.service.Ask(ctx, req)
	if err != nil {
		h.logger.Warn("ask failed", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, finding)
}

func (h *Handler) alert(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var payload alertmanagerWebhook
	if err := decodeJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(payload.Alerts) == 0 {
		writeError(w, http.StatusBadRequest, "alert webhook contains no alerts")
		return
	}

	accepted := make([]investigation.Accepted, 0, len(payload.Alerts))
	for _, alert := range payload.Alerts {
		req := alertRequest(payload.Receiver, alert)
		item, err := h.service.Accept(r.Context(), req)
		if err != nil {
			h.logger.Error("alert acceptance failed", "error", err, "fingerprint", alert.Fingerprint)
			writeError(w, http.StatusServiceUnavailable, "failed to durably accept alert")
			return
		}
		accepted = append(accepted, item)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "receiver": payload.Receiver, "investigations": accepted})
}

func (h *Handler) listInvestigations(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := queryInt(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := h.service.List(r.Context(), investigation.IncidentFilter{
		Limit:     limit,
		Offset:    offset,
		Source:    r.URL.Query().Get("source"),
		Status:    r.URL.Query().Get("status"),
		Namespace: r.URL.Query().Get("namespace"),
		Workload:  r.URL.Query().Get("workload"),
	})
	if err != nil {
		h.logger.Warn("list investigations failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list investigations")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) getInvestigation(w http.ResponseWriter, r *http.Request) {
	detail, err := h.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"incident":      detail.Incident,
		"status":        detail.Incident.Status,
		"finding":       detail.Finding,
		"evidence":      detail.Evidence,
		"notifications": detail.Notifications,
	})
}

func queryInt(r *http.Request, name string) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func (h *Handler) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.authToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.authToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func alertRequest(receiver string, alert alertmanagerAlert) investigation.Request {
	name := firstNonEmpty(alert.Labels["pod"], alert.Labels["pod_name"], alert.Labels["deployment"], alert.Labels["statefulset"], alert.Labels["daemonset"], alert.Labels["node"])
	kind := "pod"
	switch name {
	case alert.Labels["deployment"]:
		kind = "deployment"
	case alert.Labels["statefulset"]:
		kind = "statefulset"
	case alert.Labels["daemonset"]:
		kind = "daemonset"
	case alert.Labels["node"]:
		kind = "node"
	}
	question := firstNonEmpty(alert.Annotations["description"], alert.Annotations["summary"], alert.Labels["alertname"])
	if question == "" {
		question = fmt.Sprintf("Investigate firing alert from receiver %s", receiver)
	}
	req := investigation.Request{
		IdempotencyKey: alert.Fingerprint,
		Question:       question,
		Source:         "http.alert",
		Labels:         alert.Labels,
		Annotations:    alert.Annotations,
	}
	if name != "" {
		req.Target = &investigation.Target{Namespace: alert.Labels["namespace"], Kind: kind, Name: name, Container: alert.Labels["container"]}
		if failure := alertFailureIdentity(kind, alert); failure != "" {
			req.IdempotencyKey = investigation.CorrelationKey(req.Target, failure)
		}
	}
	return req
}

func alertFailureIdentity(kind string, alert alertmanagerAlert) string {
	if failure := strings.TrimSpace(alert.Labels["nerv_failure"]); failure != "" {
		return failure
	}
	text := strings.ToLower(strings.Join([]string{
		alert.Labels["alertname"],
		alert.Labels["reason"],
		alert.Annotations["summary"],
		alert.Annotations["description"],
	}, " "))
	switch {
	case strings.Contains(text, "crashloopbackoff") || strings.Contains(text, "crash loop"):
		return detection.FailureCrashLoop
	case strings.Contains(text, "imagepullbackoff") || strings.Contains(text, "errimagepull") || strings.Contains(text, "image pull"):
		return detection.FailureImagePull
	case strings.Contains(text, "unschedulable"):
		return detection.FailureUnschedulable
	case kind == "pod" && strings.Contains(text, "restart"):
		return detection.FailureContainerRestarts
	case kind == "deployment" && (strings.Contains(text, "rollout") || strings.Contains(text, "progressdeadline")):
		return detection.FailureDeploymentRollout
	case kind == "deployment" && strings.Contains(text, "unavailable"):
		return detection.FailureDeploymentUnavailable
	case kind == "statefulset" && strings.Contains(text, "unavailable"):
		return detection.FailureStatefulUnavailable
	case kind == "daemonset" && strings.Contains(text, "unavailable"):
		return detection.FailureDaemonUnavailable
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
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

func contextWithDefaultTimeout(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), timeout)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
