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
	QueueNotificationBatch(ctx context.Context, incidentID, sink string, debounce time.Duration) (batchID string, leader bool, err error)
	GetNotificationBatch(ctx context.Context, batchID string) (investigation.NotificationBatch, error)
	FailNotificationBatch(ctx context.Context, batchID string) error
	CompleteNotificationBatch(ctx context.Context, batchID, sink, externalID string) error
}

type Gotify struct {
	baseURL  string
	token    string
	client   *http.Client
	record   AttemptRecorder
	debounce time.Duration
}

func NewGotify(baseURL, token string, timeout, debounce time.Duration, recorder AttemptRecorder) *Gotify {
	return &Gotify{
		baseURL:  strings.TrimRight(baseURL, "/"),
		token:    token,
		client:   &http.Client{Timeout: timeout},
		record:   recorder,
		debounce: debounce,
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
	if finding.Source == investigation.SourceKubernetesDetector && g.debounce > 0 && g.record != nil {
		return g.sendDetectorBatch(ctx, finding)
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
	externalID, err := g.send(ctx, payload)
	if err != nil {
		g.recordAttempt(ctx, finding.ID, "", "failed", err.Error())
		return "", err
	}
	if err := g.recordAttempt(ctx, finding.ID, externalID, "sent", ""); err != nil {
		return "", err
	}
	return externalID, nil
}

func (g *Gotify) sendDetectorBatch(ctx context.Context, finding investigation.Finding) (string, error) {
	externalID, sent, err := g.record.SentNotification(ctx, finding.ID, "gotify")
	if err != nil {
		return "", err
	}
	if sent {
		return externalID, nil
	}
	batchID, leader, err := g.record.QueueNotificationBatch(ctx, finding.ID, "gotify", g.debounce)
	if err != nil {
		return "", err
	}
	if !leader {
		return "batched:" + batchID, nil
	}

	for {
		batch, err := g.record.GetNotificationBatch(ctx, batchID)
		if err != nil {
			return "", err
		}
		if batch.Sent {
			return batch.ExternalID, nil
		}
		if wait := time.Until(batch.DueAt); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
			continue
		}
		payload := formatBatch(batch)
		externalID, err := g.send(ctx, payload)
		if err != nil {
			_ = g.record.FailNotificationBatch(ctx, batchID)
			g.recordAttempt(ctx, finding.ID, "", "failed", err.Error())
			return "", err
		}
		if err := g.record.CompleteNotificationBatch(ctx, batchID, "gotify", externalID); err != nil {
			return "", err
		}
		return externalID, nil
	}
}

func (g *Gotify) send(ctx context.Context, payload gotifyRequest) (string, error) {
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
		return "", fmt.Errorf("send Gotify message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Gotify returned %s", resp.Status)
	}

	var result gotifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Gotify response: %w", err)
	}
	return fmt.Sprintf("%d", result.ID), nil
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

func formatBatch(batch investigation.NotificationBatch) gotifyRequest {
	highestAction := "investigate"
	lines := []string{fmt.Sprintf("**%d related Kubernetes incidents**", len(batch.Items)), ""}
	for i, item := range batch.Items {
		if priority(item.Action) > priority(highestAction) {
			highestAction = item.Action
		}
		if i >= 20 {
			continue
		}
		target := "unknown target"
		if item.Target != nil {
			target = fmt.Sprintf("%s/%s", item.Target.Namespace, item.Target.Name)
		}
		lines = append(lines, fmt.Sprintf("- `%s` — %s", target, item.Action))
	}
	if remaining := len(batch.Items) - 20; remaining > 0 {
		lines = append(lines, fmt.Sprintf("- …and %d more", remaining))
	}
	lines = append(lines, "", fmt.Sprintf("**Batch:** `%s`", batch.ID))
	return gotifyRequest{
		Title:    fmt.Sprintf("NERV: %d related incidents", len(batch.Items)),
		Message:  strings.Join(lines, "\n"),
		Priority: priority(highestAction),
		Extras: map[string]any{
			"client::display": map[string]any{"contentType": "text/markdown"},
			"nerv": map[string]any{
				"batch_id":       batch.ID,
				"incident_count": len(batch.Items),
				"action":         highestAction,
			},
		},
	}
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
