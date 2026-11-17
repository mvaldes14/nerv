package kubernetes

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mvaldes14/nerv/internal/investigation"
)

func TestAgentToolDefinitionsAreStrict(t *testing.T) {
	tools := NewWithClient(fake.NewClientset()).Definitions()
	if len(tools) != 7 {
		t.Fatalf("definitions = %d, want 7", len(tools))
	}
	for _, tool := range tools {
		if !tool.Function.Strict {
			t.Errorf("tool %s is not strict", tool.Function.Name)
		}
		if got := tool.Function.Parameters["additionalProperties"]; got != false {
			t.Errorf("tool %s additionalProperties = %#v", tool.Function.Name, got)
		}
		if _, ok := tool.Function.Parameters["required"]; !ok {
			t.Errorf("tool %s missing required fields", tool.Function.Name)
		}
	}
}

func TestAgentListWorkloadsDiscoversAcrossNamespaces(t *testing.T) {
	client := NewWithClient(fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vault-0", Namespace: "security"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "apps"}},
	))
	result := client.Execute(context.Background(), investigation.ToolCall{ID: "call-1", Function: investigation.ToolCallFunction{Name: "list_workloads", Arguments: `{"namespace":"","kind":"all","label_selector":"","name_contains":"vault","limit":20,"max_bytes":4096}`}})
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	for _, want := range []string{"vault-0", "security", `"kind":"Pod"`} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("result missing %q: %s", want, result.Content)
		}
	}
	for _, unwanted := range []string{"api", `"kind":"Deployment"`} {
		if strings.Contains(result.Content, unwanted) {
			t.Errorf("result unexpectedly contains %q: %s", unwanted, result.Content)
		}
	}
}

func TestAgentToolsRejectUnsafeAndMalformedCalls(t *testing.T) {
	client := NewWithClient(fake.NewClientset())
	client.ConfigureAgentTools(1024, 100)
	tests := []investigation.ToolCall{
		{Function: investigation.ToolCallFunction{Name: "exec", Arguments: `{}`}},
		{Function: investigation.ToolCallFunction{Name: "describe_resource", Arguments: `{"kind":"Secret","namespace":"default","name":"token","max_bytes":512}`}},
		{Function: investigation.ToolCallFunction{Name: "describe_resource", Arguments: `{"kind":"Pod","namespace":"../bad","name":"pod","max_bytes":512}`}},
		{Function: investigation.ToolCallFunction{Name: "get_pod_logs", Arguments: `{"namespace":"default","name":"pod","container":"","tail_lines":101,"max_bytes":512}`}},
		{Function: investigation.ToolCallFunction{Name: "get_owner_chain", Arguments: `{"namespace":"default","name":"pod","max_bytes":2048}`}},
		{Function: investigation.ToolCallFunction{Name: "get_owner_chain", Arguments: `not-json`}},
		{Function: investigation.ToolCallFunction{Name: "get_node", Arguments: `{"name":"node","max_bytes":512,"extra":true}`}},
	}
	for _, call := range tests {
		result := client.Execute(context.Background(), call)
		if result.Error == "" || !strings.Contains(result.Content, "error") {
			t.Errorf("call %#v was not safely rejected: %#v", call.Function, result)
		}
		if len(result.Content) > 1024 {
			t.Errorf("error output exceeded bound: %d", len(result.Content))
		}
	}
}
