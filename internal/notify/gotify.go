package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type AttemptRecorder interface {
	SentNotification(ctx context.Context, incidentID, sink string) (externalID string, sent bool, err error)
	RecordNotification(ctx context.Context, incidentID, sink, externalID, status, errorMessage string) error
}

type Gotify struct {
	baseURL string
	token   string
	client  *http.Client
	record  AttemptRecorder
}

func NewGotify(baseURL, token string, timeout time.Duration, recorder AttemptRecorder) *Gotify {
	return &Gotify{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
		record:  recorder,
	}
}

type gotifyRequest struct {
	Title    string         `json:"title"`
	Message  string         `json:"message"`
	Priority int            `json:"priority"`
	Extras   map[string]any `json:"extras,omitempty"`
}

type gotifyResponse struct {
	ID int64 `json:"id"`
}

func (g *Gotify) Send(ctx context.Context, finding investigation.Finding) (string, error) {
	if g.baseURL == "" || g.token == "" {
		return "", fmt.Errorf("Gotify URL and token are required")
	}
	if g.record != nil {
		externalID, sent, err := g.record.SentNotification(ctx, finding.ID, "gotify")
		if err != nil {
			return "", err
		}
		if sent {
			return externalID, nil
		}
	}
	payload := gotifyRequest{
		Title:    fmt.Sprintf("NERV: %s", actionTitle(finding.Decision.Action)),
		Message:  formatMessage(finding),
		Priority: priority(finding.Decision.Action),
		Extras: map[string]any{
			"client::display": map[string]any{"contentType": "text/markdown"},
			"nerv": map[string]any{
				"incident_id": finding.ID,
				"workflow_id": finding.WorkflowID,
				"action":      finding.Decision.Action,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode Gotify message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/message", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create Gotify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gotify-Key", g.token)

	resp, err := g.client.Do(req)
	if err != nil {
		g.recordAttempt(ctx, finding.ID, "", "failed", err.Error())
		return "", fmt.Errorf("send Gotify message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("Gotify returned %s", resp.Status)
		g.recordAttempt(ctx, finding.ID, "", "failed", err.Error())
		return "", err
	}

	var result gotifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		g.recordAttempt(ctx, finding.ID, "", "failed", err.Error())
		return "", fmt.Errorf("decode Gotify response: %w", err)
	}
	externalID := fmt.Sprintf("%d", result.ID)
	if err := g.recordAttempt(ctx, finding.ID, externalID, "sent", ""); err != nil {
		return "", err
	}
	return externalID, nil
}

func (g *Gotify) recordAttempt(ctx context.Context, incidentID, externalID, status, errorMessage string) error {
	if g.record == nil {
		return nil
	}
	return g.record.RecordNotification(ctx, incidentID, "gotify", externalID, status, errorMessage)
}

func formatMessage(f investigation.Finding) string {
	return fmt.Sprintf("**Incident:** `%s`\n\n**Question:** %s\n\n**Decision:** `%s`\n\n%s", f.ID, f.Question, f.Decision.Action, f.Answer)
}

func actionTitle(action string) string {
	switch action {
	case "escalate":
		return "escalation required"
	case "suppress":
		return "incident suppressed"
	case "research":
		return "research completed"
	default:
		return "investigation completed"
	}
}

func priority(action string) int {
	if action == "escalate" {
		return 8
	}
	return 5
}
