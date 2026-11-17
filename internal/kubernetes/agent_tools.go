package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/mvaldes14/nerv/internal/investigation"
	"github.com/mvaldes14/nerv/internal/telemetry"
)

const maxDiscoveryItems = 200

func objectSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func (c *Client) Definitions() []investigation.ToolDefinition {
	maxBytes := c.maxToolBytes
	if maxBytes <= 0 {
		maxBytes = maxEvidenceBytes
	}
	maxTail := c.maxLogTailLines
	if maxTail <= 0 {
		maxTail = 1000
	}
	stringField := func(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
	maxBytesField := map[string]any{"type": "integer", "minimum": 256, "maximum": maxBytes}
	return []investigation.ToolDefinition{
		{Type: "function", Function: investigation.ToolFunction{Name: "list_workloads", Description: "List bounded Pod, Deployment, StatefulSet, and DaemonSet summaries. Empty namespace searches all namespaces. Use name_contains for cluster-wide product or workload discovery.", Strict: true, Parameters: objectSchema(map[string]any{
			"namespace": stringField(253), "kind": map[string]any{"type": "string", "enum": []string{"all", "pod", "deployment", "statefulset", "daemonset"}}, "label_selector": stringField(1024), "name_contains": stringField(253), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": maxDiscoveryItems}, "max_bytes": maxBytesField,
		}, "namespace", "kind", "label_selector", "name_contains", "limit", "max_bytes")}},
		{Type: "function", Function: investigation.ToolFunction{Name: "describe_resource", Description: "Read a bounded status summary for an approved Kubernetes resource kind.", Strict: true, Parameters: objectSchema(map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"Pod", "Deployment", "StatefulSet", "DaemonSet", "Node"}}, "namespace": stringField(253), "name": stringField(253), "max_bytes": maxBytesField,
		}, "kind", "namespace", "name", "max_bytes")}},
		{Type: "function", Function: investigation.ToolFunction{Name: "list_events", Description: "List Kubernetes events attached to an approved resource.", Strict: true, Parameters: objectSchema(map[string]any{
			"kind": map[string]any{"type": "string", "enum": []string{"Pod", "Deployment", "StatefulSet", "DaemonSet", "Node"}}, "namespace": stringField(253), "name": stringField(253), "max_bytes": maxBytesField,
		}, "kind", "namespace", "name", "max_bytes")}},
		{Type: "function", Function: investigation.ToolFunction{Name: "get_pod_logs", Description: "Read bounded current pod logs. This does not execute commands in the pod.", Strict: true, Parameters: logSchema(stringField, maxBytesField, maxTail)}},
		{Type: "function", Function: investigation.ToolFunction{Name: "get_previous_pod_logs", Description: "Read bounded previous-container pod logs. This does not execute commands in the pod.", Strict: true, Parameters: logSchema(stringField, maxBytesField, maxTail)}},
		{Type: "function", Function: investigation.ToolFunction{Name: "get_owner_chain", Description: "Read the Pod to ReplicaSet to workload owner chain.", Strict: true, Parameters: objectSchema(map[string]any{"namespace": stringField(253), "name": stringField(253), "max_bytes": maxBytesField}, "namespace", "name", "max_bytes")}},
		{Type: "function", Function: investigation.ToolFunction{Name: "get_node", Description: "Read a bounded node status summary by exact name.", Strict: true, Parameters: objectSchema(map[string]any{"name": stringField(253), "max_bytes": maxBytesField}, "name", "max_bytes")}},
	}
}

func logSchema(stringField func(int) map[string]any, maxBytesField map[string]any, maxTail int64) map[string]any {
	return objectSchema(map[string]any{
		"namespace": stringField(253), "name": stringField(253), "container": stringField(253), "tail_lines": map[string]any{"type": "integer", "minimum": 1, "maximum": maxTail}, "max_bytes": maxBytesField,
	}, "namespace", "name", "container", "tail_lines", "max_bytes")
}

type listArgs struct {
	Namespace     string `json:"namespace"`
	Kind          string `json:"kind"`
	LabelSelector string `json:"label_selector"`
	NameContains  string `json:"name_contains"`
	Limit         int    `json:"limit"`
	MaxBytes      int    `json:"max_bytes"`
}
type resourceArgs struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	MaxBytes  int    `json:"max_bytes"`
}
type logArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container"`
	TailLines int64  `json:"tail_lines"`
	MaxBytes  int    `json:"max_bytes"`
}
type podArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	MaxBytes  int    `json:"max_bytes"`
}
type nodeArgs struct {
	Name     string `json:"name"`
	MaxBytes int    `json:"max_bytes"`
}

