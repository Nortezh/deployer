package cnpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const barmanPlugin = "barman-cloud.cloudnative-pg.io"

var (
	backupGVR            = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "backups"}
	objectStoreGVR       = schema.GroupVersionResource{Group: "barmancloud.cnpg.io", Version: "v1", Resource: "objectstores"}
	errBackupUnavailable = errors.New("CNPG backup unavailable or mismatched")
)

// ApplyBackup never changes source/plugin configuration or deletes backup/WAL objects.
// The approved location fixture must already have stock WAL archiving configured.
func ApplyBackup(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGBackup, p Profile) (*api.DeployerSetResultCNPGBackup, error) {
	if c == nil || it == nil || it.Valid() != nil || p.AllocationNamespace == "" || !p.Enabled() {
		return nil, errBackupUnavailable
	}
	name := createName(&api.DeployerCommandCNPGCreate{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity})
	ns := c.DBNamespace()
	cluster, err := c.Dynamic().Resource(clusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || cluster.GetUID() == "" || cluster.GetDeletionTimestamp() != nil || cluster.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) ||
		it.SourceClusterUID != "" && string(cluster.GetUID()) != it.SourceClusterUID {
		return nil, errBackupUnavailable
	}
	mode := backupBootstrap(cluster, name, p.BackupObjectStore)
	if str(cluster, "spec", "imageName") != p.Image || str(cluster, "spec", "storage", "storageClass") != p.StorageClass || mode == "" {
		return nil, errBackupUnavailable
	}
	// Bind the source root operation using the existing immutable bootstrap Secret.
	app, err := c.Core().CoreV1().Secrets(ns).Get(ctx, name+"-app", metav1.GetOptions{})
	if err != nil || app.Name != name+"-app" || app.Namespace != ns || app.UID == "" || len(app.OwnerReferences) != 0 || app.DeletionTimestamp != nil || app.Immutable == nil || !*app.Immutable || app.Type != v1.SecretTypeBasicAuth || len(app.Data) != 2 || len(app.StringData) != 0 || len(app.Data["password"]) == 0 || len(app.Data["username"]) == 0 ||
		str(cluster, "spec", "bootstrap", mode, "owner") != string(app.Data["username"]) || str(cluster, "spec", "bootstrap", mode, "database") == "" ||
		app.Labels["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) || app.Labels["nortezh.io/project-id"] != strconv.FormatInt(it.ProjectID, 10) || app.Labels["nortezh.io/operation-id"] != it.CreateOperationID {
		return nil, errBackupUnavailable
	}
	_, err = c.Core().CoreV1().ConfigMaps(ns).Get(ctx, retirementName(name), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return nil, errBackupUnavailable
	}
	if native, exists, err := unstructured.NestedMap(cluster.Object, "spec", "backup"); err != nil || exists && len(native) != 0 {
		return nil, errBackupUnavailable
	}
	plugins, _, err := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if err != nil || len(plugins) != 1 {
		return nil, errBackupUnavailable
	}
	plugin, ok := plugins[0].(map[string]any)
	if !ok || !onlyKeys(plugin, "name", "enabled", "isWALArchiver", "parameters") || plugin["name"] != barmanPlugin || plugin["isWALArchiver"] != true {
		return nil, errBackupUnavailable
	}
	if enabled, exists := plugin["enabled"]; exists && enabled != true {
		return nil, errBackupUnavailable
	}
	params, ok := plugin["parameters"].(map[string]any)
	if !ok || !onlyKeys(params, "barmanObjectName", "serverName") {
		return nil, errBackupUnavailable
	}
	pluginObject := &unstructured.Unstructured{Object: plugin}
	storeName := str(pluginObject, "parameters", "barmanObjectName")
	serverName := str(pluginObject, "parameters", "serverName")
	if serverName == "" {
		serverName = name
	}
	if storeName == "" || mode == "recovery" && serverName != name {
		return nil, errBackupUnavailable
	}
	store, err := c.Dynamic().Resource(objectStoreGVR).Namespace(ns).Get(ctx, storeName, metav1.GetOptions{})
	if err != nil || store.GetUID() == "" || store.GetDeletionTimestamp() != nil || str(store, "spec", "retentionPolicy") != "" {
		return nil, errBackupUnavailable // count-based MVP must not activate automatic physical rotation
	}
	annotations := map[string]string{
		"nortezh.io/backup-object-store":     storeName,
		"nortezh.io/backup-object-store-uid": string(store.GetUID()),
		"nortezh.io/backup-server":           serverName,
		"nortezh.io/backup-image":            p.Image,
		"nortezh.io/backup-storage-class":    p.StorageClass,
	}
	// Pin bootstrap/Secret identity across later polls without persisting credential data.
	bootstrap, _, _ := unstructured.NestedMap(cluster.Object, "spec", "bootstrap")
	external, _, _ := unstructured.NestedSlice(cluster.Object, "spec", "externalClusters")
	sourceJSON, err := json.Marshal(struct {
		Bootstrap map[string]any
		External  []any
		SecretUID string
	}{bootstrap, external, string(app.UID)})
	if err != nil {
		return nil, errBackupUnavailable
	}
	sourceHash := sha256.Sum256(sourceJSON)
	sourceBinding := hex.EncodeToString(sourceHash[:])
	backupName := "cnpg-backup-" + it.OperationID
	backups := c.Dynamic().Resource(backupGVR).Namespace(ns)
	backup, err := backups.Get(ctx, backupName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && it.BackupUID == "" {
		if serverName != name {
			return nil, errBackupUnavailable
		}
		archive, archiveErr := backupArchive(ctx, c, p, store)
		if archiveErr != nil {
			return nil, archiveErr
		}
		encoded, _ := json.Marshal(archive)
		annotations["nortezh.io/backup-archive"] = string(encoded)
		annotations["nortezh.io/backup-source"] = sourceBinding
		nativeAnnotations := make(map[string]any, len(annotations))
		for key, value := range annotations {
			nativeAnnotations[key] = value
		}
		backup, err = backups.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup",
			"metadata": map[string]any{"name": backupName, "namespace": ns, "annotations": nativeAnnotations,
				"labels":          map[string]any{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10), "nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.OperationID},
				"ownerReferences": []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": name, "uid": string(cluster.GetUID())}}},
			// On a primary, stock pg_backup_stop switches and waits for required WAL.
			"spec": map[string]any{"cluster": map[string]any{"name": name}, "target": "primary", "method": "plugin", "pluginConfiguration": map[string]any{"name": barmanPlugin}},
		}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			backup, err = backups.Get(ctx, backupName, metav1.GetOptions{})
		}
	}
	if err != nil || backup.GetUID() == "" || backup.GetDeletionTimestamp() != nil || !ownerUID(backup, cluster.GetUID()) ||
		backup.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) || backup.GetLabels()["nortezh.io/project-id"] != strconv.FormatInt(it.ProjectID, 10) || backup.GetLabels()["nortezh.io/operation-id"] != it.OperationID ||
		str(backup, "spec", "cluster", "name") != name || str(backup, "spec", "method") != "plugin" || str(backup, "spec", "pluginConfiguration", "name") != barmanPlugin ||
		it.BackupUID != "" && string(backup.GetUID()) != it.BackupUID {
		return nil, errBackupUnavailable
	}
	var archive *api.CNPGBackupArchive
	if binding := backup.GetAnnotations()["nortezh.io/backup-archive"]; binding != "" {
		if backup.GetAnnotations()["nortezh.io/backup-source"] != sourceBinding {
			return nil, errBackupUnavailable
		}
		archive, err = backupArchive(ctx, c, p, store)
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(archive)
		if string(encoded) != binding {
			return nil, errBackupUnavailable
		}
	}
	for key, value := range annotations {
		if backup.GetAnnotations()[key] != value {
			return nil, errBackupUnavailable
		}
	}
	result := &api.DeployerSetResultCNPGBackup{DeployerCommandCNPGBackup: *it, Status: api.CNPGBackupRunning}
	result.SourceClusterUID, result.BackupUID = string(cluster.GetUID()), string(backup.GetUID())
	switch str(backup, "status", "phase") {
	case "completed":
		major, _, _ := unstructured.NestedInt64(backup.Object, "status", "majorVersion")
		material := &api.CNPGBackupMaterial{BarmanBackupID: str(backup, "status", "backupId"), MajorVersion: int(major),
			Image: p.Image, StorageClass: p.StorageClass, ObjectStoreName: storeName, ObjectStoreUID: string(store.GetUID()), ServerName: serverName,
			EndWAL: str(backup, "status", "endWal"), EndLSN: str(backup, "status", "endLSN"), Archive: archive}
		// Missing native metadata retains the checkpoint/guard, never guessed completion.
		if material.Valid() == nil {
			result.Status, result.Material = api.CNPGBackupCompleted, material
		}
	case "failed":
		// CNPG may omit stoppedAt on a terminal failure.
		result.Status, result.FailureCode = api.CNPGBackupFailed, api.CNPGFailureBackup
	}
	if result.Valid() != nil {
		return nil, errBackupUnavailable
	}
	return result, nil
}

func backupArchive(ctx context.Context, c *k8s.Client, p Profile, store *unstructured.Unstructured) (*api.CNPGBackupArchive, error) {
	hash, _, _, ok := readinessStore(ctx, c, p, store)
	if !ok || p.BackupProfileRevision <= 0 {
		return nil, errBackupUnavailable
	}
	archive := &api.CNPGBackupArchive{
		EndpointURL:     strings.TrimRight(str(store, "spec", "configuration", "endpointURL"), "/"),
		DestinationPath: strings.TrimRight(str(store, "spec", "configuration", "destinationPath"), "/"),
		ProfileRevision: p.BackupProfileRevision, ConfigurationHash: hash,
	}
	if archive.Valid() != nil {
		return nil, errBackupUnavailable
	}
	return archive, nil
}

func str(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value
}
