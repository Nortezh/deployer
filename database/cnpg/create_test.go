package cnpg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
)

// Real client/TLS/Postgres wire probe against a test server, not a PG/Operator fixture.
func TestDedicatedCreateQueriesBeforeReady(t *testing.T) {
	host, port, ca, queries := testPostgresTLS(t, bootstrapPassword)
	objects := map[string]map[string]any{}
	allocations := map[string]any{}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		respond := func(x any) {
			if err := json.NewEncoder(w).Encode(x); err != nil {
				t.Error(err)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/nodes") {
			respond(map[string]any{"apiVersion": "v1", "kind": "NodeList", "items": []any{map[string]any{"metadata": map[string]any{"name": "lb", "labels": map[string]any{"kdb/role": "lb"}, "annotations": map[string]any{"kdb.io/host": host}}}}})
			return
		}
		if r.URL.Path == "/api/v1/namespaces/allocator/configmaps/"+allocationMap {
			if r.Method == http.MethodPut {
				var cm map[string]any
				if json.NewDecoder(r.Body).Decode(&cm) != nil {
					t.Error("bad allocation")
					return
				}
				allocations = cm["data"].(map[string]any)
			}
			respond(map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": allocationMap, "namespace": "allocator", "resourceVersion": "1"}, "data": allocations})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/secrets/secrets-2") {
			respond(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "secrets-2", "namespace": "app", "labels": map[string]string{"id": "secrets-2", "projectId": "2"}, "annotations": map[string]string{"nortezh.io/secret-revision-3": "1"}}, "type": "Opaque", "data": map[string][]byte{"secret-3": []byte(bootstrapPassword)}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/secrets/cnpg-1-2-ca") {
			respond(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "cnpg-1-2-ca", "namespace": "app-database"}, "data": map[string][]byte{"ca.crt": ca}})
			return
		}
		if r.Method == http.MethodPost {
			var obj map[string]any
			if json.NewDecoder(r.Body).Decode(&obj) != nil {
				t.Error("bad resource")
				return
			}
			if obj["kind"] == "Cluster" {
				obj["status"] = map[string]any{"phase": "Cluster in healthy state", "readyInstances": 2}
			}
			name := obj["metadata"].(map[string]any)["name"].(string)
			objects[r.URL.Path+"/"+name] = obj
			w.WriteHeader(201)
			respond(obj)
			return
		}
		if obj, ok := objects[r.URL.Path]; ok {
			respond(obj)
			return
		}
		w.WriteHeader(404)
		respond(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{AllocationNamespace: "allocator", PortRange: port + "-" + port, PVCRetention: "delete", StorageClass: "test", Image: "registry/postgres@sha256:" + strings.Repeat("a", 64)}
	command := bootstrapCommand()
	for i := 0; i < 2; i++ {
		gotHost, gotPort, ready, err := ApplyCreate(context.Background(), client, command, p)
		wantPort, _ := strconv.Atoi(port)
		if err != nil || !ready || gotHost != host || gotPort != wantPort {
			t.Fatal("query-ready failed", err)
		}
	}
	if queries.Load() != 2 {
		t.Fatal("ready bypassed authenticated query")
	}
	cfg := &api.DatabaseConfigPostgres{User: command.Username, Password: bootstrapPassword, Database: command.Username}
	if err := probe(context.Background(), host, port, cfg, []byte("bad CA")); err == nil {
		t.Fatal("wrong CA accepted")
	}
	if err := probe(context.Background(), "localhost", port, cfg, ca); err == nil {
		t.Fatal("wrong certificate hostname accepted")
	}
	bad := *cfg
	bad.Password = "wrong-test-password"
	if err := probe(context.Background(), host, port, &bad, ca); err == nil {
		t.Fatal("wrong password accepted")
	}
	mu.Lock()
	cluster := objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]
	if cluster == nil || cluster["spec"].(map[string]any)["instances"] != float64(2) {
		mu.Unlock()
		t.Fatal("stock HA2 Cluster missing")
	}
	cluster["status"].(map[string]any)["readyInstances"] = 1
	mu.Unlock()
	if _, _, ready, err := ApplyCreate(context.Background(), client, command, p); err != nil || ready {
		t.Fatal("one ready instance published HA2 ready", err)
	}
	mu.Lock()
	cluster["spec"].(map[string]any)["storage"].(map[string]any)["size"] = "2048Mi"
	mu.Unlock()
	if _, _, ready, err := ApplyCreate(context.Background(), client, command, p); err == nil || ready {
		t.Fatal("existing owned but incompatible Cluster adopted")
	}
	if queries.Load() != 2 {
		t.Fatal("incomplete/mismatched Cluster reached query")
	}
}