func (c *Client) Execute(ctx context.Context, call investigation.ToolCall) investigation.ToolResult {
	name := strings.TrimSpace(call.Function.Name)
	ctx, span := telemetry.StartTool(ctx, name, "function", call.ID)
	result := investigation.ToolResult{CallID: call.ID, Name: name}
	content, err := c.execute(ctx, name, call.Function.Arguments)
	span.End(err, "")
	if err != nil {
		result.Error = err.Error()
		result.Content = c.errorOutput(err)
		return result
	}
	result.Content = content
	return result
}

func (c *Client) execute(ctx context.Context, name, raw string) (string, error) {
	switch name {
	case "list_workloads":
		var args listArgs
		if err := decodeStrict(raw, &args); err != nil {
			return "", err
		}
		if err := c.validateMaxBytes(args.MaxBytes); err != nil {
			return "", err
		}
		if args.Namespace != "" {
			if err := validateNamespace(args.Namespace); err != nil {
				return "", err
			}
		}
		switch args.Kind {
		case "all", "pod", "deployment", "statefulset", "daemonset":
		default:
			return "", fmt.Errorf("unsupported workload kind %q", args.Kind)
		}
		if args.Limit < 1 || args.Limit > maxDiscoveryItems {
			return "", fmt.Errorf("limit must be between 1 and %d", maxDiscoveryItems)
		}
		if _, err := labels.Parse(args.LabelSelector); err != nil {
			return "", fmt.Errorf("invalid label_selector: %w", err)
		}
		args.NameContains = strings.ToLower(strings.TrimSpace(args.NameContains))
		if len(args.NameContains) > 253 {
			return "", fmt.Errorf("name_contains exceeds 253 bytes")
		}
		value, err := c.listWorkloads(ctx, args)
		if err != nil {
			return "", err
		}
		return encodeToolOutput(value, args.MaxBytes)
	case "describe_resource", "list_events":
		var args resourceArgs
		if err := decodeStrict(raw, &args); err != nil {
			return "", err
		}
		if err := c.validateResourceArgs(args); err != nil {
			return "", err
		}
		var value string
		var err error
		if name == "describe_resource" {
			value, err = c.DescribeResource(ctx, args.Namespace, args.Kind, args.Name)
		} else {
			value, err = c.ListEvents(ctx, args.Namespace, args.Kind, args.Name)
		}
		if err != nil {
			return "", err
		}
		return encodeRawToolOutput(value, args.MaxBytes)
	case "get_pod_logs", "get_previous_pod_logs":
		var args logArgs
		if err := decodeStrict(raw, &args); err != nil {
			return "", err
		}
		if err := c.validateMaxBytes(args.MaxBytes); err != nil {
			return "", err
		}
		if err := validateNamespace(args.Namespace); err != nil {
			return "", err
		}
		if err := validateName("pod", args.Name); err != nil {
			return "", err
		}
		if args.Container != "" {
			if err := validateName("container", args.Container); err != nil {
				return "", err
			}
		}
		if args.TailLines < 1 || args.TailLines > c.maxLogTailLines {
			return "", fmt.Errorf("tail_lines must be between 1 and %d", c.maxLogTailLines)
		}
		value, err := c.GetPodLogs(ctx, args.Namespace, args.Name, args.Container, name == "get_previous_pod_logs", args.TailLines)
		if err != nil {
			return "", err
		}
		return encodeToolOutput(value, args.MaxBytes)
	case "get_owner_chain":
		var args podArgs
		if err := decodeStrict(raw, &args); err != nil {
			return "", err
		}
		if err := c.validateMaxBytes(args.MaxBytes); err != nil {
			return "", err
		}
		if err := validateNamespace(args.Namespace); err != nil {
			return "", err
		}
		if err := validateName("pod", args.Name); err != nil {
			return "", err
		}
		value, err := c.GetOwnerChain(ctx, args.Namespace, args.Name)
		if err != nil {
			return "", err
		}
		return encodeToolOutput(value, args.MaxBytes)
	case "get_node":
		var args nodeArgs
		if err := decodeStrict(raw, &args); err != nil {
			return "", err
		}
		if err := c.validateMaxBytes(args.MaxBytes); err != nil {
			return "", err
		}
		if err := validateName("node", args.Name); err != nil {
			return "", err
		}
		value, err := c.DescribeResource(ctx, "", "Node", args.Name)
		if err != nil {
			return "", err
		}
		return encodeRawToolOutput(value, args.MaxBytes)
	default:
		return "", fmt.Errorf("unknown or unapproved tool %q", name)
	}
}

func decodeStrict(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("invalid tool arguments: trailing JSON")
	}
	return nil
}

