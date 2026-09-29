// Package cnpg is the disposable CNPG control-path spike. Its explicit profile is
// deliberately required: an unconfigured deployer must never route CNPG to kdb.
package cnpg

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database"
	"github.com/deploys-app/deployer/k8s"
	_ "github.com/lib/pq"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var clusterGVR = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
var routeGVR = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutetcps"}

// Profile is local-only: no allocator, public DNS or production retention contract.
type Profile struct {
	HostSuffix, Port, EntryPoint, NodeName, StorageClass, Image string
}

func (p Profile) Enabled() bool {
	port, _ := strconv.Atoi(p.Port)
	return p.HostSuffix != "" && port > 0 && port <= 65535 && p.EntryPoint != "" && p.NodeName != "" && p.StorageClass != "" && p.Image != ""
}

func identity(it *api.DeployerCommandDatabaseCreate) (name string, labels map[string]string) {
	name = k8s.ResourceID(it.ProjectID, it.Name)
	return name, map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10)}
}

func owned(obj *unstructured.Unstructured, labels map[string]string) bool {
	return obj.GetLabels()["nortezh.io/database-id"] == labels["nortezh.io/database-id"]
}

func effectiveConfig(cfg *api.DatabaseConfigPostgres) api.DatabaseConfigPostgres {
	resolved := *cfg
	if resolved.Database == "" {
		resolved.Database = resolved.User // kdb's postgres image defaults POSTGRES_DB to POSTGRES_USER
	}
	return resolved
}

func Apply(ctx context.Context, c *k8s.Client, it *api.DeployerCommandDatabaseCreate, p Profile) (string, int, bool, error) {
	if !p.Enabled() || it.PostgresConfig == nil || it.PostgresConfig.User == "" || it.PostgresConfig.Password == "" || it.StorageSize <= 0 {
		return "", 0, false, errors.New("CNPG spike profile or database configuration missing")
	}
	cfg := effectiveConfig(it.PostgresConfig)
	name, labels := identity(it)
	host := name + p.HostSuffix
	secretName := name + "-app"
	secrets := c.Core().CoreV1().Secrets(c.DBNamespace())
	secret, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		secret, err = secrets.Create(ctx, &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: c.DBNamespace(), Labels: labels},
			Type:       v1.SecretTypeBasicAuth,
			Data:       map[string][]byte{"username": []byte(it.PostgresConfig.User), "password": []byte(it.PostgresConfig.Password)},
		}, metav1.CreateOptions{})
	}
	if err != nil {
		return "", 0, false, errors.New("CNPG credential Secret unavailable")
	}
	if secret.Labels["nortezh.io/database-id"] != labels["nortezh.io/database-id"] ||
		secret.Type != v1.SecretTypeBasicAuth ||
		subtle.ConstantTimeCompare(secret.Data["username"], []byte(it.PostgresConfig.User)) != 1 ||
		subtle.ConstantTimeCompare(secret.Data["password"], []byte(it.PostgresConfig.Password)) != 1 {
		return "", 0, false, errors.New("CNPG credential Secret mismatch")
	}

	clusters := c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace())
	cluster, err := clusters.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		spec := map[string]any{
			"instances": int64(2), "imageName": p.Image,
			"storage":      map[string]any{"storageClass": p.StorageClass, "size": fmt.Sprintf("%dMi", it.StorageSize), "pvcTemplate": map[string]any{"accessModes": []any{"ReadWriteOnce"}}},
			"bootstrap":    map[string]any{"initdb": map[string]any{"database": cfg.Database, "owner": cfg.User, "secret": map[string]any{"name": secretName}}},
			"certificates": map[string]any{"serverAltDNSNames": []any{host}},
		}
		if resources := database.ResourcesBlock(it.Resources); resources != nil {
			spec["resources"] = resources
		}
		cluster, err = clusters.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": name, "namespace": c.DBNamespace(), "labels": map[string]any{"nortezh.io/database-id": labels["nortezh.io/database-id"]}},
			"spec":     spec,
		}}, metav1.CreateOptions{})
	}
	if err != nil || !owned(cluster, labels) {
		return "", 0, false, errors.New("CNPG Cluster unavailable or owned by another database")
	}

	routes := c.Dynamic().Resource(routeGVR).Namespace(c.DBNamespace())
	route, err := routes.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		route, err = routes.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "traefik.io/v1alpha1", "kind": "IngressRouteTCP",
			"metadata": map[string]any{"name": name, "namespace": c.DBNamespace(), "labels": map[string]any{"nortezh.io/database-id": labels["nortezh.io/database-id"], "kdb.io/lb-node": p.NodeName}},
			"spec": map[string]any{"entryPoints": []any{p.EntryPoint}, "routes": []any{map[string]any{
				"match": "HostSNI(`" + host + "`)", "services": []any{map[string]any{"name": name + "-rw", "port": int64(5432)}},
			}}, "tls": map[string]any{"passthrough": true}},
		}}, metav1.CreateOptions{})
	}
	if err != nil || !owned(route, labels) {
		return "", 0, false, errors.New("CNPG TCP route unavailable or owned by another database")
	}

	phase, _, _ := unstructured.NestedString(cluster.Object, "status", "phase")
	if phase != "Cluster in healthy state" {
		return "", 0, false, nil
	}
	ca, err := secrets.Get(ctx, name+"-ca", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", 0, false, nil
	}
	if err != nil || len(ca.Data["ca.crt"]) == 0 {
		return "", 0, false, errors.New("CNPG CA unavailable")
	}
	if err := probe(ctx, host, p.Port, &cfg, ca.Data["ca.crt"]); err != nil {
		return "", 0, false, nil // retry; never disclose password, TLS or raw server errors
	}
	port, _ := strconv.Atoi(p.Port)
	return host, port, true, nil
}

