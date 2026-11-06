package cnpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const deleteProgressAnnotation = "nortezh.io/delete-progress"

type deleteStageError struct{ stage string }

func (e *deleteStageError) Error() string { return "CNPG managed cleanup blocked at " + e.stage }
func DeleteFailureStage(err error) string {
	var failure *deleteStageError
	if errors.As(err, &failure) {
		return failure.stage
	}
	return api.CNPGDeleteScope
}

type deleteObservation struct {
	Entry, Shutdown, LogComplete, Terminal, Failed bool
}

type deleteProgress struct {
	InventoryUID, BindingHash, Stage string
	Started                          time.Time
	Pods                             map[string]deleteObservation
}

func readDeleteProgress(cm *v1.ConfigMap, inv *deleteInventory) (*deleteProgress, error) {
	p := &deleteProgress{InventoryUID: string(cm.UID), BindingHash: bindingHash(&inv.Binding), Pods: map[string]deleteObservation{}}
	raw := cm.Annotations[deleteProgressAnnotation]
	if raw == "" {
		return p, nil
	}
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), p) != nil || p.InventoryUID != string(cm.UID) || p.BindingHash != bindingHash(&inv.Binding) || p.Started.IsZero() || p.Started.After(time.Now()) || p.Pods == nil {
		return nil, errDeleteBlocked
	}
	if p.Stage != "stopping" && p.Stage != "stopped" && p.Stage != "archives" && p.Stage != "resources" {
		return nil, errDeleteBlocked
	}
	if len(p.Pods) != len(inv.Executors) {
		return nil, errDeleteBlocked
	}
	for _, e := range inv.Executors {
		o, ok := p.Pods[string(e.UID)]
		if !ok || o.Shutdown && !o.Entry || o.LogComplete && !o.Shutdown || p.Stage != "stopping" && (!o.Entry || !o.Shutdown || !o.LogComplete || !o.Terminal || o.Failed) {
			return nil, errDeleteBlocked
		}
	}
	return p, nil
}

func saveDeleteProgress(ctx context.Context, c *k8s.Client, original *v1.ConfigMap, inv *deleteInventory, next *deleteProgress) error {
	maps := c.Core().CoreV1().ConfigMaps(c.DBNamespace())
	current, err := maps.Get(ctx, original.Name, metav1.GetOptions{})
	if err != nil || current.UID != original.UID || current.Immutable == nil || !*current.Immutable || current.DeletionTimestamp != nil || !reflect.DeepEqual(current.Data, original.Data) || !reflect.DeepEqual(current.Labels, original.Labels) || len(current.BinaryData) != 0 || len(current.OwnerReferences) != 0 {
		return errDeleteBlocked
	}
	old, err := readDeleteProgress(current, inv)
	if err != nil || next.InventoryUID != old.InventoryUID || next.BindingHash != old.BindingHash {
		return errDeleteBlocked
	}
	rank := map[string]int{"": 0, "stopping": 1, "stopped": 2, "archives": 3, "resources": 4}
	if rank[next.Stage] < rank[old.Stage] || rank[next.Stage] > rank[old.Stage]+1 || old.Stage != "" && !next.Started.Equal(old.Started) {
		return errDeleteBlocked
	}
	for uid, before := range old.Pods {
		after := next.Pods[uid]
		if before.Entry && !after.Entry || before.Shutdown && !after.Shutdown || before.LogComplete && !after.LogComplete || before.Terminal && !after.Terminal || before.Failed && !after.Failed {
			return errDeleteBlocked
		}
	}
	raw, _ := json.Marshal(next)
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[deleteProgressAnnotation] = string(raw)
	if _, err := readDeleteProgress(current, inv); err != nil {
		return err
	}
	_, err = maps.Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		// A lost ACK may have committed; only the exact same checkpoint is accepted.
		seen, getErr := maps.Get(ctx, current.Name, metav1.GetOptions{})
		if getErr != nil || seen.UID != current.UID || !reflect.DeepEqual(seen.Data, current.Data) || seen.Annotations[deleteProgressAnnotation] != string(raw) {
			return errDeleteBlocked
		}
	}
	return nil
}

func deleteReceipt(cm *v1.ConfigMap, inv *deleteInventory) *api.CNPGDeleteReceipt {
	return &api.CNPGDeleteReceipt{InventoryUID: string(cm.UID), ClusterUID: inv.Binding.ClusterUID, ArchiveBindingHash: bindingHash(&inv.Binding), WriterStopped: true, ArchiveAbsent: true}
}

// Called only after ApplyDelete succeeds; validates the persisted checkpoint, not a removed root.
func DeleteReceipt(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGDelete) (*api.CNPGDeleteReceipt, error) {
	cm, err := c.Core().CoreV1().ConfigMaps(c.DBNamespace()).Get(ctx, retirementName(deleteName(it)), metav1.GetOptions{})
	if err != nil || cm.Immutable == nil || !*cm.Immutable || cm.DeletionTimestamp != nil || len(cm.OwnerReferences) != 0 || len(cm.Data) != 1 || len(cm.BinaryData) != 0 {
		return nil, errDeleteBlocked
	}
	var inv deleteInventory
	if json.Unmarshal([]byte(cm.Data["inventory"]), &inv) != nil || !inventoryMatches(&inv, it) {
		return nil, errDeleteBlocked
	}
	progress, err := readDeleteProgress(cm, &inv)
	receipt := deleteReceipt(cm, &inv)
	if err != nil || progress.Stage != "resources" || receipt.Valid() != nil {
		return nil, errDeleteBlocked
	}
	return receipt, nil
}

