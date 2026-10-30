package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mvaldes14/nerv/internal/investigation"
)

const maxEvidenceBytes = 32 * 1024

type Client struct {
	client kubernetes.Interface
}

func New(kubeconfig string) (*Client, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes config: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return &Client{client: client}, nil
}

func NewWithClient(client kubernetes.Interface) *Client { return &Client{client: client} }

func (c *Client) Collect(ctx context.Context, req investigation.Request) ([]investigation.Evidence, error) {
	if req.Target == nil || req.Target.Name == "" {
		return nil, nil
	}
	target := *req.Target
	kind := strings.ToLower(target.Kind)
	if kind == "" {
		kind = "pod"
	}
	var evidence []investigation.Evidence

	description, err := c.DescribeResource(ctx, target.Namespace, kind, target.Name)
	if err != nil {
		return nil, err
	}
	evidence = append(evidence, investigation.Evidence{Source: "kubernetes.describe", Summary: description})

	events, err := c.ListEvents(ctx, target.Namespace, kind, target.Name)
	if err == nil && len(events) > 0 {
		evidence = append(evidence, investigation.Evidence{Source: "kubernetes.events", Summary: events})
	}

	if kind == "pod" || kind == "pods" {
		logs, logErr := c.GetPodLogs(ctx, target.Namespace, target.Name, target.Container, false, 200)
		if logErr == nil && strings.TrimSpace(logs) != "" {
			evidence = append(evidence, investigation.Evidence{Source: "kubernetes.logs", Summary: logs})
		}
		previous, previousErr := c.GetPodLogs(ctx, target.Namespace, target.Name, target.Container, true, 200)
		if previousErr == nil && strings.TrimSpace(previous) != "" {
			evidence = append(evidence, investigation.Evidence{Source: "kubernetes.previous_logs", Summary: previous})
		}
		owners, ownerErr := c.GetOwnerChain(ctx, target.Namespace, target.Name)
		if ownerErr == nil && owners != "" {
			evidence = append(evidence, investigation.Evidence{Source: "kubernetes.owner_chain", Summary: owners})
		}
	}
	return evidence, nil
}

func (c *Client) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	return c.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) ListPods(ctx context.Context, namespace, labelSelector string) ([]corev1.Pod, error) {
	list, err := c.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (c *Client) GetPodLogs(ctx context.Context, namespace, name, container string, previous bool, tailLines int64) (string, error) {
	opts := &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tailLines}
	stream, err := c.client.CoreV1().Pods(namespace).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, stream, maxEvidenceBytes); err != nil && err != io.EOF {
		return "", err
	}
	return buf.String(), nil
}

func (c *Client) DescribeResource(ctx context.Context, namespace, kind, name string) (string, error) {
	var resource any
	switch strings.ToLower(kind) {
	case "pod", "pods":
		pod, err := c.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		resource = summarizePod(pod)
	case "deployment", "deployments":
		deployment, err := c.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		resource = summarizeDeployment(deployment)
	case "statefulset", "statefulsets":
		statefulSet, err := c.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		resource = summarizeStatefulSet(statefulSet)
	case "daemonset", "daemonsets":
		daemonSet, err := c.client.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		resource = summarizeDaemonSet(daemonSet)
	case "node", "nodes":
		node, err := c.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		resource = summarizeNode(node)
	default:
		return "", fmt.Errorf("unsupported Kubernetes resource kind %q", kind)
	}
	return boundedJSON(resource)
}

func (c *Client) ListEvents(ctx context.Context, namespace, kind, name string) (string, error) {
	selector := fields.AndSelectors(
		fields.OneTermEqualSelector("involvedObject.name", name),
		fields.OneTermEqualSelector("involvedObject.kind", canonicalKind(kind)),
	).String()
	list, err := c.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		return "", err
	}
	type eventSummary struct {
		Type    string `json:"type"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Count   int32  `json:"count"`
	}
	out := make([]eventSummary, 0, len(list.Items))
	for _, event := range list.Items {
		out = append(out, eventSummary{Type: event.Type, Reason: event.Reason, Message: event.Message, Count: event.Count})
	}
	return boundedJSON(out)
}

func (c *Client) GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error) {
	return c.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) GetStatefulSet(ctx context.Context, namespace, name string) (*appsv1.StatefulSet, error) {
	return c.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) GetDaemonSet(ctx context.Context, namespace, name string) (*appsv1.DaemonSet, error) {
	return c.client.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	return c.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
}

func (c *Client) GetOwnerChain(ctx context.Context, namespace, podName string) (string, error) {
	pod, err := c.GetPod(ctx, namespace, podName)
	if err != nil {
		return "", err
	}
	chain := []string{"Pod/" + pod.Name}
	owners := pod.OwnerReferences
	for len(owners) > 0 {
		owner := owners[0]
		chain = append(chain, owner.Kind+"/"+owner.Name)
		if owner.Kind != "ReplicaSet" {
			break
		}
		rs, err := c.client.AppsV1().ReplicaSets(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil || len(rs.OwnerReferences) == 0 {
			break
		}
		owners = rs.OwnerReferences
	}
	return strings.Join(chain, " -> "), nil
}

type conditionSummary struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type containerSummary struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restartCount"`
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
}

