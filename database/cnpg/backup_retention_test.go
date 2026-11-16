package cnpg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type retentionFixture struct {
	mu           sync.Mutex
	job          *batchv1.Job
	pods         []v1.Pod
	backups      map[string]*unstructured.Unstructured
	logs         map[string]string
	creates      int
	lost         bool
	denyLogs     bool
	truncateLogs bool
	latePod      bool
}

func newRetentionFixture(t *testing.T) (*retentionFixture, *backupFixture, *k8s.Client, Profile, *api.DeployerCommandCNPGBackupRetention) {
	t.Helper()
	native, c, p := newBackupFixture(t)
	seed := backupCommand()
	first, err := ApplyBackup(context.Background(), c, seed, p)
	if err != nil {
		t.Fatal(err)
	}
	native.mu.Lock()
	native.backup["status"] = completedBackupStatus()
	native.mu.Unlock()
	completed, err := ApplyBackup(context.Background(), c, &first.DeployerCommandCNPGBackup, p)
	if err != nil || completed.Material == nil {
		t.Fatal(err)
	}
	it := &api.DeployerCommandCNPGBackupRetention{DeployerCommandCNPGIdentity: seed.DeployerCommandCNPGIdentity, CreateOperationID: seed.CreateOperationID, SourceClusterUID: first.SourceClusterUID}
	it.OperationID = "f1111111-1111-4111-8111-111111111111"
	f := &retentionFixture{backups: map[string]*unstructured.Unstructured{}, logs: map[string]string{"delete": ""}}
	var catalog []map[string]string
	for i := range 8 {
		b := api.CNPGBackupRetentionItem{OperationID: fmt.Sprintf("b1111111-1111-4111-8111-%012d", i+1), BackupUID: fmt.Sprintf("d1111111-1111-4111-8111-%012d", i+1), Material: *completed.Material}
		b.Material.BarmanBackupID = fmt.Sprintf("202611%02dT020000", i+1)
		it.Backups = append(it.Backups, b)
		backup := (&unstructured.Unstructured{Object: native.backup}).DeepCopy()
		backup.SetName("cnpg-backup-" + b.OperationID)
		backup.SetUID(types.UID(b.BackupUID))
		labels := backup.GetLabels()
		labels["nortezh.io/operation-id"] = b.OperationID
		backup.SetLabels(labels)
		_ = unstructured.SetNestedField(backup.Object, b.Material.BarmanBackupID, "status", "backupId")
		f.backups[backup.GetName()] = backup
		if i > 0 {
			catalog = append(catalog, map[string]string{"backup_id": b.Material.BarmanBackupID, "status": "DONE"})
		}
	}
	it.ReplacementOperationID = it.Backups[7].OperationID
	data, _ := json.Marshal(map[string]any{"backups_list": catalog})
	f.logs["catalog"] = string(data)
	if err := it.Valid(); err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse(os.Getenv("KUBE_PROXY_URL"))
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		missing := func() {
			w.WriteHeader(404)
			reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
		}
		switch {
		case strings.Contains(r.URL.Path, "/backups/"):
			backup := f.backups[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if backup == nil {
				missing()
			} else {
				reply(backup)
			}
		case strings.HasSuffix(r.URL.Path, "/jobs") && r.Method == http.MethodPost:
			f.creates++
			_ = json.NewDecoder(r.Body).Decode(&f.job)
			f.job.UID = "f2222222-2222-4222-8222-222222222222"
			f.job.Spec.Template.Spec.DeprecatedServiceAccount = f.job.Spec.Template.Spec.ServiceAccountName
			if f.lost {
				w.WriteHeader(504)
				reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "Timeout", "code": 504})
			} else {
				reply(f.job)
			}
		case strings.Contains(r.URL.Path, "/jobs/"):
			if f.job == nil {
				missing()
			} else {
				reply(f.job)
			}
		case strings.HasSuffix(r.URL.Path, "/pods"):
			reply(&v1.PodList{Items: f.pods})
		case strings.HasSuffix(r.URL.Path, "/log"):
			if f.denyLogs {
				w.WriteHeader(403)
				return
			}
			if r.URL.Query().Get("limitBytes") != "1048577" || r.URL.Query().Get("tailLines") != "" || r.URL.Query().Get("follow") == "true" {
				t.Error("logs are not bounded full-request observations")
			}
			data := f.logs[r.URL.Query().Get("container")]
			if f.truncateLogs {
				w.Header().Set("Content-Length", fmt.Sprint(len(data)+10))
			}
			_, _ = w.Write([]byte(data))
			if f.latePod && r.URL.Query().Get("container") == "catalog" {
				pod := *f.pods[0].DeepCopy()
				pod.UID = "f4444444-4444-4444-8444-444444444444"
				f.pods = append(f.pods, pod)
			}
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("KUBE_PROXY_URL", server.URL)
	c, err = k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	return f, native, c, p, it
}

