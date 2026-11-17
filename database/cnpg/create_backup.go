package cnpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const archiveBindingAnnotation = "nortezh.io/archive-binding"
const reconcileLoopAnnotation = "cnpg.io/reconciliationLoop"

// Native metadata only: references and configuration, never credential values.
type archiveBinding struct {
	Version                                                int
	ID, ProjectID, SecretID, SecretRevision                int64
	Namespace, CreateOperationID, ClusterUID, BootstrapUID string
	Image, StorageClass, ObjectStoreName, ObjectStoreUID   string
	ObjectStoreGeneration, ClusterGeneration               int64
	Archive                                                api.CNPGBackupArchive
	Credentials                                            []readinessCredential
	Bucket, Prefix, Server                                 string
}

func encodeBinding(b *archiveBinding) string {
	raw, _ := json.Marshal(b)
	return string(raw)
}

func bindingHash(b *archiveBinding) string {
	sum := sha256.Sum256([]byte(encodeBinding(b)))
	return hex.EncodeToString(sum[:])
}

func archivePlugins(b *archiveBinding) []any {
	return []any{map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": true,
		"parameters": map[string]any{"barmanObjectName": b.ObjectStoreName, "serverName": b.Server}}}
}

func bindingSnapshot(ctx context.Context, c *k8s.Client, p Profile, name string, it *api.DeployerCommandCNPGCreate) (*archiveBinding, error) {
	if it == nil || it.Valid() != nil || p.AllocationNamespace == "" || p.BackupProfileRevision <= 0 || p.BackupObjectStore == "" {
		return nil, errBackupUnavailable
	}
	app, err := c.Core().CoreV1().Secrets(c.DBNamespace()).Get(ctx, name+"-app", metav1.GetOptions{})
	if err != nil || app.UID == "" || app.DeletionTimestamp != nil || len(app.OwnerReferences) != 0 || app.Immutable == nil || !*app.Immutable || app.Type != v1.SecretTypeBasicAuth {
		return nil, errBackupUnavailable
	}
	for key, value := range map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10), "nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.OperationID, "nortezh.io/secret-id": strconv.FormatInt(it.SecretID, 10), "nortezh.io/secret-revision": strconv.FormatInt(it.SecretRevision, 10)} {
		if app.Labels[key] != value {
			return nil, errBackupUnavailable
		}
	}
	store, err := c.Dynamic().Resource(objectStoreGVR).Namespace(c.DBNamespace()).Get(ctx, p.BackupObjectStore, metav1.GetOptions{})
	if err != nil {
		return nil, errBackupUnavailable
	}
	archive, err := backupArchive(ctx, c, p, store)
	if err != nil {
		return nil, err
	}
	_, secrets, _, ok := readinessStore(ctx, c, p, store)
	if !ok {
		return nil, errBackupUnavailable
	}
	b := &archiveBinding{Version: 1, ID: it.ID, ProjectID: it.ProjectID, SecretID: it.SecretID, SecretRevision: it.SecretRevision,
		Namespace: c.DBNamespace(), CreateOperationID: it.OperationID, BootstrapUID: string(app.UID), Image: p.Image, StorageClass: p.StorageClass,
		ObjectStoreName: store.GetName(), ObjectStoreUID: string(store.GetUID()), ObjectStoreGeneration: store.GetGeneration(), Archive: *archive, Server: name}
	destination, _ := url.Parse(archive.DestinationPath) // backupArchive already validates this boundary.
	b.Bucket = destination.Host
	b.Prefix = strings.TrimPrefix(destination.Path, "/")
	if b.Prefix != "" {
		b.Prefix += "/"
	}
	b.Prefix += name + "/"
	selectors, _, _ := unstructured.NestedMap(store.Object, "spec", "configuration", "s3Credentials")
	for i, role := range []string{"accessKeyId", "secretAccessKey"} {
		ref := selectors[role].(map[string]any)
		b.Credentials = append(b.Credentials, readinessCredential{Name: secrets[i].Name, Key: ref["key"].(string), UID: string(secrets[i].UID), ResourceVersion: secrets[i].ResourceVersion})
	}
	return b, nil
}

// Missing optional prerequisites preserve ordinary provisioning; existing roots never gain an intent.
func newArchiveBinding(ctx context.Context, c *k8s.Client, p Profile, name string, it *api.DeployerCommandCNPGCreate, recovery *api.DeployerCommandCNPGRestore) *archiveBinding {
	b, err := bindingSnapshot(ctx, c, p, name, it)
	if err != nil {
		return nil
	}
	if recovery != nil {
		// The supported recovery profile shares one store and distinct canonical server scopes.
		if recovery.Material.ObjectStoreName != b.ObjectStoreName || recovery.Material.ServerName == name {
			return nil
		}
	}
	if exclusiveArchiveScope(ctx, c, b, "") != nil {
		return nil
	}
	return b
}

func scopesOverlap(a, b *archiveBinding) bool {
	return a.Archive.EndpointURL == b.Archive.EndpointURL && a.Bucket == b.Bucket && (strings.HasPrefix(a.Prefix, b.Prefix) || strings.HasPrefix(b.Prefix, a.Prefix))
}