func deleteBindingMatches(inv *deleteInventory, it *api.DeployerCommandCNPGDelete) bool {
	b := &inv.Binding
	if len(inv.Objects) < 3 || b.Version != 1 || b.ID != it.ID || b.ProjectID != it.ProjectID || b.SecretID != it.SecretID || b.SecretRevision != it.SecretRevision || b.CreateOperationID != it.CreateOperationID || b.Namespace == "" || b.ClusterUID != string(inv.Objects[0].UID) || b.BootstrapUID != string(inv.Objects[2].UID) || b.Server != deleteName(it) || b.ObjectStoreGeneration < 1 || b.ClusterGeneration < 2 || len(b.Credentials) != 2 || b.Archive.Valid() != nil {
		return false
	}
	destination, err := url.Parse(b.Archive.DestinationPath)
	if err != nil {
		return false
	}
	prefix := strings.TrimPrefix(destination.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return b.Bucket == destination.Host && b.Prefix == prefix+b.Server+"/" && len(inv.RootSpecHash) == 64
}

func knownDeleteBackup(backup *unstructured.Unstructured, inv *deleteInventory) bool {
	known := false
	for _, obj := range inv.Objects {
		known = known || obj.Kind == "Backup" && obj.Name == backup.GetName() && obj.UID == backup.GetUID()
	}
	return known && validDeleteBackup(backup, inv)
}

func validDeleteBackup(backup *unstructured.Unstructured, inv *deleteInventory) bool {
	return backup.GetUID() != "" && ownerUID(backup, inv.Objects[0].UID) &&
		backup.GetLabels()["nortezh.io/database-id"] == strconv.FormatInt(inv.Command.ID, 10) && backup.GetLabels()["nortezh.io/project-id"] == strconv.FormatInt(inv.Command.ProjectID, 10) &&
		str(backup, "spec", "cluster", "name") == deleteName(&inv.Command) && str(backup, "spec", "method") == "plugin" && str(backup, "spec", "pluginConfiguration", "name") == barmanPlugin &&
		(str(backup, "status", "phase") == "completed" || str(backup, "status", "phase") == "failed")
}

func captureDeleteBackups(ctx context.Context, c *k8s.Client, root *unstructured.Unstructured, inv *deleteInventory) error {
	backups, err := c.Dynamic().Resource(backupGVR).Namespace(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, backup := range backups.Items {
		if ownerUID(&backup, root.GetUID()) || str(&backup, "spec", "cluster", "name") == root.GetName() {
			if backup.GetDeletionTimestamp() != nil || !validDeleteBackup(&backup, inv) {
				return errDeleteBlocked
			}
			inv.Objects = append(inv.Objects, deleteObject{"Backup", backup.GetName(), backup.GetUID()})
		}
	}
	return nil
}

func verifyDeleteStopped(ctx context.Context, c *k8s.Client, inv *deleteInventory, retiring ...bool) error {
	root, err := c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace()).Get(ctx, deleteName(&inv.Command), metav1.GetOptions{})
	if err != nil || root.GetUID() != inv.Objects[0].UID || root.GetDeletionTimestamp() != nil && (len(retiring) != 1 || !retiring[0]) || root.GetAnnotations()["cnpg.io/hibernation"] != "on" {
		return errDeleteBlocked
	}
	conditions, _, err := unstructured.NestedSlice(root.Object, "status", "conditions")
	if err != nil {
		return errDeleteBlocked
	}
	hibernated := false
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			return errDeleteBlocked
		}
		hibernated = hibernated || condition["type"] == "cnpg.io/hibernation" && condition["status"] == "True" && condition["reason"] == "Hibernated"
	}
	if !hibernated {
		return errDeleteBlocked
	}
	pods, err := c.Core().CoreV1().Pods(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, pod := range pods.Items {
		if pod.Labels["cnpg.io/cluster"] == root.GetName() || ownerUID(&pod, root.GetUID()) {
			for _, container := range pod.Spec.Containers {
				if container.Name == "postgres" {
					return errDeleteBlocked
				}
			}
			if !terminalDeletePod(&pod) {
				return errDeleteBlocked
			}
		}
	}
	return nil
}

func terminalDeletePod(pod *v1.Pod) bool {
	if pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed {
		return false
	}
	if len(pod.Status.ContainerStatuses) != len(pod.Spec.Containers) || len(pod.Status.InitContainerStatuses) != len(pod.Spec.InitContainers) || len(pod.Spec.EphemeralContainers) != 0 {
		return false
	}
	for _, status := range append(append([]v1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...) {
		if status.State.Terminated == nil {
			return false
		}
	}
	return true
}

func deleteRootSpecHash(root *unstructured.Unstructured) string {
	raw, _ := json.Marshal(root.Object["spec"])
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func deleteRoot(ctx context.Context, c *k8s.Client, inv *deleteInventory) (*unstructured.Unstructured, error) {
	root, err := c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace()).Get(ctx, inv.Binding.Server, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil || root.GetUID() != inv.Objects[0].UID || root.GetGeneration() != inv.Binding.ClusterGeneration || root.GetAnnotations()[archiveBindingAnnotation] != encodeBinding(&inv.Binding) || root.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(inv.Command.ID, 10) {
		return nil, errDeleteBlocked
	}
	plugins, _, e := unstructured.NestedSlice(root.Object, "spec", "plugins")
	if e != nil || deleteRootSpecHash(root) != inv.RootSpecHash || !reflect.DeepEqual(plugins, archivePlugins(&inv.Binding)) || str(root, "spec", "imageName") != inv.Binding.Image || str(root, "spec", "storage", "storageClass") != inv.Binding.StorageClass {
		return nil, errDeleteBlocked
	}
	return root, nil
}
