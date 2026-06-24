// Package database applies kdb.io managed-database CRs (Postgres/Redis/Mongo) via the dynamic client.
// Shared core here; per-engine spec differences live in the postgres/redis/mongo leaf packages.
package database

import (
	"context"
	"fmt"

	"github.com/Nortezh/api"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/deploys-app/deployer/k8s"
)

// GVR per engine. The CRD plurals are non-standard: plural == singular for all three
// (postgres/redis/mongo), verified from the kdb CRD YAMLs — do NOT let client-go pluralize.
var GVR = map[api.DatabaseType]schema.GroupVersionResource{
	api.DatabaseTypePostgres: {Group: "kdb.io", Version: "v1alpha1", Resource: "postgres"},
	api.DatabaseTypeRedis:    {Group: "kdb.io", Version: "v1alpha1", Resource: "redis"},
	api.DatabaseTypeMongo:    {Group: "kdb.io", Version: "v1alpha1", Resource: "mongo"},
}

// Engine builds the kind-specific spec; everything else (apply/status/delete) is shared. `name` is the
// resolved "<name>-<projectID>", computed once in Apply and passed in so leaves don't re-derive it.
type Engine interface {
	Kind() string // "Postgres" | "Redis" | "Mongo"
	Type() api.DatabaseType
	Spec(it *api.DeployerCommandDatabaseCreate, name string) map[string]any
}

// StorageBlock is the kdb spec.storage block. Storage class defaults to deploys-default unless the
// command overrides it (same pattern as diskCreate).
func StorageBlock(name string, sizeMB int64, class string) map[string]any {
	if class == "" {
		class = "deploys-default"
	}
	return map[string]any{
		"pvcName":      name + "-data",
		"size":         fmt.Sprintf("%dMi", sizeMB),
		"storageClass": class,
		"accessModes":  []any{"ReadWriteOnce"},
	}
}

// Apply does create-or-get of the engine's CR in the DB namespace, then reads status.
// ready iff the operator has finished (phase Running/Ready) AND host+port are populated.
func Apply(ctx context.Context, c *k8s.Client, eng Engine, it *api.DeployerCommandDatabaseCreate) (host string, port int, ready bool, err error) {
	name := k8s.ResourceID(it.ProjectID, it.Name)

	ri := c.Dynamic().Resource(GVR[eng.Type()]).Namespace(c.DBNamespace())
	cur, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kdb.io/v1alpha1",
			"kind":       eng.Kind(),
			"metadata": map[string]any{
				"name":      name,
				"namespace": c.DBNamespace(),
			},
			"spec": eng.Spec(it, name),
		}}
		cur, err = ri.Create(ctx, obj, metav1.CreateOptions{})
	}
	if err != nil {
		return "", 0, false, err
	}

	host, _, _ = unstructured.NestedString(cur.Object, "status", "host")
	p, _, _ := unstructured.NestedInt64(cur.Object, "status", "port")
	phase, _, _ := unstructured.NestedString(cur.Object, "status", "phase")

	// Gate on BOTH endpoint and phase: kdb sets host/port early (before the pod is serving), so
	// host/port alone would flip success while the DB is still starting. Postgres ends at Running,
	// Mongo/Redis at Ready — accept both.
	ready = (phase == "Running" || phase == "Ready") && host != "" && p != 0
	return host, int(p), ready, nil
}

// Delete removes the engine's CR (children cascade via owner refs). NotFound is treated as success.
func Delete(ctx context.Context, c *k8s.Client, t api.DatabaseType, projectID int64, name string) error {
	err := c.Dynamic().Resource(GVR[t]).Namespace(c.DBNamespace()).
		Delete(ctx, k8s.ResourceID(projectID, name), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