// Inspect actual destinations, not just ObjectStore reference names. Unknown archivers block enrollment.
func exclusiveArchiveScope(ctx context.Context, c *k8s.Client, b *archiveBinding, ownUID types.UID) error {
	clusters, err := c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errBackupUnavailable
	}
	for _, cluster := range clusters.Items {
		if cluster.GetName() == b.Server && cluster.GetUID() == ownUID && ownUID != "" {
			continue
		}
		plugins, _, e := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
		native, _, ne := unstructured.NestedMap(cluster.Object, "spec", "backup")
		if e != nil || ne != nil || len(native) != 0 {
			return errBackupUnavailable
		}
		for _, value := range plugins {
			plugin, ok := value.(map[string]any)
			if !ok || plugin["name"] != barmanPlugin {
				return errBackupUnavailable
			}
			params, ok := plugin["parameters"].(map[string]any)
			if !ok {
				return errBackupUnavailable
			}
			storeName, ok := params["barmanObjectName"].(string)
			if !ok || storeName == "" {
				return errBackupUnavailable
			}
			server, _ := params["serverName"].(string)
			if server == "" {
				server = cluster.GetName()
			}
			// DNS server names cannot contain path traversal or overlap a canonical trailing slash by accident.
			if strings.ContainsAny(server, "/\\") || server == "." || server == ".." {
				return errBackupUnavailable
			}
			store, e := c.Dynamic().Resource(objectStoreGVR).Namespace(c.DBNamespace()).Get(ctx, storeName, metav1.GetOptions{})
			if e != nil {
				return errBackupUnavailable
			}
			profile := Profile{Image: b.Image, StorageClass: b.StorageClass, BackupObjectStore: storeName, BackupProfileRevision: b.Archive.ProfileRevision}
			archive, e := backupArchive(ctx, c, profile, store)
			if e != nil {
				return errBackupUnavailable
			}
			d, _ := url.Parse(archive.DestinationPath)
			prefix := strings.TrimPrefix(d.Path, "/")
			if prefix != "" {
				prefix += "/"
			}
			other := &archiveBinding{Archive: *archive, Bucket: d.Host, Prefix: prefix + server + "/"}
			if scopesOverlap(b, other) {
				return errBackupUnavailable
			}
		}
	}
	return nil
}

func activateArchiveBinding(ctx context.Context, c *k8s.Client, p Profile, cluster *unstructured.Unstructured, it *api.DeployerCommandCNPGCreate) (*unstructured.Unstructured, error) {
	raw := cluster.GetAnnotations()[archiveBindingAnnotation]
	if raw == "" {
		if _, fenced := cluster.GetAnnotations()[reconcileLoopAnnotation]; fenced {
			return nil, errBackupUnavailable
		}
		return cluster, nil
	} // legacy/unbound roots are not enrolled by a retry.
	var binding archiveBinding
	if len(raw) > 32<<10 || json.Unmarshal([]byte(raw), &binding) != nil || encodeBinding(&binding) != raw || it == nil || cluster.GetUID() == "" || cluster.GetResourceVersion() == "" {
		return nil, errBackupUnavailable
	}
	current, err := bindingSnapshot(ctx, c, p, cluster.GetName(), it)
	if err != nil {
		return nil, err
	}
	current.ClusterUID = binding.ClusterUID
	current.ClusterGeneration = binding.ClusterGeneration
	if !reflect.DeepEqual(current, &binding) || binding.ClusterUID != "" && (binding.ClusterUID != string(cluster.GetUID()) || binding.ClusterGeneration != cluster.GetGeneration()) || exclusiveArchiveScope(ctx, c, &binding, cluster.GetUID()) != nil {
		return nil, errBackupUnavailable
	}
	plugins, _, err := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if err != nil {
		return nil, errBackupUnavailable
	}
	if binding.ClusterUID != "" {
		if _, fenced := cluster.GetAnnotations()[reconcileLoopAnnotation]; fenced || !reflect.DeepEqual(plugins, archivePlugins(&binding)) {
			return nil, errBackupUnavailable
		}
		return cluster, nil
	}
	if len(plugins) != 0 || cluster.GetGeneration() != 1 || binding.ClusterGeneration != 0 || cluster.GetAnnotations()[reconcileLoopAnnotation] != "disabled" {
		return nil, errBackupUnavailable
	}
	binding.ClusterUID = string(cluster.GetUID())
	binding.ClusterGeneration = cluster.GetGeneration() + 1
	annotations := cluster.GetAnnotations()
	annotations[archiveBindingAnnotation] = encodeBinding(&binding)
	delete(annotations, reconcileLoopAnnotation)
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": string(cluster.GetUID())},
		{"op": "test", "path": "/metadata/resourceVersion", "value": cluster.GetResourceVersion()},
		{"op": "add", "path": "/metadata/annotations", "value": annotations},
		{"op": "add", "path": "/spec/plugins", "value": archivePlugins(&binding)},
	})
	updated, err := c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace()).Patch(ctx, cluster.GetName(), types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, errBackupUnavailable
	} // retry reads the same UID/binding after an ACK loss.
	if _, fenced := updated.GetAnnotations()[reconcileLoopAnnotation]; fenced || updated.GetUID() != cluster.GetUID() || updated.GetGeneration() != binding.ClusterGeneration || updated.GetAnnotations()[archiveBindingAnnotation] != encodeBinding(&binding) {
		return nil, errBackupUnavailable
	}
	return updated, nil
}
