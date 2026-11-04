package cnpg

import (
	"context"
	"database/sql"
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

// A CNPG Ready/ContinuousArchiving condition alone can predate plugin setup.
// Require a successful real archive after relevant configuration/pod changes.
const readinessWALQuery = `SELECT CASE WHEN NOT pg_is_in_recovery()
 AND current_setting('archive_mode') IN ('on','always')
 AND current_setting('archive_command') LIKE '/controller/manager wal-archive%'
 AND archived_count > 0 AND last_archived_wal ~ '^[0-9A-F]{24}$'
 AND left(last_archived_wal,8) <> '00000000' AND last_archived_time IS NOT NULL
 AND (last_failed_time IS NULL OR last_failed_time < last_archived_time)
 THEN floor(extract(epoch FROM last_archived_time)*1000000)::bigint ELSE 0 END FROM pg_catalog.pg_stat_archiver`

// ObserveBackupReadiness makes Kubernetes GETs and one authenticated read-only
// SQL query. Uncertainty disables admission; it never fails/unlocks a Backup.
func ObserveBackupReadiness(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGBackupReadiness, p Profile) (*api.DeployerSetResultCNPGBackupReadiness, error) {
	if c == nil || it == nil || it.Valid() != nil {
		return nil, errBackupUnavailable
	}
	result := &api.DeployerSetResultCNPGBackupReadiness{DeployerCommandCNPGBackupReadiness: *it, FailureCode: api.CNPGFailureBackupReadiness}
	if p.AllocationNamespace == "" || !p.Enabled() || p.BackupProfileRevision <= 0 || p.BackupProfileRevision != it.ProfileRevision || p.BackupObjectStore == "" {
		return result, nil
	}
	name := createName(&api.DeployerCommandCNPGCreate{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity})
	ns := c.DBNamespace()
	if !readinessNotRetiring(ctx, c, name) {
		return result, nil
	}
	cluster, err := c.Dynamic().Resource(clusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || cluster.GetName() != name || cluster.GetNamespace() != ns || cluster.GetUID() == "" || cluster.GetDeletionTimestamp() != nil || cluster.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) || !readinessClusterHealthy(cluster) {
		return result, nil
	}
	mode := backupBootstrap(cluster, name, p.BackupObjectStore)
	if str(cluster, "spec", "imageName") != p.Image || str(cluster, "spec", "storage", "storageClass") != p.StorageClass || mode == "" {
		return result, nil
	}
	since, ok := configurationTime(cluster, cluster.GetGeneration() == 1, "f:spec")
	if !ok || cluster.GetGeneration() < 1 {
		return result, nil
	}
	if backup, exists, err := unstructured.NestedMap(cluster.Object, "spec", "backup"); err != nil || exists && len(backup) != 0 {
		return result, nil
	}
	plugins, _, err := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if err != nil || len(plugins) != 1 {
		return result, nil
	}
	plugin, ok := plugins[0].(map[string]any)
	if !ok || !onlyKeys(plugin, "name", "enabled", "isWALArchiver", "parameters") || plugin["name"] != barmanPlugin || plugin["isWALArchiver"] != true {
		return result, nil
	}
	if enabled, exists := plugin["enabled"]; exists && enabled != true {
		return result, nil
	}
	params, ok := plugin["parameters"].(map[string]any)
	if !ok || !onlyKeys(params, "barmanObjectName", "serverName") || params["barmanObjectName"] != p.BackupObjectStore {
		return result, nil
	}
	if server, exists := params["serverName"]; exists && server != name {
		return result, nil
	}
	app, err := c.Core().CoreV1().Secrets(ns).Get(ctx, name+"-app", metav1.GetOptions{})
	if err != nil || app.Name != name+"-app" || app.Namespace != ns || app.UID == "" || app.ResourceVersion == "" || app.DeletionTimestamp != nil || len(app.OwnerReferences) != 0 || app.Immutable == nil || !*app.Immutable || app.Type != v1.SecretTypeBasicAuth || len(app.Data) != 2 || len(app.StringData) != 0 || len(app.Data["username"]) == 0 || len(app.Data["password"]) == 0 {
		return result, nil
	}
	for key, expected := range map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10), "nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.OperationID} {
		if app.Labels[key] != expected {
			return result, nil
		}
	}
	cfg := &api.DatabaseConfigPostgres{User: string(app.Data["username"]), Password: string(app.Data["password"]), Database: str(cluster, "spec", "bootstrap", mode, "database")}
	if cfg.Database == "" || str(cluster, "spec", "bootstrap", mode, "owner") != cfg.User {
		return result, nil
	}
	store, err := c.Dynamic().Resource(objectStoreGVR).Namespace(ns).Get(ctx, p.BackupObjectStore, metav1.GetOptions{})
	if err != nil {
		return result, nil
	}
	hash, credentials, changed, ok := readinessStore(ctx, c, p, store)
	if !ok {
		return result, nil
	}
	if changed.After(since) {
		since = changed
	}
	primary := str(cluster, "status", "currentPrimary")
	pod, err := c.Core().CoreV1().Pods(ns).Get(ctx, primary, metav1.GetOptions{})
	if err != nil || pod.Name != primary || pod.Namespace != ns || pod.UID == "" || pod.DeletionTimestamp != nil || !metav1.IsControlledBy(pod, cluster) || !readinessPodHealthy(pod, p.Image) {
		return result, nil
	}
	if pod.CreationTimestamp.Time.IsZero() || pod.CreationTimestamp.Time.After(time.Now()) {
		return result, nil
	}
	if pod.CreationTimestamp.Time.After(since) {
		since = pod.CreationTimestamp.Time
	}
	ca, err := c.Core().CoreV1().Secrets(ns).Get(ctx, name+"-ca", metav1.GetOptions{})
	if err != nil || ca.Name != name+"-ca" || ca.Namespace != ns || ca.UID == "" || ca.ResourceVersion == "" || ca.DeletionTimestamp != nil || !ownerUID(ca, cluster.GetUID()) || len(ca.Data["ca.crt"]) == 0 {
		return result, nil
	}
	ep, ok := readinessEndpoint(ctx, c, p, name, it.ID)
	if !ok {
		return result, nil
	}
	db, closeDB, err := openDB(ep.Host, ep.Port, cfg, ca.Data["ca.crt"])
	if err != nil {
		return result, nil
	}
	defer closeDB()
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := db.BeginTx(queryCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, nil
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(queryCtx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return result, nil
	}
	var archived int64
	if err := tx.QueryRowContext(queryCtx, readinessWALQuery).Scan(&archived); err != nil || archived <= 0 {
		return result, nil
	}
	archivedAt := time.UnixMicro(archived)
	// Kubernetes metadata times have second precision; do not reuse success
	// from the same second as a configuration change.
	if archivedAt.Before(since.Add(time.Second)) || archivedAt.After(time.Now()) {
		return result, nil
	}
	// Reject replacements/config changes observed across the probe; these reads
	// are a bounded snapshot, not a late-writer fence or future availability SLA.
	after, err := c.Dynamic().Resource(clusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || after.GetUID() != cluster.GetUID() || after.GetGeneration() != cluster.GetGeneration() || after.GetDeletionTimestamp() != nil || after.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) || str(after, "status", "currentPrimary") != primary || !readinessClusterHealthy(after) {
		return result, nil
	}
	currentStore, err := c.Dynamic().Resource(objectStoreGVR).Namespace(ns).Get(ctx, p.BackupObjectStore, metav1.GetOptions{})
	if err != nil || currentStore.GetUID() != store.GetUID() || currentStore.GetGeneration() != store.GetGeneration() || currentStore.GetDeletionTimestamp() != nil {
		return result, nil
	}
	for _, secret := range append(credentials, app, ca) {
		current, err := c.Core().CoreV1().Secrets(ns).Get(ctx, secret.Name, metav1.GetOptions{})
		if err != nil || current.UID != secret.UID || current.ResourceVersion != secret.ResourceVersion || current.DeletionTimestamp != nil {
			return result, nil
		}
	}
	currentPod, err := c.Core().CoreV1().Pods(ns).Get(ctx, primary, metav1.GetOptions{})
	if err != nil || currentPod.UID != pod.UID || currentPod.DeletionTimestamp != nil || !metav1.IsControlledBy(currentPod, after) || !readinessPodHealthy(currentPod, p.Image) || !readinessNotRetiring(ctx, c, name) {
		return result, nil
	}
	result.Available, result.FailureCode = true, ""
	result.Evidence = &api.CNPGBackupReadinessEvidence{SourceClusterUID: string(cluster.GetUID()), ObjectStoreName: store.GetName(), ObjectStoreUID: string(store.GetUID()), ServerName: name, ConfigurationHash: hash}
	if result.Valid() != nil {
		return &api.DeployerSetResultCNPGBackupReadiness{DeployerCommandCNPGBackupReadiness: *it, FailureCode: api.CNPGFailureBackupReadiness}, nil
	}
	return result, nil
}

