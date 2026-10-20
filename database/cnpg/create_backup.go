package cnpg

import (
	"context"

	"github.com/deploys-app/deployer/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Optional backup prerequisites must not prevent ordinary database creation.
// This is configuration preflight, not proof of plugin/WAL runtime readiness.
func createBackupPlugins(ctx context.Context, c *k8s.Client, p Profile, name string) []any {
	if p.AllocationNamespace == "" || p.BackupProfileRevision <= 0 || p.BackupObjectStore == "" {
		return nil
	}
	store, err := c.Dynamic().Resource(objectStoreGVR).Namespace(c.DBNamespace()).Get(ctx, p.BackupObjectStore, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	if _, _, _, supported := readinessStore(ctx, c, p, store); !supported {
		return nil
	}
	return []any{map[string]any{
		"name": barmanPlugin, "enabled": true, "isWALArchiver": true,
		"parameters": map[string]any{"barmanObjectName": p.BackupObjectStore, "serverName": name},
	}}
}