func openDB(host, port string, cfg *api.DatabaseConfigPostgres, ca []byte) (*sql.DB, func(), error) {
	file, err := os.CreateTemp("", "cnpg-ca-*.crt")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.Remove(file.Name()) }
	if _, err = file.Write(ca); err != nil {
		file.Close()
		cleanup()
		return nil, nil, err
	}
	if err = file.Close(); err != nil {
		cleanup()
		return nil, nil, err
	}
	dsn := &url.URL{Scheme: "postgres", User: url.UserPassword(cfg.User, cfg.Password), Host: host + ":" + port, Path: "/" + cfg.Database}
	q := dsn.Query()
	q.Set("sslmode", "verify-full")
	q.Set("sslrootcert", file.Name())
	q.Set("connect_timeout", "3")
	dsn.RawQuery = q.Encode()
	db, err := sql.Open("postgres", dsn.String())
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return db, func() { db.Close(); cleanup() }, nil
}

func probe(ctx context.Context, host, port string, cfg *api.DatabaseConfigPostgres, ca []byte) error {
	db, closeDB, err := openDB(host, port, cfg, ca)
	if err != nil {
		return err
	}
	defer closeDB()
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(queryCtx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	if one != 1 {
		return errors.New("unexpected query result")
	}
	return nil
}

// Delete waits until the Cluster is gone before removing this database's route
// and credential. Disposable PVC cleanup is verified separately in the spike.
func Delete(ctx context.Context, c *k8s.Client, it *api.DeployerCommandDatabaseMetadata) (bool, error) {
	name := k8s.ResourceID(it.ProjectID, it.Name)
	labels := map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10)}
	clusters := c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace())
	cluster, err := clusters.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if !owned(cluster, labels) {
			return false, errors.New("CNPG Cluster ownership mismatch")
		}
		if cluster.GetDeletionTimestamp() == nil {
			if err := clusters.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return false, errors.New("CNPG Cluster deletion failed")
			}
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, errors.New("CNPG Cluster lookup failed")
	}
	routes := c.Dynamic().Resource(routeGVR).Namespace(c.DBNamespace())
	route, err := routes.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if !owned(route, labels) {
			return false, errors.New("CNPG route ownership mismatch")
		}
		if err := routes.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return false, errors.New("CNPG route deletion failed")
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, errors.New("CNPG route lookup failed")
	}
	secrets := c.Core().CoreV1().Secrets(c.DBNamespace())
	secret, err := secrets.Get(ctx, name+"-app", metav1.GetOptions{})
	if err == nil {
		if secret.Labels["nortezh.io/database-id"] != labels["nortezh.io/database-id"] {
			return false, errors.New("CNPG credential ownership mismatch")
		}
		if err := secrets.Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return false, errors.New("CNPG credential deletion failed")
		}
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, errors.New("CNPG credential lookup failed")
	}
	return true, nil
}