func readinessNotRetiring(ctx context.Context, c *k8s.Client, name string) bool {
	_, err := c.Core().CoreV1().ConfigMaps(c.DBNamespace()).Get(ctx, retirementName(name), metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}

func readinessClusterHealthy(cluster *unstructured.Unstructured) bool {
	if str(cluster, "status", "phase") != "Cluster in healthy state" || str(cluster, "status", "currentPrimary") == "" {
		return false
	}
	for _, field := range [][]string{{"spec", "instances"}, {"status", "readyInstances"}} {
		count, _, err := unstructured.NestedInt64(cluster.Object, field...)
		if err != nil || count != 2 {
			return false
		}
	}
	conditions, _, err := unstructured.NestedSlice(cluster.Object, "status", "conditions")
	if err != nil {
		return false
	}
	seen := map[string]bool{}
	for _, value := range conditions {
		condition, ok := value.(map[string]any)
		if !ok {
			return false
		}
		kind, _ := condition["type"].(string)
		if kind != "Ready" && kind != "ContinuousArchiving" {
			continue
		}
		if seen[kind] || condition["status"] != "True" {
			return false
		}
		if observed, exists := condition["observedGeneration"]; exists && observed != cluster.GetGeneration() {
			return false
		}
		if kind == "ContinuousArchiving" && condition["reason"] != "ContinuousArchivingSuccess" {
			return false
		}
		seen[kind] = true
	}
	loaded, _, err := unstructured.NestedSlice(cluster.Object, "status", "pluginStatus")
	if err != nil || !seen["Ready"] || !seen["ContinuousArchiving"] {
		return false
	}
	found := false
	for _, value := range loaded {
		plugin, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if plugin["name"] == barmanPlugin {
			version, _ := plugin["version"].(string)
			if found || strings.TrimPrefix(version, "v") != readinessPluginVersion {
				return false
			}
			found = true
		}
	}
	return found
}

func readinessPodHealthy(pod *v1.Pod, image string) bool {
	if pod.Status.Phase != v1.PodRunning {
		return false
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		ready = ready || condition.Type == v1.PodReady && condition.Status == v1.ConditionTrue
	}
	if !ready {
		return false
	}
	postgresImage, pluginImage, pluginFound := false, false, false
	pluginStatuses := pod.Status.ContainerStatuses
	for _, container := range pod.Spec.Containers {
		postgresImage = postgresImage || container.Name == "postgres" && container.Image == image
		if container.Name == "plugin-barman-cloud" {
			if pluginFound {
				return false
			}
			pluginFound, pluginImage = true, container.Image == readinessSidecarImage
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if container.Name != "plugin-barman-cloud" {
			continue
		}
		if pluginFound || container.RestartPolicy == nil || *container.RestartPolicy != v1.ContainerRestartPolicyAlways {
			return false
		}
		pluginFound, pluginImage = true, container.Image == readinessSidecarImage
		pluginStatuses = pod.Status.InitContainerStatuses
	}
	if !postgresImage || !pluginImage {
		return false
	}
	postgres, plugin := false, false
	for _, status := range pod.Status.ContainerStatuses {
		postgres = postgres || status.Name == "postgres" && status.Ready && status.State.Running != nil
	}
	for _, status := range pluginStatuses {
		plugin = plugin || status.Name == "plugin-barman-cloud" && status.Ready && status.State.Running != nil
	}
	return postgres && plugin
}

func readinessEndpoint(ctx context.Context, c *k8s.Client, p Profile, name string, id int64) (endpoint, bool) {
	cm, err := c.Core().CoreV1().ConfigMaps(p.AllocationNamespace).Get(ctx, allocationMap, metav1.GetOptions{})
	if err != nil || cm.UID == "" || cm.DeletionTimestamp != nil {
		return endpoint{}, false
	}
	owner := allocationOwner(c.DBNamespace(), name, id)
	key := ""
	for k, v := range cm.Data {
		if v == owner {
			if key != "" {
				return endpoint{}, false
			}
			key = k
		}
	}
	nodeName, port, ok := splitDeleteAllocation(key)
	if !ok {
		return endpoint{}, false
	}
	lo, hi, err := portBounds(p.PortRange)
	number, _ := strconv.Atoi(port)
	if err != nil || number < lo || number > hi {
		return endpoint{}, false
	}
	node, err := c.Core().CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil || node.UID == "" || node.DeletionTimestamp != nil || node.Labels["kdb/role"] != "lb" {
		return endpoint{}, false
	}
	host := node.Annotations["kdb.io/host"]
	if host == "" {
		for _, addr := range node.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				host = addr.Address
				break
			}
		}
	}
	return endpoint{Node: nodeName, Host: host, Port: port}, host != ""
}
