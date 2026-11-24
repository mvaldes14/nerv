package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
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
	"github.com/mvaldes14/nerv/internal/remediation"
)

type investigationService interface {
	Ready(context.Context) error
	Ask(context.Context, investigation.Request) (investigation.Finding, error)
	Accept(context.Context, investigation.Request) (investigation.Accepted, error)
	Get(context.Context, string) (investigation.Investigation, error)
	List(context.Context, investigation.IncidentFilter) (investigation.IncidentPage, error)
}

type remediationService interface {
	Create(context.Context, string, remediation.CreateRequest) (remediation.Proposal, error)
	List(context.Context, string) ([]remediation.Proposal, error)
	Get(context.Context, string) (remediation.Proposal, error)
	Approve(context.Context, string, remediation.DecisionRequest, string) (remediation.Proposal, error)
	Reject(context.Context, string, remediation.DecisionRequest, string) (remediation.Proposal, error)
	Submit(context.Context, string) (remediation.Proposal, error)
}

type Dependencies struct {
	Service     investigationService
	Remediation remediationService
	AuthToken   string
	AskTimeout  time.Duration
	Logger      *slog.Logger
}

type Handler struct {
	mux         *http.ServeMux
	service     investigationService
	remediation remediationService
	authToken   string
	logger      *slog.Logger
	askTimeout  time.Duration
}

func NewHandler(deps Dependencies) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	askTimeout := deps.AskTimeout
	if askTimeout <= 0 {
		askTimeout = 90 * time.Second
	}
	h := &Handler{mux: http.NewServeMux(), service: deps.Service, remediation: deps.Remediation, authToken: deps.AuthToken, logger: logger, askTimeout: askTimeout}
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
	h.mux.Handle("POST /investigations/{id}/remediation-proposals", h.requireAuth(http.HandlerFunc(h.createRemediationProposal)))
	h.mux.Handle("GET /investigations/{id}/remediation-proposals", h.requireAuth(http.HandlerFunc(h.listRemediationProposals)))
	h.mux.Handle("GET /remediation-proposals/{id}", h.requireAuth(http.HandlerFunc(h.getRemediationProposal)))
	h.mux.Handle("POST /remediation-proposals/{id}/approve", h.requireAuth(http.HandlerFunc(h.approveRemediationProposal)))
	h.mux.Handle("POST /remediation-proposals/{id}/reject", h.requireAuth(http.HandlerFunc(h.rejectRemediationProposal)))
	h.mux.Handle("POST /remediation-proposals/{id}/submit", h.requireAuth(http.HandlerFunc(h.submitRemediationProposal)))
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
	req.Source = investigation.SourceHTTPAsk
	if strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}
	ctx, cancel := contextWithDefaultTimeout(r, h.askTimeout)
	defer cancel()
	finding, err := h.service.Ask(ctx, req)
	if err != nil {
		h.logger.Warn("ask failed", "error", err)
		if writeAdmissionError(w, err) {
			return
		}
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
			if writeAdmissionError(w, err) {
				return
			}
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
	if limit < 0 || limit > investigation.MaxIncidentLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 0 and %d", investigation.MaxIncidentLimit))
		return
	}
	if offset < 0 || offset > investigation.MaxIncidentOffset {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("offset must be between 0 and %d", investigation.MaxIncidentOffset))
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

func (h *Handler) createRemediationProposal(w http.ResponseWriter, r *http.Request) {
	if h.remediation == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation service unavailable")
		return
	}
	defer r.Body.Close()
	var req remediation.CreateRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proposal, err := h.remediation.Create(r.Context(), r.PathValue("id"), req)
	if err != nil {
		h.writeRemediationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, proposal)
}

func (h *Handler) listRemediationProposals(w http.ResponseWriter, r *http.Request) {
	if h.remediation == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation service unavailable")
		return
	}
	proposals, err := h.remediation.List(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeRemediationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": proposals})
}

func (h *Handler) getRemediationProposal(w http.ResponseWriter, r *http.Request) {
	if h.remediation == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation service unavailable")
		return
	}
	proposal, err := h.remediation.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeRemediationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (h *Handler) approveRemediationProposal(w http.ResponseWriter, r *http.Request) {
	h.decideRemediationProposal(w, r, true)
}

func (h *Handler) rejectRemediationProposal(w http.ResponseWriter, r *http.Request) {
	h.decideRemediationProposal(w, r, false)
}

func (h *Handler) decideRemediationProposal(w http.ResponseWriter, r *http.Request, approve bool) {
	if h.remediation == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation service unavailable")
		return
	}
	defer r.Body.Close()
	var req remediation.DecisionRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var (
		proposal remediation.Proposal
		err      error
	)
	if approve {
		proposal, err = h.remediation.Approve(r.Context(), r.PathValue("id"), req, "shared-bearer")
	} else {
		proposal, err = h.remediation.Reject(r.Context(), r.PathValue("id"), req, "shared-bearer")
	}
	if err != nil {
		h.writeRemediationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (h *Handler) submitRemediationProposal(w http.ResponseWriter, r *http.Request) {
	if h.remediation == nil {
		writeError(w, http.StatusServiceUnavailable, "remediation service unavailable")
		return
	}
	proposal, err := h.remediation.Submit(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeRemediationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (h *Handler) writeRemediationError(w http.ResponseWriter, err error) {
	var validation *remediation.ValidationError
	switch {
	case errors.As(err, &validation):
		writeError(w, http.StatusBadRequest, validation.Error())
	case errors.Is(err, remediation.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, remediation.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("remediation request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "remediation request failed")
	}
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

func writeAdmissionError(w http.ResponseWriter, err error) bool {
	var denied *investigation.AdmissionError
	if !errors.As(err, &denied) {
		return false
	}
	retrySeconds := max(int64((denied.RetryAfter+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
	writeError(w, http.StatusTooManyRequests, denied.Reason)
	return true
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
		Source:         investigation.SourceHTTPAlert,
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

func decodeStrictJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid JSON body: multiple JSON values are not allowed")
		}
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
