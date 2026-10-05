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

// HTTP-only lifecycle check: no cluster, SQL connection or live credentials.
func TestAllocatedLifecycle(t *testing.T) {
	objects := map[string]map[string]any{}
	allocations := map[string]any{"lb_6200": "legacy/db"}
	pvcRemaining := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		respond := func(obj any) {
			if err := json.NewEncoder(w).Encode(obj); err != nil {
				t.Error(err)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/nodes") {
			respond(map[string]any{"apiVersion": "v1", "kind": "NodeList", "items": []any{map[string]any{"metadata": map[string]any{"name": "lb", "labels": map[string]any{"kdb/role": "lb"}, "annotations": map[string]any{"kdb.io/host": "lb.example"}}}}})
			return
		}
		if strings.Contains(r.URL.Path, "/configmaps/") {
			if r.Method == http.MethodPut {
				var cm map[string]any
				json.NewDecoder(r.Body).Decode(&cm)
				allocations = cm["data"].(map[string]any)
			}
			respond(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": allocationMap, "namespace": "allocator", "resourceVersion": "1"}, "data": allocations})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/persistentvolumeclaims") {
			items := []any{}
			if pvcRemaining {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "orders-2-1"}})
			}
			respond(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaimList", "items": items})
			return
		}
		switch r.Method {
		case http.MethodPost:
			var obj map[string]any
			json.NewDecoder(r.Body).Decode(&obj)
			objects[r.URL.Path+"/"+obj["metadata"].(map[string]any)["name"].(string)] = obj
			w.WriteHeader(201)
			respond(obj)
		case http.MethodDelete:
			delete(objects, r.URL.Path)
			respond(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
		case http.MethodGet:
			if obj, ok := objects[r.URL.Path]; ok {
				respond(obj)
				return
			}
			w.WriteHeader(404)
			respond(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(405)
		}
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("deploys")
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{AllocationNamespace: "allocator", PortRange: "6200-6202", PVCRetention: "delete", StorageClass: "test", Image: "pinned"}
	it := &api.DeployerCommandDatabaseCreate{ID: 9, ProjectID: 2, Name: "orders", StorageSize: 1024, PostgresConfig: &api.DatabaseConfigPostgres{User: "app", Password: "test-only"}}
	for i := 0; i < 2; i++ {
		_, _, ready, err := Apply(context.Background(), client, it, p)
		if err != nil || ready {
			t.Fatalf("apply %d: %v %v", i, ready, err)
		}
	}
	if allocations["lb_6201"] != allocationOwner("deploys-database", "orders-2", 9) || len(allocations) != 2 {
		t.Fatal("allocation not idempotent")
	}
	route := &unstructured.Unstructured{Object: objects["/apis/traefik.io/v1alpha1/namespaces/deploys-database/ingressroutetcps/orders-2"]}
	entries, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	routes, _, _ := unstructured.NestedSlice(route.Object, "spec", "routes")
	tls, _, _ := unstructured.NestedBool(route.Object, "spec", "tls", "passthrough")
	if len(entries) != 1 || entries[0] != "tcp-6201" || len(routes) != 1 || routes[0].(map[string]any)["match"] != "HostSNI(`*`)" || !tls || route.GetLabels()["kdb.io/lb-node"] != "lb" {
		t.Fatal("incorrect allocated route")
	}
	cluster := objects["/apis/postgresql.cnpg.io/v1/namespaces/deploys-database/clusters/orders-2"]
	hosts, _, _ := unstructured.NestedStringSlice(cluster, "spec", "certificates", "serverAltDNSNames")
	if len(hosts) != 1 || hosts[0] != "lb.example" {
		t.Fatal("wrong certificate endpoint")
	}
	// A stale or modified route must not silently become a ready endpoint.
	if err := unstructured.SetNestedStringSlice(route.Object, []string{"tcp-6202"}, "spec", "entryPoints"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Apply(context.Background(), client, it, p); err == nil {
		t.Fatal("route drift accepted")
	}
	if err := unstructured.SetNestedStringSlice(route.Object, []string{"tcp-6201"}, "spec", "entryPoints"); err != nil {
		t.Fatal(err)
	}
	metadata := &api.DeployerCommandDatabaseMetadata{ID: 9, ProjectID: 2, Name: "orders"}
	for i := 0; i < 4; i++ {
		done, err := Delete(context.Background(), client, metadata, p)
		if err != nil || done {
			t.Fatalf("premature delete %d: %v %v", i, done, err)
		}
		if len(allocations) != 2 {
			t.Fatal("port released before cleanup")
		}
	}
	pvcRemaining = false
	done, err := Delete(context.Background(), client, metadata, p)
	if err != nil || !done || len(allocations) != 1 || allocations["lb_6200"] != "legacy/db" {
		t.Fatal("cleanup did not release only owned port", err)
	}
}
