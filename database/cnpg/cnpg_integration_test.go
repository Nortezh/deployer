package cnpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Run only against the isolated k3d cluster with kubectl proxy on localhost:8001.
func TestDisposableCNPG(t *testing.T) {
	if os.Getenv("CNPG_TEST_PASSWORD") == "" {
		t.Skip("requires an isolated kubectl proxy and CNPG_TEST_PASSWORD")
	}
	ctx := context.Background()
	client, err := k8s.NewLocalClient("nortezh-local")
	if err != nil {
		t.Fatal(err)
	}
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	id, _ := strconv.ParseInt(hex.EncodeToString(entropy[:7]), 16, 64)
	it := &api.DeployerCommandDatabaseCreate{
		ID: id, ProjectID: 99007, Name: "slice07-cnpg", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG,
		PostgresConfig: &api.DatabaseConfigPostgres{User: "app", Password: os.Getenv("CNPG_TEST_PASSWORD"), Database: "app"},
		StorageSize:    1024,
	}
	p := Profile{HostSuffix: ".localhost", Port: "6109", EntryPoint: "tcp-6109", NodeName: "k3d-nortezh-slice03-agent-0",
		StorageClass: "local-path", Image: "ghcr.io/cloudnative-pg/postgresql:17.5@sha256:b1deeed2aa998b2f381e39c5cadb9ec06127708c8bd62965743af19abf21628f"}
	if _, _, ready, err := Apply(ctx, client, it, Profile{}); err == nil || ready {
		t.Fatal("unconfigured CNPG was accepted")
	}
	defer func() {
		meta := &api.DeployerCommandDatabaseMetadata{ID: it.ID, ProjectID: it.ProjectID, Name: it.Name, Type: it.Type, Provider: it.Provider}
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			gone, err := Delete(ctx, client, meta)
			if err != nil {
				t.Errorf("CNPG cleanup: %v", err)
				break
			}
			if gone {
				return
			}
		}
		t.Error("CNPG cleanup timed out")
	}()
	var host string
	var port int
	ready := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		host, port, ready, err = Apply(ctx, client, it, p)
		if err != nil {
			t.Fatalf("CNPG apply: %v", err) // errors returned by Apply contain no secrets
		}
		if ready {
			break
		}
	}
	if !ready || host == "" || port != 6109 {
		t.Fatal("CNPG did not become query/TLS ready")
	}
	// A real verified endpoint must reject the wrong trust root.
	if err := probe(ctx, host, p.Port, it.PostgresConfig, []byte("not a certificate")); err == nil {
		t.Fatal("wrong CA passed TLS verification")
	}
	cluster, err := client.Dynamic().Resource(clusterGVR).Namespace(client.DBNamespace()).Get(ctx, k8s.ResourceID(it.ProjectID, it.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cluster.GetName() == "" {
		t.Fatal("CNPG cluster missing")
	}
	primary, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "status", "readyInstances")
	if primary == "" || instances != 2 {
		t.Fatalf("failover requires two ready instances: primary=%q ready=%d", primary, instances)
	}
	ca, err := client.Core().CoreV1().Secrets(client.DBNamespace()).Get(ctx, cluster.GetName()+"-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal("CA Secret missing")
	}
	db, closeDB, err := openDB(host, p.Port, it.PostgresConfig, ca.Data["ca.crt"])
	if err != nil {
		t.Fatal("cannot open verified connection")
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE slice07_marker (value bigint NOT NULL)"); err != nil {
		closeDB()
		t.Fatal("marker create failed")
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO slice07_marker VALUES ($1)", id); err != nil {
		closeDB()
		t.Fatal("marker write failed")
	}
	closeDB()
	start := time.Now()
	if err := client.Core().CoreV1().Pods(client.DBNamespace()).Delete(ctx, primary, metav1.DeleteOptions{}); err != nil {
		t.Fatal("primary Pod deletion failed")
	}
	promoted := false
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		current, err := client.Dynamic().Resource(clusterGVR).Namespace(client.DBNamespace()).Get(ctx, cluster.GetName(), metav1.GetOptions{})
		if err != nil {
			t.Fatal("CNPG cluster disappeared during failover")
		}
		newPrimary, _, _ := unstructured.NestedString(current.Object, "status", "currentPrimary")
		if newPrimary == "" || newPrimary == primary {
			continue
		}
		if _, _, ready, err := Apply(ctx, client, it, p); err != nil || !ready {
			continue
		}
		db, closeDB, err := openDB(host, p.Port, it.PostgresConfig, ca.Data["ca.crt"])
		if err != nil {
			continue
		}
		var marker int64
		err = db.QueryRowContext(ctx, "SELECT value FROM slice07_marker").Scan(&marker)
		if err == nil && marker == id {
			_, err = db.ExecContext(ctx, "INSERT INTO slice07_marker VALUES ($1)", id+1)
		}
		closeDB()
		if err == nil && marker == id {
			t.Logf("standby promoted and verified TLS read/write recovered in %s", time.Since(start).Round(time.Second))
			promoted = true
			break
		}
	}
	if !promoted {
		t.Fatalf("failover did not recover via same endpoint after %s", time.Since(start).Round(time.Second))
	}
}