func (c *Client) validateResourceArgs(args resourceArgs) error {
	if err := c.validateMaxBytes(args.MaxBytes); err != nil {
		return err
	}
	kind := strings.ToLower(args.Kind)
	switch kind {
	case "pod", "deployment", "statefulset", "daemonset":
		if err := validateNamespace(args.Namespace); err != nil {
			return err
		}
	case "node":
		if args.Namespace != "" {
			return fmt.Errorf("namespace must be empty for Node")
		}
	default:
		return fmt.Errorf("unsupported Kubernetes resource kind %q", args.Kind)
	}
	return validateName(kind, args.Name)
}

func (c *Client) validateMaxBytes(value int) error {
	if value < 256 || value > c.maxToolBytes {
		return fmt.Errorf("max_bytes must be between 256 and %d", c.maxToolBytes)
	}
	return nil
}

func validateNamespace(value string) error {
	if value == "" {
		return fmt.Errorf("namespace is required")
	}
	if errors := kvalidation.IsDNS1123Label(value); len(errors) > 0 {
		return fmt.Errorf("invalid namespace %q", value)
	}
	return nil
}

func validateName(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s name is required", kind)
	}
	if errors := kvalidation.IsDNS1123Subdomain(value); len(errors) > 0 {
		return fmt.Errorf("invalid %s name %q", kind, value)
	}
	return nil
}

func (c *Client) errorOutput(err error) string {
	data, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return boundJSONBytes(data, min(c.maxToolBytes, 4096))
}

func encodeToolOutput(value any, maxBytes int) (string, error) {
	data, err := json.Marshal(map[string]any{"ok": true, "result": value})
	if err != nil {
		return "", err
	}
	return boundJSONBytes(data, maxBytes), nil
}

func encodeRawToolOutput(value string, maxBytes int) (string, error) {
	var raw json.RawMessage
	if json.Valid([]byte(value)) {
		raw = json.RawMessage(value)
	} else {
		raw, _ = json.Marshal(value)
	}
	data, err := json.Marshal(map[string]any{"ok": true, "result": raw})
	if err != nil {
		return "", err
	}
	return boundJSONBytes(data, maxBytes), nil
}

func boundJSONBytes(data []byte, maxBytes int) string {
	if maxBytes <= 0 || len(data) <= maxBytes {
		return string(data)
	}
	truncated, _ := json.Marshal(map[string]any{"ok": true, "truncated": true, "result": string(data[:max(0, maxBytes-80)])})
	if len(truncated) > maxBytes {
		truncated = []byte(`{"ok":false,"error":"tool output exceeded byte limit"}`)
	}
	return string(truncated)
}

func (c *Client) listWorkloads(ctx context.Context, args listArgs) ([]any, error) {
	out := make([]any, 0, args.Limit)
	appendValue := func(kind, name string, value any) bool {
		if args.NameContains != "" && !strings.Contains(strings.ToLower(name), args.NameContains) {
			return true
		}
		if len(out) >= args.Limit {
			return false
		}
		out = append(out, map[string]any{"kind": kind, "resource": value})
		return true
	}
	queryLimit := int64(args.Limit)
	if args.NameContains != "" {
		queryLimit = 1000
	}
	want := func(kind string) bool { return args.Kind == "all" || strings.EqualFold(args.Kind, kind) }
	if want("pod") {
		items, err := c.client.CoreV1().Pods(args.Namespace).List(ctx, metav1.ListOptions{LabelSelector: args.LabelSelector, Limit: queryLimit})
		if err != nil {
			return nil, err
		}
		for i := range items.Items {
			if !appendValue("Pod", items.Items[i].Name, summarizePod(&items.Items[i])) {
				return out, nil
			}
		}
	}
	if want("deployment") {
		items, err := c.client.AppsV1().Deployments(args.Namespace).List(ctx, metav1.ListOptions{LabelSelector: args.LabelSelector, Limit: queryLimit})
		if err != nil {
			return nil, err
		}
		for i := range items.Items {
			if !appendValue("Deployment", items.Items[i].Name, summarizeDeployment(&items.Items[i])) {
				return out, nil
			}
		}
	}
	if want("statefulset") {
		items, err := c.client.AppsV1().StatefulSets(args.Namespace).List(ctx, metav1.ListOptions{LabelSelector: args.LabelSelector, Limit: queryLimit})
		if err != nil {
			return nil, err
		}
		for i := range items.Items {
			if !appendValue("StatefulSet", items.Items[i].Name, summarizeStatefulSet(&items.Items[i])) {
				return out, nil
			}
		}
	}
	if want("daemonset") {
		items, err := c.client.AppsV1().DaemonSets(args.Namespace).List(ctx, metav1.ListOptions{LabelSelector: args.LabelSelector, Limit: queryLimit})
		if err != nil {
			return nil, err
		}
		for i := range items.Items {
			if !appendValue("DaemonSet", items.Items[i].Name, summarizeDaemonSet(&items.Items[i])) {
				return out, nil
			}
		}
	}
	return out, nil
}
