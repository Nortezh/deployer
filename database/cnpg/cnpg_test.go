package cnpg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// HTTP mock only: no kubeconfig, cluster, DNS lookup, or PostgreSQL connection.
func TestCNPGTraefikRoute(t *testing.T) {
	created := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(405)
			return
		}
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		created[obj["kind"].(string)] = obj
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(obj)
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("deploys")
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{HostSuffix: ".db.example", Port: "6109", EntryPoint: "tcp-6109", NodeName: "staging-node", StorageClass: "approved-class", Image: "ghcr.io/cloudnative-pg/postgresql:17.5@sha256:" + strings.Repeat("a", 64)}
	it := &api.DeployerCommandDatabaseCreate{ID: 1, ProjectID: 2, Name: "orders", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG, StorageSize: 1024, PostgresConfig: &api.DatabaseConfigPostgres{User: "app", Password: "test-only"}}
	host, port, ready, err := Apply(context.Background(), client, it, p)
	if err != nil || ready || host != "" || port != 0 {
		t.Fatalf("unhealthy cluster should remain pending: ready=%v err=%v", ready, err)
	}
	if len(created) != 3 {
		t.Fatalf("expected Secret, Cluster, route; got %d resources", len(created))
	}
	route := &unstructured.Unstructured{Object: created["IngressRouteTCP"]}
	if route.GetNamespace() != "deploys-database" || route.GetLabels()["kdb.io/lb-node"] != p.NodeName || route.GetLabels()["nortezh.io/database-id"] != "1" {
		t.Fatal("incorrect route namespace/ownership/Traefik label")
	}
	entries, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	if len(entries) != 1 || entries[0] != "tcp-6109" {
		t.Fatal("incorrect Traefik entrypoint")
	}
	passthrough, _, _ := unstructured.NestedBool(route.Object, "spec", "tls", "passthrough")
	routes, _, _ := unstructured.NestedSlice(route.Object, "spec", "routes")
	if !passthrough || len(routes) != 1 {
		t.Fatal("expected one TLS passthrough route")
	}
	r := routes[0].(map[string]any)
	service := r["services"].([]any)[0].(map[string]any)
	if r["match"] != "HostSNI(`orders-2.db.example`)" || service["name"] != "orders-2-rw" || service["port"] != float64(5432) {
		t.Fatal("incorrect SNI/read-write target")
	}
}
