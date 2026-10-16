package cnpg

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deploys-app/deployer/k8s"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestRestoreFailureReadsNativeEvidenceWithoutWrites(t *testing.T) {
	controller := true
	cluster := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "target", "uid": "71111111-1111-4111-8111-111111111111"}, "status": map[string]any{"targetPrimary": "target-1"}}}
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "target-1-full-recovery", UID: "61111111-1111-4111-8111-111111111111", Labels: map[string]string{"cnpg.io/jobRole": "full-recovery", "cnpg.io/instanceName": "target-1"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", UID: cluster.GetUID(), Controller: &controller}}}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	for _, denial := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "transport"}[denial], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Query().Get("labelSelector") != "cnpg.io/cluster=target" {
					t.Error("unexpected native mutation or scope")
					w.WriteHeader(500)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if denial {
					w.WriteHeader(403)
					_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "code": 403, "reason": "Forbidden"})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					_ = json.NewEncoder(w).Encode(batchv1.JobList{TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "JobList"}, Items: []batchv1.Job{job}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pods") {
					_ = json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}})
					return
				}
				t.Error("unexpected native request")
				w.WriteHeader(500)
			}))
			defer server.Close()
			t.Setenv("KUBE_PROXY_URL", server.URL)
			client, err := k8s.NewLocalClient("app")
			if err != nil {
				t.Fatal(err)
			}
			err = restoreFailure(context.Background(), client, cluster)
			var failed *RestoreFailure
			if errors.As(err, &failed) == denial {
				t.Fatal("transport/native failure confused", err)
			}
			if !denial && (failed.TargetClusterUID != string(cluster.GetUID()) || failed.RecoveryJobUID != string(job.UID)) {
				t.Fatal("native failure identity lost")
			}
		})
	}
}

func TestRestoreFailureRequiresTerminalOwnedJobAndStoppedPods(t *testing.T) {
	clusterUID := types.UID("71111111-1111-4111-8111-111111111111")
	controller := true
	base := batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "target-1-full-recovery", UID: "61111111-1111-4111-8111-111111111111", Labels: map[string]string{"cnpg.io/jobRole": "full-recovery", "cnpg.io/instanceName": "target-1"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", UID: clusterUID, Controller: &controller}}}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	for _, mode := range []string{"terminal", "retrying", "failure-target", "foreign", "deleting", "active", "succeeded", "running-pod", "terminating-pod", "unknown-pod", "multiple-jobs", "missing-job", "missing-uid", "wrong-role", "wrong-instance"} {
		t.Run(mode, func(t *testing.T) {
			job := *base.DeepCopy()
			jobs := []batchv1.Job{job}
			pods := []corev1.Pod{{Status: corev1.PodStatus{Phase: corev1.PodFailed}}}
			switch mode {
			case "retrying":
				jobs[0].Status.Conditions = nil
			case "failure-target":
				jobs[0].Status.Conditions[0].Type = batchv1.JobConditionType("FailureTarget")
			case "foreign":
				jobs[0].OwnerReferences[0].UID = "other"
			case "deleting":
				now := metav1.Now()
				jobs[0].DeletionTimestamp = &now
			case "active":
				jobs[0].Status.Active = 1
			case "succeeded":
				jobs[0].Status.Succeeded = 1
			case "running-pod":
				pods[0].Status.Phase = corev1.PodRunning
			case "terminating-pod":
				now := metav1.Now()
				pods[0].DeletionTimestamp = &now
				pods[0].Status.Phase = corev1.PodRunning
			case "unknown-pod":
				pods[0].Status.Phase = corev1.PodUnknown
			case "multiple-jobs":
				jobs = append(jobs, job)
			case "missing-job":
				jobs = nil
			case "missing-uid":
				jobs[0].UID = ""
			case "wrong-instance":
				jobs[0].Name = "target-2-full-recovery"
			case "wrong-role":
				jobs[0].Labels["cnpg.io/jobRole"] = "initdb"
			}
			got := failedRecoveryJob(clusterUID, "target-1", jobs, pods)
			if (got != "") != (mode == "terminal") {
				t.Fatal("unsafe terminal classification", mode, got)
			}
		})
	}
}
