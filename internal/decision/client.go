package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mvaldes14/nerv/internal/investigation"
)

type Client struct {
	baseURL string
	token   string
	name    string
	version string
	client  *http.Client
}

func NewClient(baseURL, token, name, version string, timeout time.Duration) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, name: name, version: version, client: &http.Client{Timeout: timeout}}
}

// alertDispositionRequest matches the alert-disposition/v1 contract currently
// deployed in Decision Gateway.
type alertDispositionRequest struct {
	AlertName             string `json:"alertName"`
	Summary               string `json:"summary"`
	Source                string `json:"source"`
	FiringForMinutes      int    `json:"firingForMinutes"`
	RepeatedCount         int    `json:"repeatedCount"`
	HasRunbook            bool   `json:"hasRunbook"`
	RecentRelatedIncident bool   `json:"recentRelatedIncident"`
}

type executeResponse struct {
	Results map[string]struct {
		Choice string `json:"choice"`
	} `json:"results"`
	Provider       string `json:"provider"`
	ReviewRequired bool   `json:"review_required"`
	RequestID      string `json:"request_id"`
}

func (c *Client) Evaluate(ctx context.Context, req investigation.Request, evidence []investigation.Evidence) (investigation.Decision, error) {
	if c.baseURL == "" || c.name == "" || c.version == "" {
		return investigation.Decision{}, fmt.Errorf("Decision Gateway URL, decision name, and version are required")
	}
	payload := alertDispositionRequest{
		AlertName:             truncate(firstNonEmpty(req.Labels["alertname"], req.Question), 200),
		Summary:               evidenceSummary(req.Question, evidence),
		Source:                decisionSource(req.Source),
		FiringForMinutes:      0,
		RepeatedCount:         1,
		HasRunbook:            req.Annotations["runbook_url"] != "" || req.Annotations["runbook"] != "",
		RecentRelatedIncident: false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return investigation.Decision{}, fmt.Errorf("encode decision request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/v1/decisions/%s/versions/%s:execute", c.baseURL, url.PathEscape(c.name), url.PathEscape(c.version))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return investigation.Decision{}, fmt.Errorf("create decision request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return investigation.Decision{}, fmt.Errorf("call Decision Gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return investigation.Decision{}, fmt.Errorf("Decision Gateway returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var result executeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return investigation.Decision{}, fmt.Errorf("decode Decision Gateway response: %w", err)
	}
	disposition := result.Results["disposition"].Choice
	urgency := result.Results["urgency"].Choice
	action, err := mapAction(disposition, urgency)
	if err != nil {
		return investigation.Decision{}, err
	}
	return investigation.Decision{
		Action:         action,
		Reason:         fmt.Sprintf("disposition=%s urgency=%s", disposition, urgency),
		ReviewRequired: result.ReviewRequired,
		Provider:       result.Provider,
		RequestID:      result.RequestID,
	}, nil
}

func mapAction(disposition, urgency string) (string, error) {
	switch disposition {
	case "noisy", "duplicate", "informational":
		return "suppress", nil
	case "actionable":
		switch urgency {
		case "immediate":
			return "escalate", nil
		case "soon":
			return "investigate", nil
		case "backlog":
			return "research", nil
		case "none":
			return "suppress", nil
		}
	}
	return "", fmt.Errorf("Decision Gateway returned unsupported disposition %q and urgency %q", disposition, urgency)
}

func decisionSource(source string) string {
	if strings.Contains(source, "alert") {
		return "signoz"
	}
	if strings.Contains(source, "kubernetes") {
		return "kubernetes"
	}
	return "other"
}

func evidenceSummary(question string, evidence []investigation.Evidence) string {
	var builder strings.Builder
	builder.WriteString(question)
	for _, item := range evidence {
		if builder.Len() >= 1900 {
			break
		}
		builder.WriteString("\n")
		builder.WriteString(item.Source)
		builder.WriteString(": ")
		builder.WriteString(item.Summary)
	}
	value := builder.String()
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "unknown"
}
