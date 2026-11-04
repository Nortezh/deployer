package cnpg

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const retentionLogLimit = 1 << 20

// ApplyBackupRetention reconciles one stock Job; it never retries deletion as a new Job.
func ApplyBackupRetention(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGBackupRetention, p Profile) (*api.DeployerSetResultCNPGBackupRetention, error) {
	if c == nil || it == nil || it.Valid() != nil || !p.Enabled() || p.AllocationNamespace == "" {
		return nil, errBackupUnavailable
	}
	result := &api.DeployerSetResultCNPGBackupRetention{DeployerCommandCNPGBackupRetention: *it, Status: api.CNPGBackupRetentionBlocked, FailureCode: api.CNPGFailureRetentionScope}
	// Reuse the current native source/bootstrap/profile checks, without creating a Backup.
	newest := it.Backups[7]
	if p.Image != newest.Material.Image || p.StorageClass != newest.Material.StorageClass || p.BackupObjectStore != newest.Material.ObjectStoreName || p.BackupProfileRevision != newest.Material.Archive.ProfileRevision {
		return result, nil
	}
	source := &api.DeployerCommandCNPGBackup{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity, CreateOperationID: it.CreateOperationID, SourceClusterUID: it.SourceClusterUID, BackupUID: newest.BackupUID}
	source.OperationID = newest.OperationID
	observed, err := ApplyBackup(ctx, c, source, p)
	// shortcut: shared Backup errors conflate scope/transport; keep the guard and prior phase until typed observations are available.
	if err != nil {
		return nil, err
	}
	if observed.Status != api.CNPGBackupCompleted || !reflect.DeepEqual(observed.Material, &newest.Material) {
		return result, nil
	}
	store, err := c.Dynamic().Resource(objectStoreGVR).Namespace(c.DBNamespace()).Get(ctx, newest.Material.ObjectStoreName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	archive, err := backupArchive(ctx, c, p, store)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(archive, newest.Material.Archive) {
		return result, nil
	}
	desired := retentionJob(it, c.DBNamespace(), store.Object)
	jobs := c.Core().BatchV1().Jobs(c.DBNamespace())
	job, err := jobs.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if it.JobUID != "" {
			result.FailureCode = api.CNPGFailureRetentionJob
			return result, nil
		}
		// Before the first execution every exact native material must still agree.
		for _, b := range it.Backups {
			check := *source
			check.OperationID = b.OperationID
			check.BackupUID = b.BackupUID
			seen, err := ApplyBackup(ctx, c, &check, p)
			if err != nil {
				return nil, err
			}
			if seen.Status != api.CNPGBackupCompleted || !reflect.DeepEqual(seen.Material, &b.Material) {
				return result, nil
			}
		}
		job, err = jobs.Create(ctx, desired, metav1.CreateOptions{})
		if err != nil { // A lost response may have created the deterministic Job.
			job, err = jobs.Get(ctx, desired.Name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return nil, err
	}
	result.FailureCode = api.CNPGFailureRetentionJob
	if job.UID == "" || job.DeletionTimestamp != nil || it.JobUID != "" && it.JobUID != string(job.UID) || !retentionJobMatches(desired, job) {
		return result, nil
	}
	result.JobUID = string(job.UID)
	// Always durably ACK the UID before reporting terminal observations.
	if it.JobUID == "" {
		result.Status = api.CNPGBackupRetentionRunning
		result.FailureCode = ""
		return result, nil
	}
	pods, err := c.Core().CoreV1().Pods(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var owned []v1.Pod
	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.UID == job.UID {
				if owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.Name != job.Name || owner.Controller == nil || !*owner.Controller || pod.UID == "" {
					return result, nil
				}
				owned = append(owned, pod)
				break
			}
		}
	}
	for _, pod := range owned {
		if len(result.PodUIDs) < 8 {
			result.PodUIDs = append(result.PodUIDs, string(pod.UID))
		}
	}
	complete, failed := false, false
	for _, condition := range job.Status.Conditions {
		if condition.Status == v1.ConditionTrue {
			complete = complete || condition.Type == batchv1.JobComplete
			failed = failed || condition.Type == batchv1.JobFailed
		}
	}
	if len(owned) > 1 || complete && failed {
		return result, nil
	}
	if !complete && !failed {
		result.Status = api.CNPGBackupRetentionRunning
		result.FailureCode = ""
		return result, nil
	}
	if len(owned) != 1 || job.Status.Active != 0 || job.Status.Terminating != nil && *job.Status.Terminating != 0 || job.Status.UncountedTerminatedPods != nil && (len(job.Status.UncountedTerminatedPods.Succeeded) != 0 || len(job.Status.UncountedTerminatedPods.Failed) != 0) {
		return result, nil
	}
	pod := &owned[0]
	if pod.DeletionTimestamp != nil || pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed || !retentionPodMatches(job, pod) || !retentionContainersStopped(pod) {
		return result, nil
	}
	if failed || !complete || job.Status.Succeeded != 1 || job.Status.Failed != 0 || pod.Status.Phase != v1.PodSucceeded || !retentionContainersSucceeded(pod) {
		return result, nil
	}
	result.FailureCode = api.CNPGFailureRetentionObserve
	deletion, err := retentionLogs(ctx, c, pod.Name, "delete")
	if err != nil || len(deletion) != 0 {
		return result, nil
	}
	catalog, err := retentionLogs(ctx, c, pod.Name, "catalog")
	if err != nil || validateRetentionCatalog(catalog, it) != nil {
		return result, nil
	}
	// Log endpoints use names, not UID preconditions; recheck the owned executor after reading.
	after, err := c.Core().CoreV1().Pods(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	executors := 0
	for _, next := range after.Items {
		for _, owner := range next.OwnerReferences {
			if owner.UID == job.UID {
				executors++
				if next.UID != pod.UID || next.DeletionTimestamp != nil || !reflect.DeepEqual(next.Spec, pod.Spec) || !reflect.DeepEqual(next.Status, pod.Status) || !reflect.DeepEqual(next.OwnerReferences, pod.OwnerReferences) {
					return result, nil
				}
				break
			}
		}
	}
	if executors != 1 {
		return result, nil
	}
	result.Status = api.CNPGBackupRetentionCompleted
	result.FailureCode = ""
	for _, b := range it.Backups[1:] {
		result.RetainedIDs = append(result.RetainedIDs, b.Material.BarmanBackupID)
	}
	result.ExpiredIDs = []string{it.Backups[0].Material.BarmanBackupID}
	return result, nil
}

func retentionJob(it *api.DeployerCommandCNPGBackupRetention, ns string, store map[string]any) *batchv1.Job {
	zero, one := int32(0), int32(1)
	no := false
	yes := true
	labels := map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10), "nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.OperationID}
	// The archive hash binds Secret UID/resource version as well as the supported spec.
	intent := *it
	intent.JobUID = ""
	raw, _ := json.Marshal(intent)
	env := []v1.EnvVar{{Name: "AWS_DEFAULT_REGION", Value: "us-east-1"}}
	// shortcut: supported S3 profile uses the SDK default region; revalidate for a provider requiring a different region.
	config := store["spec"].(map[string]any)["configuration"].(map[string]any)
	selectors := config["s3Credentials"].(map[string]any)
	for i, role := range []string{"accessKeyId", "secretAccessKey"} {
		selector := selectors[role].(map[string]any)
		name := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}[i]
		env = append(env, v1.EnvVar{Name: name, ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{LocalObjectReference: v1.LocalObjectReference{Name: selector["name"].(string)}, Key: selector["key"].(string), Optional: &no}}})
	}
	material := it.Backups[0].Material
	common := []string{"--cloud-provider", "aws-s3", "--endpoint-url", material.Archive.EndpointURL}
	container := func(name, command string, options ...string) v1.Container {
		args := append(append(append([]string{}, common...), options...), material.Archive.DestinationPath, material.ServerName)
		return v1.Container{Name: name, Image: readinessSidecarImage, ImagePullPolicy: v1.PullIfNotPresent, Command: []string{"/venv/bin/" + command}, Args: args, Env: env,
			TerminationMessagePath: "/dev/termination-log", TerminationMessagePolicy: v1.TerminationMessageReadFile,
			SecurityContext: &v1.SecurityContext{AllowPrivilegeEscalation: &no, RunAsNonRoot: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}}}}
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "cnpg-retention-" + it.OperationID, Namespace: ns, Labels: labels, Annotations: map[string]string{"nortezh.io/retention-intent": string(raw)}},
		Spec: batchv1.JobSpec{Parallelism: &one, Completions: &one, BackoffLimit: &zero, Template: v1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever, AutomountServiceAccountToken: &no, ServiceAccountName: "default", DNSPolicy: v1.DNSClusterFirst, SchedulerName: v1.DefaultSchedulerName,
			InitContainers: []v1.Container{container("delete", "barman-cloud-backup-delete", "--retention-policy", "REDUNDANCY 7")},
			Containers:     []v1.Container{container("catalog", "barman-cloud-backup-list", "--format", "json")}}}}}
}