func (f *retentionFixture) finish() {
	f.job.Status = batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: v1.ConditionTrue}}}
	yes := true
	f.pods = []v1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "retention-pod", Namespace: f.job.Namespace, UID: "f3333333-3333-4333-8333-333333333333", OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: f.job.Name, UID: f.job.UID, Controller: &yes}}}, Spec: *f.job.Spec.Template.Spec.DeepCopy(), Status: v1.PodStatus{Phase: v1.PodSucceeded,
		InitContainerStatuses: []v1.ContainerStatus{{Name: "delete", State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 0}}}},
		ContainerStatuses:     []v1.ContainerStatus{{Name: "catalog", State: v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 0}}}}}}}
}

func TestRetentionCalledJobCheckpointAndACKLoss(t *testing.T) {
	f, _, c, p, it := newRetentionFixture(t)
	f.lost = true
	first, err := ApplyBackupRetention(context.Background(), c, it, p)
	if err != nil || first == nil || first.Status != api.CNPGBackupRetentionRunning || first.JobUID == "" || first.Valid() != nil {
		t.Fatal("missing checkpoint", first, err)
	}
	f.mu.Lock()
	f.finish()
	f.mu.Unlock()
	// An ACK-lost uncheckpointed command only re-observes/ACKs the same deterministic Job.
	replay, err := ApplyBackupRetention(context.Background(), c, it, p)
	if err != nil || replay.Status != api.CNPGBackupRetentionRunning || replay.JobUID != first.JobUID {
		t.Fatal("lost create/ACK response", err)
	}
	it.JobUID = first.JobUID
	for range 2 {
		result, err := ApplyBackupRetention(context.Background(), c, it, p)
		if err != nil || result == nil || result.Status != api.CNPGBackupRetentionCompleted || result.Valid() != nil {
			t.Fatal("no validated completion", result, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 {
		t.Fatal("duplicate execution", f.creates)
	}
	if f.job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *f.job.Spec.Template.Spec.AutomountServiceAccountToken || *f.job.Spec.BackoffLimit != 0 || f.job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("unsafe Job lifecycle")
	}
	init := f.job.Spec.Template.Spec.InitContainers[0]
	if init.Image != readinessSidecarImage || init.Command[0] != "/venv/bin/barman-cloud-backup-delete" || strings.Join(init.Args, " ") != "--cloud-provider aws-s3 --endpoint-url http://minio.minio.svc.cluster.local:9000 --retention-policy REDUNDANCY 7 s3://cnpg-backups/staging cnpg-1-2" {
		t.Fatal("not fixed stock argv")
	}
	live := f.job.DeepCopy()
	live.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": string(live.UID)}}
	live.Spec.Template.Labels["batch.kubernetes.io/controller-uid"] = string(live.UID)
	live.Spec.Template.Labels["batch.kubernetes.io/job-name"] = live.Name
	mode := batchv1.NonIndexedCompletion
	live.Spec.CompletionMode = &mode
	grace := int64(30)
	live.Spec.Template.Spec.TerminationGracePeriodSeconds = &grace
	if !retentionJobMatches(f.job, live) {
		t.Fatal("standard API/controller defaults rejected")
	}
	live.Spec.Template.Spec.Volumes = []v1.Volume{{Name: "foreign"}}
	if retentionJobMatches(f.job, live) {
		t.Fatal("unreviewed execution fields accepted")
	}
	for _, env := range init.Env {
		if env.Name == "AWS_SECRET_ACCESS_KEY" && (env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil) {
			t.Fatal("credential value in manifest")
		}
	}
}

func TestRetentionServiceAccountDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, account, alias string
		matches              bool
	}{
		{"empty-alias", "default", "", true},
		{"matching-alias", "default", "default", true},
		{"conflicting-alias", "default", "foreign", false},
		{"changed-account", "foreign", "", false},
		{"changed-account-and-alias", "foreign", "foreign", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := &batchv1.Job{Spec: batchv1.JobSpec{Template: v1.PodTemplateSpec{Spec: v1.PodSpec{ServiceAccountName: "default"}}}}
			actual := expected.DeepCopy()
			actual.Spec.Template.Spec.ServiceAccountName = tc.account
			actual.Spec.Template.Spec.DeprecatedServiceAccount = tc.alias
			if got := retentionJobMatches(expected, actual); got != tc.matches {
				t.Errorf("Job match = %v, want %v", got, tc.matches)
			}
			expected.Spec.Template.Spec.DeprecatedServiceAccount = "default"
			pod := &v1.Pod{Spec: *actual.Spec.Template.Spec.DeepCopy()}
			if got := retentionPodMatches(expected, pod); got != tc.matches {
				t.Errorf("Pod match = %v, want %v", got, tc.matches)
			}
		})
	}
}

