package detection

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEvaluatePodDeterministicFailures(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "crash loop",
			pod:  podWithStatus(corev1.PodRunning, corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}),
			want: FailureCrashLoop,
		},
		{
			name: "image pull",
			pod:  podWithStatus(corev1.PodPending, corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}}),
			want: FailureImagePull,
		},
		{
			name: "repeated recent unexpected restarts",
			pod: podWithStatus(corev1.PodRunning, corev1.ContainerStatus{Name: "app", RestartCount: 3, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(-time.Minute)),
			}}}),
			want: FailureContainerRestarts,
		},
		{
			name: "unschedulable pending",
			pod:  &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}, Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}}},
			want: FailureUnschedulable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluatePod(tt.pod, RuleConfig{RestartThreshold: 3, RestartWindow: 10 * time.Minute, Now: now})
			if len(got) != 1 || got[0].Identity != tt.want {
				t.Fatalf("EvaluatePod() = %#v, want one %q failure", got, tt.want)
			}
		})
	}
}

func TestEvaluatePodIgnoresHistoricalRestarts(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pod := podWithStatus(corev1.PodRunning, corev1.ContainerStatus{Name: "app", RestartCount: 8, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(-time.Hour)),
	}}})
	if got := EvaluatePod(pod, RuleConfig{RestartThreshold: 3, RestartWindow: 10 * time.Minute, Now: now}); len(got) != 0 {
		t.Fatalf("historical restart produced failures: %#v", got)
	}
}

func TestEvaluatePodIgnoresExpectedCompletionAndMaintenance(t *testing.T) {
	completed := podWithStatus(corev1.PodRunning, corev1.ContainerStatus{Name: "job", RestartCount: 5, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}})
	if got := EvaluatePod(completed, RuleConfig{RestartThreshold: 3}); len(got) != 0 {
		t.Fatalf("completed container produced failures: %#v", got)
	}
	maintained := podWithStatus(corev1.PodRunning, corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}})
	maintained.Annotations = map[string]string{"nerv.io/maintenance": "true"}
	if got := EvaluatePod(maintained, RuleConfig{RestartThreshold: 3, MaintenanceAnnotation: "nerv.io/maintenance"}); len(got) != 0 {
		t.Fatalf("maintenance-suppressed pod produced failures: %#v", got)
	}
}

func TestEvaluateWorkloadFailures(t *testing.T) {
	replicas := int32(3)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		}}},
	}
	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1},
	}
	daemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "ns", Generation: 1},
		Status:     appsv1.DaemonSetStatus{ObservedGeneration: 1, DesiredNumberScheduled: 3, NumberAvailable: 1},
	}

	assertOneFailure(t, EvaluateDeployment(deployment, RuleConfig{}), FailureDeploymentRollout)
	deployment.Status.Conditions = nil
	deployment.Status.AvailableReplicas = 2
	assertOneFailure(t, EvaluateDeployment(deployment, RuleConfig{}), FailureDeploymentUnavailable)
	assertOneFailure(t, EvaluateStatefulSet(statefulSet, RuleConfig{}), FailureStatefulUnavailable)
	assertOneFailure(t, EvaluateDaemonSet(daemonSet, RuleConfig{}), FailureDaemonUnavailable)
}

func TestEvaluateWorkloadWaitsForObservedGeneration(t *testing.T) {
	replicas := int32(2)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Generation: 2}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1}}
	if got := EvaluateDeployment(deployment, RuleConfig{}); len(got) != 0 {
		t.Fatalf("stale deployment status produced failures: %#v", got)
	}
}

func podWithStatus(phase corev1.PodPhase, status corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"}, Status: corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{status}}}
}

func assertOneFailure(t *testing.T, got []Failure, want string) {
	t.Helper()
	if len(got) != 1 || got[0].Identity != want {
		t.Fatalf("failures = %#v, want one %q failure", got, want)
	}
}