// Ignore only known API defaults/controller labels, not user-supplied execution fields.
func retentionJobMatches(expected, actual *batchv1.Job) bool {
	if actual.Name != expected.Name || actual.Namespace != expected.Namespace || len(actual.OwnerReferences) != 0 || !reflect.DeepEqual(actual.Labels, expected.Labels) || !reflect.DeepEqual(actual.Annotations, expected.Annotations) {
		return false
	}
	spec := actual.Spec.DeepCopy()
	if spec.ManualSelector != nil && *spec.ManualSelector {
		return false
	}
	spec.ManualSelector = nil
	if spec.CompletionMode != nil {
		if *spec.CompletionMode != batchv1.NonIndexedCompletion {
			return false
		}
		spec.CompletionMode = nil
	}
	if spec.Suspend != nil {
		if *spec.Suspend {
			return false
		}
		spec.Suspend = nil
	}
	if spec.PodReplacementPolicy != nil {
		if *spec.PodReplacementPolicy != batchv1.TerminatingOrFailed {
			return false
		}
		spec.PodReplacementPolicy = nil
	}
	if spec.Selector != nil {
		if len(spec.Selector.MatchExpressions) != 0 || len(spec.Selector.MatchLabels) != 1 || spec.Selector.MatchLabels["batch.kubernetes.io/controller-uid"] != string(actual.UID) {
			return false
		}
		spec.Selector = nil
	}
	labels := spec.Template.Labels
	for _, key := range []string{"controller-uid", "batch.kubernetes.io/controller-uid", "job-name", "batch.kubernetes.io/job-name"} {
		if value, ok := labels[key]; ok {
			want := actual.Name
			if key == "controller-uid" || key == "batch.kubernetes.io/controller-uid" {
				want = string(actual.UID)
			}
			if value != want {
				return false
			}
			delete(labels, key)
		}
	}
	normalizeRetentionPod(&spec.Template.Spec)
	want := expected.Spec.DeepCopy()
	normalizeRetentionPod(&want.Template.Spec)
	return reflect.DeepEqual(want, spec)
}

