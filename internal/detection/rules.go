package detection

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	FailureCrashLoop             = "crash-loop"
	FailureImagePull             = "image-pull"
	FailureUnschedulable         = "unschedulable"
	FailureContainerRestarts     = "container-restarts"
	FailureDeploymentRollout     = "deployment-rollout"
	FailureDeploymentUnavailable = "deployment-unavailable"
	FailureStatefulUnavailable   = "statefulset-unavailable"
	FailureDaemonUnavailable     = "daemonset-unavailable"
)

type Failure struct {
	Identity  string
	Kind      string
	Namespace string
	Name      string
	Container string
	Reason    string
	Summary   string
}

type RuleConfig struct {
	RestartThreshold      int32
	RestartWindow         time.Duration
	Now                   time.Time
	MaintenanceAnnotation string
}

func EvaluatePod(pod *corev1.Pod, cfg RuleConfig) []Failure {
	if pod == nil || maintenanceSuppressed(pod.Annotations, cfg.MaintenanceAnnotation) {
		return nil
	}
	var failures []Failure
	for _, status := range pod.Status.ContainerStatuses {
		reason := ""
		if status.State.Waiting != nil {
			reason = status.State.Waiting.Reason
		}
		switch reason {
		case "CrashLoopBackOff":
			failures = append(failures, podFailure(pod, status.Name, FailureCrashLoop, reason,
				fmt.Sprintf("container %s is in CrashLoopBackOff", status.Name)))
			continue
		case "ImagePullBackOff", "ErrImagePull":
			failures = append(failures, podFailure(pod, status.Name, FailureImagePull, reason,
				fmt.Sprintf("container %s cannot pull its image: %s", status.Name, reason)))
			continue
		}
		if cfg.RestartThreshold > 0 && status.RestartCount >= cfg.RestartThreshold && recentUnexpectedTermination(status.LastTerminationState.Terminated, cfg.Now, cfg.RestartWindow) {
			failures = append(failures, podFailure(pod, status.Name, FailureContainerRestarts, "RepeatedRestarts",
				fmt.Sprintf("container %s restarted %d times after %s", status.Name, status.RestartCount, status.LastTerminationState.Terminated.Reason)))
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
				failures = append(failures, podFailure(pod, "", FailureUnschedulable, condition.Reason,
					"pod remains Pending because it is unschedulable"))
				break
			}
		}
	}
	return failures
}

func EvaluateDeployment(value *appsv1.Deployment, cfg RuleConfig) []Failure {
	if value == nil || maintenanceSuppressed(value.Annotations, cfg.MaintenanceAnnotation) || value.Status.ObservedGeneration < value.Generation {
		return nil
	}
	for _, condition := range value.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse && condition.Reason == "ProgressDeadlineExceeded" {
			return []Failure{workloadFailure("deployment", value.Namespace, value.Name, FailureDeploymentRollout, condition.Reason,
				"deployment rollout exceeded its progress deadline")}
		}
	}
	desired := int32(1)
	if value.Spec.Replicas != nil {
		desired = *value.Spec.Replicas
	}
	if desired > 0 && value.Status.AvailableReplicas < desired {
		return []Failure{workloadFailure("deployment", value.Namespace, value.Name, FailureDeploymentUnavailable, "UnavailableReplicas",
			fmt.Sprintf("deployment has %d/%d available replicas", value.Status.AvailableReplicas, desired))}
	}
	return nil
}

func EvaluateStatefulSet(value *appsv1.StatefulSet, cfg RuleConfig) []Failure {
	if value == nil || maintenanceSuppressed(value.Annotations, cfg.MaintenanceAnnotation) || value.Status.ObservedGeneration < value.Generation {
		return nil
	}
	desired := int32(1)
	if value.Spec.Replicas != nil {
		desired = *value.Spec.Replicas
	}
	if desired > 0 && value.Status.ReadyReplicas < desired {
		return []Failure{workloadFailure("statefulset", value.Namespace, value.Name, FailureStatefulUnavailable, "UnavailableReplicas",
			fmt.Sprintf("statefulset has %d/%d ready replicas", value.Status.ReadyReplicas, desired))}
	}
	return nil
}

func EvaluateDaemonSet(value *appsv1.DaemonSet, cfg RuleConfig) []Failure {
	if value == nil || maintenanceSuppressed(value.Annotations, cfg.MaintenanceAnnotation) || value.Status.ObservedGeneration < value.Generation {
		return nil
	}
	desired := value.Status.DesiredNumberScheduled
	if desired > 0 && value.Status.NumberAvailable < desired {
		return []Failure{workloadFailure("daemonset", value.Namespace, value.Name, FailureDaemonUnavailable, "UnavailableReplicas",
			fmt.Sprintf("daemonset has %d/%d available replicas", value.Status.NumberAvailable, desired))}
	}
	return nil
}

func podFailure(pod *corev1.Pod, container, identity, reason, summary string) Failure {
	return Failure{Identity: identity, Kind: "pod", Namespace: pod.Namespace, Name: pod.Name, Container: container, Reason: reason, Summary: summary}
}

func workloadFailure(kind, namespace, name, identity, reason, summary string) Failure {
	return Failure{Identity: identity, Kind: kind, Namespace: namespace, Name: name, Reason: reason, Summary: summary}
}

func recentUnexpectedTermination(termination *corev1.ContainerStateTerminated, now time.Time, window time.Duration) bool {
	if termination == nil || termination.Reason == "Completed" || termination.ExitCode == 0 || now.IsZero() || window <= 0 || termination.FinishedAt.IsZero() {
		return false
	}
	return !termination.FinishedAt.Time.Before(now.Add(-window))
}

func maintenanceSuppressed(annotations map[string]string, key string) bool {
	if key == "" || annotations == nil {
		return false
	}
	value, ok := annotations[key]
	if !ok {
		return false
	}
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return true
	}
	parsed, err := strconv.ParseBool(value)
	if err == nil {
		return parsed
	}
	return value != "off" && value != "no" && value != "disabled"
}
