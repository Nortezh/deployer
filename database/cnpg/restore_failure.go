package cnpg

import (
	"context"
	"errors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deploys-app/deployer/k8s"
)

// RestoreFailure is native evidence only; it never authorizes resource cleanup.
type RestoreFailure struct{ TargetClusterUID, RecoveryJobUID string }

func (*RestoreFailure) Error() string { return "CNPG restore failed" }

// shortcut: CNPG 1.27.4 retains non-completed bootstrap Jobs; revalidate before an operator upgrade.
// Require the terminal Job condition and independently check Pod termination; never use a phase string or FailureTarget.
func restoreFailure(ctx context.Context, c *k8s.Client, cluster *unstructured.Unstructured) error {
	jobs, err := c.Core().BatchV1().Jobs(c.DBNamespace()).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + cluster.GetName()})
	if err != nil {
		return errors.New("CNPG recovery Job unavailable")
	}
	pods, err := c.Core().CoreV1().Pods(c.DBNamespace()).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + cluster.GetName()})
	if err != nil {
		return errors.New("CNPG recovery Pods unavailable")
	}
	if uid := failedRecoveryJob(cluster.GetUID(), str(cluster, "status", "targetPrimary"), jobs.Items, pods.Items); uid != "" {
		return &RestoreFailure{TargetClusterUID: string(cluster.GetUID()), RecoveryJobUID: string(uid)}
	}
	return nil
}

func failedRecoveryJob(clusterUID types.UID, instance string, jobs []batchv1.Job, pods []corev1.Pod) types.UID {
	if clusterUID == "" || instance == "" || len(jobs) != 1 {
		return ""
	}
	job := &jobs[0]
	owner := metav1.GetControllerOf(job)
	if job.UID == "" || job.Name != instance+"-full-recovery" || job.Labels["cnpg.io/instanceName"] != instance || job.DeletionTimestamp != nil || owner == nil || owner.UID != clusterUID || owner.Kind != "Cluster" || owner.APIVersion != "postgresql.cnpg.io/v1" || job.Labels["cnpg.io/jobRole"] != "full-recovery" || job.Status.Active != 0 || job.Status.Succeeded != 0 {
		return ""
	}
	failed := false
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return ""
		}
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			failed = true
		}
	}
	if !failed {
		return ""
	}
	for _, pod := range pods {
		if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
			return ""
		}
	}
	return job.UID
}