func summarizePod(pod *corev1.Pod) any {
	containers := make([]containerSummary, 0, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		state, reason, message := containerState(status.State)
		containers = append(containers, containerSummary{Name: status.Name, Image: status.Image, Ready: status.Ready, RestartCount: status.RestartCount, State: state, Reason: reason, Message: message})
	}
	conditions := make([]conditionSummary, 0, len(pod.Status.Conditions))
	for _, condition := range pod.Status.Conditions {
		conditions = append(conditions, conditionSummary{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message})
	}
	return struct {
		Name       string                  `json:"name"`
		Namespace  string                  `json:"namespace"`
		Phase      corev1.PodPhase         `json:"phase"`
		Node       string                  `json:"node,omitempty"`
		PodIP      string                  `json:"podIP,omitempty"`
		CreatedAt  metav1.Time             `json:"createdAt"`
		Owners     []metav1.OwnerReference `json:"owners,omitempty"`
		Conditions []conditionSummary      `json:"conditions,omitempty"`
		Containers []containerSummary      `json:"containers,omitempty"`
	}{pod.Name, pod.Namespace, pod.Status.Phase, pod.Spec.NodeName, pod.Status.PodIP, pod.CreationTimestamp, pod.OwnerReferences, conditions, containers}
}

func summarizeDeployment(value *appsv1.Deployment) any {
	conditions := make([]conditionSummary, 0, len(value.Status.Conditions))
	for _, condition := range value.Status.Conditions {
		conditions = append(conditions, conditionSummary{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message})
	}
	return map[string]any{"name": value.Name, "namespace": value.Namespace, "generation": value.Generation, "observedGeneration": value.Status.ObservedGeneration, "replicas": value.Status.Replicas, "readyReplicas": value.Status.ReadyReplicas, "availableReplicas": value.Status.AvailableReplicas, "unavailableReplicas": value.Status.UnavailableReplicas, "conditions": conditions}
}

func summarizeStatefulSet(value *appsv1.StatefulSet) any {
	conditions := make([]conditionSummary, 0, len(value.Status.Conditions))
	for _, condition := range value.Status.Conditions {
		conditions = append(conditions, conditionSummary{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message})
	}
	return map[string]any{"name": value.Name, "namespace": value.Namespace, "generation": value.Generation, "observedGeneration": value.Status.ObservedGeneration, "replicas": value.Status.Replicas, "readyReplicas": value.Status.ReadyReplicas, "currentReplicas": value.Status.CurrentReplicas, "updatedReplicas": value.Status.UpdatedReplicas, "conditions": conditions}
}

func summarizeDaemonSet(value *appsv1.DaemonSet) any {
	conditions := make([]conditionSummary, 0, len(value.Status.Conditions))
	for _, condition := range value.Status.Conditions {
		conditions = append(conditions, conditionSummary{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message})
	}
	return map[string]any{"name": value.Name, "namespace": value.Namespace, "generation": value.Generation, "observedGeneration": value.Status.ObservedGeneration, "desiredNumberScheduled": value.Status.DesiredNumberScheduled, "numberReady": value.Status.NumberReady, "numberAvailable": value.Status.NumberAvailable, "numberUnavailable": value.Status.NumberUnavailable, "updatedNumberScheduled": value.Status.UpdatedNumberScheduled, "conditions": conditions}
}

func summarizeNode(value *corev1.Node) any {
	conditions := make([]conditionSummary, 0, len(value.Status.Conditions))
	for _, condition := range value.Status.Conditions {
		conditions = append(conditions, conditionSummary{Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason, Message: condition.Message})
	}
	return map[string]any{"name": value.Name, "unschedulable": value.Spec.Unschedulable, "conditions": conditions, "capacity": value.Status.Capacity, "allocatable": value.Status.Allocatable}
}

func containerState(state corev1.ContainerState) (string, string, string) {
	if state.Waiting != nil {
		return "waiting", state.Waiting.Reason, state.Waiting.Message
	}
	if state.Terminated != nil {
		return "terminated", state.Terminated.Reason, state.Terminated.Message
	}
	if state.Running != nil {
		return "running", "", ""
	}
	return "unknown", "", ""
}

func boundedJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > maxEvidenceBytes {
		data = append(data[:maxEvidenceBytes], []byte("...<truncated>")...)
	}
	return string(data), nil
}

func canonicalKind(kind string) string {
	switch strings.ToLower(kind) {
	case "pod", "pods":
		return "Pod"
	case "deployment", "deployments":
		return "Deployment"
	case "statefulset", "statefulsets":
		return "StatefulSet"
	case "daemonset", "daemonsets":
		return "DaemonSet"
	case "node", "nodes":
		return "Node"
	default:
		return kind
	}
}