func TestRetentionRejectsUnsafeObservation(t *testing.T) {
	cases := []string{"missing-job", "replaced-job", "foreign-spec", "foreign-label", "credential-drift", "destination-drift", "unsupported-scope", "delete-warning", "swallowed-wal-error", "catalog-warning", "extra-json", "duplicate-key", "overflow", "unreadable", "truncated", "late-pod", "missing-id", "extra-id", "keep", "duplicate-id", "failed-status", "two-pods", "uncounted-pod", "active-pod", "main-failure", "restart", "failed-job", "failure-target"}
	for _, mode := range cases {
		t.Run(mode, func(t *testing.T) {
			f, native, c, p, it := newRetentionFixture(t)
			first, err := ApplyBackupRetention(context.Background(), c, it, p)
			if err != nil || first == nil || first.JobUID == "" {
				t.Fatal(err)
			}
			it.JobUID = first.JobUID
			f.mu.Lock()
			f.finish()
			switch mode {
			case "missing-job":
				f.job = nil
			case "replaced-job":
				f.job.UID = "f4444444-4444-4444-8444-444444444444"
			case "foreign-spec":
				f.job.Spec.Template.Spec.Containers[0].Args = append(f.job.Spec.Template.Spec.Containers[0].Args, "--quiet")
			case "foreign-label":
				f.job.Labels["nortezh.io/operation-id"] = "foreign"
			case "delete-warning":
				f.logs["delete"] = "WARNING: missing object\n"
			case "swallowed-wal-error":
				f.logs["delete"] = "ERROR: WAL deletion failed\n"
			case "catalog-warning":
				f.logs["catalog"] = "WARNING: skipped metadata\n" + f.logs["catalog"]
			case "extra-json":
				f.logs["catalog"] += "{}"
			case "duplicate-key":
				f.logs["catalog"] = strings.Replace(f.logs["catalog"], `"status":"DONE"`, `"status":"FAILED","status":"DONE"`, 1)
			case "overflow":
				f.logs["catalog"] = strings.Repeat(" ", retentionLogLimit+1)
			case "unreadable":
				f.denyLogs = true
			case "truncated":
				f.truncateLogs = true
			case "late-pod":
				f.latePod = true
			case "missing-id":
				f.logs["catalog"] = `{"backups_list":[]}`
			case "extra-id":
				f.logs["catalog"] = strings.Replace(f.logs["catalog"], `]}`, `,{"backup_id":"20261201T020000","status":"DONE"}]}`, 1)
			case "keep":
				f.logs["catalog"] = strings.Replace(f.logs["catalog"], it.Backups[7].Material.BarmanBackupID, it.Backups[0].Material.BarmanBackupID, 1)
			case "duplicate-id":
				f.logs["catalog"] = strings.Replace(f.logs["catalog"], it.Backups[7].Material.BarmanBackupID, it.Backups[1].Material.BarmanBackupID, 1)
			case "failed-status":
				f.logs["catalog"] = strings.Replace(f.logs["catalog"], "DONE", "FAILED", 1)
			case "two-pods":
				pod := *f.pods[0].DeepCopy()
				pod.UID = "f4444444-4444-4444-8444-444444444444"
				f.pods = append(f.pods, pod)
			case "uncounted-pod":
				f.job.Status.UncountedTerminatedPods = &batchv1.UncountedTerminatedPods{Succeeded: []types.UID{"f4444444-4444-4444-8444-444444444444"}}
			case "active-pod":
				f.pods[0].Status.Phase = v1.PodRunning
			case "main-failure":
				f.pods[0].Status.ContainerStatuses[0].State.Terminated.ExitCode = 1
			case "restart":
				f.pods[0].Status.InitContainerStatuses[0].RestartCount = 1
			case "failed-job":
				f.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: v1.ConditionTrue}}
			case "failure-target":
				f.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailureTarget, Status: v1.ConditionTrue}}
			}
			f.mu.Unlock()
			native.mu.Lock()
			switch mode {
			case "credential-drift":
				native.credentials.ResourceVersion = "8"
			case "destination-drift":
				native.store["spec"].(map[string]any)["configuration"].(map[string]any)["destinationPath"] = "s3://other"
			case "unsupported-scope":
				p.BackupObjectStore = "other"
			}
			native.mu.Unlock()
			result, err := ApplyBackupRetention(context.Background(), c, it, p)
			if mode == "credential-drift" || mode == "destination-drift" {
				if err == nil || result != nil {
					t.Fatal("uncertain source/profile produced a receipt")
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if f.creates != 1 {
					t.Fatal("scope drift created another Job")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := api.CNPGBackupRetentionBlocked
			if mode == "failure-target" {
				expected = api.CNPGBackupRetentionRunning
			}
			if result == nil || result.Status != expected || result.Valid() != nil {
				t.Fatal("unsafe observation accepted or invalid", result, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.creates != 1 {
				t.Fatal("recreated Job")
			}
		})
	}
}

func TestRetentionDoesNotAdoptForeignUncheckpointedJob(t *testing.T) {
	f, _, c, p, it := newRetentionFixture(t)
	first, err := ApplyBackupRetention(context.Background(), c, it, p)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.job.Spec.Template.Spec.Containers[0].Image = "foreign"
	f.mu.Unlock()
	result, err := ApplyBackupRetention(context.Background(), c, it, p)
	if err != nil || result.Status != api.CNPGBackupRetentionBlocked || result.JobUID != "" {
		t.Fatal("adopted foreign Job", err)
	}
}
