package kubernetes

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDescribeEventsAndOwnerChain(t *testing.T) {
	controller := true
	client := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo-abc", Namespace: "apps", OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "demo-rs", Controller: &controller}}}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "demo-rs", Namespace: "apps", OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "demo", Controller: &controller}}}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "pull-failed", Namespace: "apps"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "demo-abc"}, Type: "Warning", Reason: "Failed", Message: "image pull failed", Count: 2},
	)
	tools := NewWithClient(client)

	description, err := tools.DescribeResource(context.Background(), "apps", "pod", "demo-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, "demo-abc") {
		t.Fatalf("description missing pod name: %s", description)
	}

	events, err := tools.ListEvents(context.Background(), "apps", "pod", "demo-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events, "image pull failed") {
		t.Fatalf("events missing message: %s", events)
	}

	chain, err := tools.GetOwnerChain(context.Background(), "apps", "demo-abc")
	if err != nil {
		t.Fatal(err)
	}
	if chain != "Pod/demo-abc -> ReplicaSet/demo-rs -> Deployment/demo" {
		t.Fatalf("unexpected owner chain %q", chain)
	}
}