func normalizeRetentionPod(p *v1.PodSpec) {
	if p.TerminationGracePeriodSeconds != nil && *p.TerminationGracePeriodSeconds == 30 {
		p.TerminationGracePeriodSeconds = nil
	}
	if p.SecurityContext != nil && reflect.DeepEqual(p.SecurityContext, &v1.PodSecurityContext{}) {
		p.SecurityContext = nil
	}
	if p.EnableServiceLinks != nil && *p.EnableServiceLinks {
		p.EnableServiceLinks = nil
	}
	if p.PreemptionPolicy != nil && *p.PreemptionPolicy == v1.PreemptLowerPriority {
		p.PreemptionPolicy = nil
	}
}

func retentionPodMatches(job *batchv1.Job, pod *v1.Pod) bool {
	spec := pod.Spec.DeepCopy()
	want := job.Spec.Template.Spec.DeepCopy()
	// Scheduling and admission add these fields without changing the fixed command.
	spec.NodeName = ""
	if spec.Priority != nil && *spec.Priority == 0 && spec.PriorityClassName == "" {
		spec.Priority = nil
	}
	spec.DeprecatedServiceAccount = ""
	want.DeprecatedServiceAccount = ""
	spec.Tolerations = nil
	want.Tolerations = nil
	normalizeRetentionPod(spec)
	normalizeRetentionPod(want)
	return len(pod.Spec.EphemeralContainers) == 0 && reflect.DeepEqual(spec, want)
}

func retentionContainersStopped(pod *v1.Pod) bool {
	if len(pod.Status.InitContainerStatuses) != 1 || len(pod.Status.ContainerStatuses) != 1 || len(pod.Status.EphemeralContainerStatuses) != 0 {
		return false
	}
	return pod.Status.InitContainerStatuses[0].Name == "delete" && pod.Status.ContainerStatuses[0].Name == "catalog" && pod.Status.InitContainerStatuses[0].State.Terminated != nil && pod.Status.ContainerStatuses[0].State.Terminated != nil
}
func retentionContainersSucceeded(pod *v1.Pod) bool {
	for _, s := range append(append([]v1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		if s.RestartCount != 0 || s.State.Terminated == nil || s.State.Terminated.ExitCode != 0 || s.State.Terminated.Signal != 0 {
			return false
		}
	}
	return true
}
func retentionLogs(ctx context.Context, c *k8s.Client, pod, container string) ([]byte, error) {
	limit := int64(retentionLogLimit + 1)
	stream, err := c.Core().CoreV1().Pods(c.DBNamespace()).GetLogs(pod, &v1.PodLogOptions{Container: container, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(stream, limit))
	closeErr := stream.Close()
	if err != nil || closeErr != nil || len(data) > retentionLogLimit {
		return nil, errors.New("CNPG retention logs unavailable")
	}
	return data, nil
}
