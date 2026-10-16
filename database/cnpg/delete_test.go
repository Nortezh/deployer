package cnpg

import (
	"context"
	"encoding/json"
	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type deleteFixture struct {
	expiredLease bool
	denyReceipt  bool
	mu           sync.Mutex
	objects      map[string]map[string]any
	deletes      []string
	requests     []string
	denied       string
}

func deleteCommand() *api.DeployerCommandCNPGDelete {
	return &api.DeployerCommandCNPGDelete{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: api.CNPGDeleteVersion, ID: 1, ProjectID: 2, OperationID: "b1111111-1111-4111-8111-111111111111"}, Phase: api.CNPGDeleteResources, SecretID: 3, SecretRevision: 1, CreateOperationID: "a1111111-1111-4111-8111-111111111111"}
}
func deleteProfile() Profile {
	return Profile{AllocationNamespace: "allocator", PortRange: "6109-6110", PVCRetention: "delete", StorageClass: "local-path", Image: "ghcr.io/cloudnative-pg/postgresql:17.5@sha256:" + strings.Repeat("a", 64)}
}

func newDeleteFixture(t *testing.T) (*k8s.Client, *deleteFixture) {
	t.Helper()
	f := &deleteFixture{objects: map[string]map[string]any{}}
	put := func(path, kind, uid string, extra map[string]any) {
		obj := map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": path[strings.LastIndex(path, "/")+1:], "uid": uid, "resourceVersion": "1"}}
		if i := strings.Index(path, "/namespaces/"); i >= 0 {
			obj["metadata"].(map[string]any)["namespace"] = strings.Split(path[i+12:], "/")[0]
		}
		for k, v := range extra {
			obj[k] = v
		}
		f.objects[path] = obj
	}
	ns := "/api/v1/namespaces/app-database/"
	root := "/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"
	labels := map[string]any{"nortezh.io/database-id": "1"}
	refs := []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "cnpg-1-2", "uid": "cluster-uid", "controller": true}}
	put(root, "Cluster", "cluster-uid", map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "spec": map[string]any{"instances": 2, "imageName": deleteProfile().Image, "storage": map[string]any{"storageClass": "local-path"}, "bootstrap": map[string]any{"initdb": map[string]any{"secret": map[string]any{"name": "cnpg-1-2-app"}}}}, "status": map[string]any{"readyInstances": 2}})
	f.objects[root]["metadata"].(map[string]any)["labels"] = labels
	route := "/apis/traefik.io/v1alpha1/namespaces/app-database/ingressroutetcps/cnpg-1-2"
	put(route, "IngressRouteTCP", "route-uid", map[string]any{"apiVersion": "traefik.io/v1alpha1", "spec": map[string]any{"entryPoints": []any{"tcp-6109"}}})
	f.objects[route]["metadata"].(map[string]any)["labels"] = map[string]any{"nortezh.io/database-id": "1", "kdb.io/lb-node": "lb"}
	put(ns+"secrets/cnpg-1-2-app", "Secret", "bootstrap-uid", map[string]any{"immutable": true, "type": "kubernetes.io/basic-auth", "data": map[string]any{"username": "YXBw", "password": "Zml4dHVyZS1vbmx5"}})
	f.objects[ns+"secrets/cnpg-1-2-app"]["metadata"].(map[string]any)["labels"] = map[string]any{"nortezh.io/database-id": "1", "nortezh.io/project-id": "2", "nortezh.io/operation-id": deleteCommand().CreateOperationID, "nortezh.io/secret-id": "3", "nortezh.io/secret-revision": "1"}
	for _, suffix := range []string{"1", "2"} {
		pvcName := "cnpg-1-2-" + suffix
		uid := "pvc-" + suffix
		put(ns+"persistentvolumeclaims/"+pvcName, "PersistentVolumeClaim", uid, map[string]any{"spec": map[string]any{"storageClassName": "local-path", "volumeName": "pv-" + suffix}, "status": map[string]any{"phase": "Bound"}})
		f.objects[ns+"persistentvolumeclaims/"+pvcName]["metadata"].(map[string]any)["ownerReferences"] = refs
		f.objects[ns+"persistentvolumeclaims/"+pvcName]["metadata"].(map[string]any)["labels"] = map[string]any{"cnpg.io/cluster": "cnpg-1-2"}
		put("/api/v1/persistentvolumes/pv-"+suffix, "PersistentVolume", "pv-uid-"+suffix, map[string]any{"spec": map[string]any{"storageClassName": "local-path", "persistentVolumeReclaimPolicy": "Delete", "local": map[string]any{"path": "/owned-fixture"}, "claimRef": map[string]any{"namespace": "app-database", "name": pvcName, "uid": uid}, "nodeAffinity": map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{"lb"}}}}}}}}})
		put(ns+"pods/"+pvcName, "Pod", "pod-"+suffix, map[string]any{"spec": map[string]any{"nodeName": "lb", "containers": []any{map[string]any{"name": "postgres", "image": "fixture"}}, "volumes": []any{map[string]any{"name": "pgdata", "persistentVolumeClaim": map[string]any{"claimName": pvcName}}}}})
		f.objects[ns+"pods/"+pvcName]["metadata"].(map[string]any)["ownerReferences"] = refs
		f.objects[ns+"pods/"+pvcName]["metadata"].(map[string]any)["labels"] = map[string]any{"cnpg.io/cluster": "cnpg-1-2"}
	}
	put("/api/v1/nodes/lb", "Node", "node-uid", map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "nodeInfo": map[string]any{"bootID": "boot-1", "kubeletVersion": "v1.31.11+k3s1"}}})
	put("/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/lb", "Lease", "lease-uid", map[string]any{"apiVersion": "coordination.k8s.io/v1", "spec": map[string]any{"holderIdentity": "lb", "leaseDurationSeconds": 40, "renewTime": metav1.NewMicroTime(time.Now().UTC())}})
	f.objects["/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/lb"]["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "Node", "name": "lb", "uid": "node-uid"}}
	put("/apis/storage.k8s.io/v1/storageclasses/local-path", "StorageClass", "storage-uid", map[string]any{"apiVersion": "storage.k8s.io/v1", "provisioner": "rancher.io/local-path", "reclaimPolicy": "Delete", "volumeBindingMode": "WaitForFirstConsumer"})
	put("/apis/apps/v1/namespaces/kube-system/deployments/local-path-provisioner", "Deployment", "provisioner-uid", map[string]any{"apiVersion": "apps/v1", "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "local-path-provisioner", "image": "rancher/local-path-provisioner:v0.0.31"}}}}}, "status": map[string]any{"availableReplicas": 1}})
	put("/api/v1/namespaces/allocator/configmaps/"+allocationMap, "ConfigMap", "allocation-uid", map[string]any{"data": map[string]any{"lb_6109": "cnpg:app-database/cnpg-1-2:1", "lb_6110": "unrelated-owner"}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		status := func(code int, reason string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReason(reason), Code: int32(code)})
		}
		if f.denyReceipt && r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/cnpg-1-2-retirement") {
			status(403, "Forbidden")
			return
		}
		if r.URL.Path == f.denied {
			status(403, "Forbidden")
			return
		}
		obj, exists := f.objects[r.URL.Path]
		if exists && strings.Contains(r.URL.Path, "/leases/") && !f.expiredLease {
			obj["spec"].(map[string]any)["renewTime"] = metav1.NewMicroTime(time.Now().UTC())
		}
		switch r.Method {
		case http.MethodDelete:
			if !exists {
				status(404, "NotFound")
				return
			}
			var opts metav1.DeleteOptions
			if json.NewDecoder(r.Body).Decode(&opts) != nil || opts.Preconditions == nil || opts.Preconditions.UID == nil || string(*opts.Preconditions.UID) != obj["metadata"].(map[string]any)["uid"] || opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "1" || opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationForeground {
				t.Error("unsafe delete preconditions")
				status(409, "Conflict")
				return
			}
			f.deletes = append(f.deletes, r.URL.Path)
			delete(f.objects, r.URL.Path)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success"})
			return
		case http.MethodPost, http.MethodPut:
			var incoming map[string]any
			if json.NewDecoder(r.Body).Decode(&incoming) != nil {
				status(400, "BadRequest")
				return
			}
			meta := incoming["metadata"].(map[string]any)
			path := r.URL.Path
			if r.Method == http.MethodPost {
				path += "/" + meta["name"].(string)
				meta["uid"] = "retirement-uid"
				meta["resourceVersion"] = "1"
			}
			f.objects[path] = incoming
			_ = json.NewEncoder(w).Encode(incoming)
			return
		}
		if exists {
			_ = json.NewEncoder(w).Encode(obj)
			return
		}
		kinds := map[string]string{"deployments": "DeploymentList", "statefulsets": "StatefulSetList", "daemonsets": "DaemonSetList", "replicasets": "ReplicaSetList", "services": "ServiceList", "secrets": "SecretList", "pods": "PodList", "persistentvolumeclaims": "PersistentVolumeClaimList", "persistentvolumes": "PersistentVolumeList", "jobs": "JobList", "cronjobs": "CronJobList", "backups": "BackupList", "scheduledbackups": "ScheduledBackupList"}
		part := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if kind, ok := kinds[part]; ok {
			items := []any{}
			for path, item := range f.objects {
				if strings.HasPrefix(path, r.URL.Path+"/") && !strings.Contains(strings.TrimPrefix(path, r.URL.Path+"/"), "/") {
					items = append(items, item)
				}
			}
			version := "v1"
			if part == "deployments" || part == "statefulsets" || part == "daemonsets" || part == "replicasets" {
				version = "apps/v1"
			}
			if part == "jobs" || part == "cronjobs" {
				version = "batch/v1"
			}
			if part == "backups" || part == "scheduledbackups" {
				version = "postgresql.cnpg.io/v1"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": kind, "items": items})
			return
		}
		status(404, "NotFound")
	}))
	t.Cleanup(server.Close)
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	return client, f
}

func TestStandardDeleteWaitsForOwnedResourcesAndSecret(t *testing.T) {
	for _, mode := range []string{"initdb", "recovery"} {
		t.Run(mode, func(t *testing.T) { testStandardDeleteLifecycle(t, mode) })
	}
}

func testStandardDeleteLifecycle(t *testing.T, mode string) {
	t.Helper()
	c, f := newDeleteFixture(t)
	if mode == "recovery" {
		spec := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["spec"].(map[string]any)
		spec["bootstrap"] = map[string]any{"recovery": map[string]any{"source": "selected-backup", "secret": map[string]any{"name": "cnpg-1-2-app"}, "recoveryTarget": map[string]any{"backupID": "20261015T120000", "targetImmediate": true}}}
		spec["externalClusters"] = []any{map[string]any{"name": "selected-backup", "plugin": map[string]any{"name": barmanPlugin, "parameters": map[string]any{"barmanObjectName": "source-store", "serverName": "source"}}}}
	}
	it := deleteCommand()
	// These source resources are outside the target's ownership and must survive cleanup.
	sourcePaths := map[string]string{
		"/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/source":            "Cluster",
		"/apis/postgresql.cnpg.io/v1/namespaces/app-database/backups/source-backup":      "Backup",
		"/apis/barmancloud.cnpg.io/v1/namespaces/app-database/objectstores/source-store": "ObjectStore",
		"/api/v1/namespaces/app-database/secrets/source-app":                             "Secret",
	}
	for path, kind := range sourcePaths {
		f.objects[path] = map[string]any{"apiVersion": "v1", "kind": kind, "metadata": map[string]any{"name": path[strings.LastIndex(path, "/")+1:], "uid": "source-uid", "resourceVersion": "1"}}
	}
	ctx := context.Background()
	p := deleteProfile()
	// Full lifecycle also works with create disabled and a degraded HA2 database.
	p.Image, p.PortRange = "", ""
	f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["status"].(map[string]any)["readyInstances"] = 0
	delete(f.objects, "/api/v1/namespaces/app-database/pods/cnpg-1-2-2")
	absent, err := ApplyDelete(ctx, c, it, p)
	if err != nil || absent {
		dump, _ := json.Marshal(f.objects["/api/v1/namespaces/app-database/configmaps/cnpg-1-2-retirement"])
		t.Fatal("must start pending", err, f.requests, string(dump))
	}
	f.mu.Lock()
	record := f.objects["/api/v1/namespaces/app-database/configmaps/cnpg-1-2-retirement"]
	if record == nil || len(f.deletes) != 1 || !strings.Contains(f.deletes[0], "/clusters/") {
		t.Fatal("inventory must precede root delete")
	}
	data, _ := json.Marshal(record)
	if strings.Contains(string(data), "fixture-only") || strings.Contains(string(data), "password") {
		t.Fatal("credential leaked into retirement inventory")
	}
	// Simulate stock GC of controller dependents, but intentionally retain PV1.
	for path := range f.objects {
		if (strings.Contains(path, "/pods/") || strings.Contains(path, "/persistentvolumeclaims/")) || strings.HasSuffix(path, "persistentvolumes/pv-2") {
			delete(f.objects, path)
		}
	}
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		absent, err = ApplyDelete(ctx, c, it, p)
		if err != nil || absent {
			t.Fatal("PV must retain pending", err)
		}
	}
	f.mu.Lock()
	delete(f.objects, "/api/v1/persistentvolumes/pv-1")
	f.mu.Unlock()
	absent, err = ApplyDelete(ctx, c, it, p)
	if err != nil || !absent {
		t.Fatal("resource stage incomplete", err)
	}
	f.mu.Lock()
	alloc := f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)
	if alloc["lb_6109"] == nil {
		t.Fatal("premature port release")
	}
	f.mu.Unlock()
	final := *it
	final.Phase = api.CNPGDeleteFinalize
	f.mu.Lock()
	f.objects["/api/v1/namespaces/app/secrets/secrets-2"] = map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "secrets-2", "namespace": "app", "labels": map[string]any{"id": "secrets-2", "projectId": "2"}, "annotations": map[string]any{"nortezh.io/secret-revision-3": "1"}}, "type": "Opaque", "data": map[string]any{"secret-3": "Zml4dHVyZS1vbmx5"}}
	f.mu.Unlock()
	if done, err := ApplyDelete(ctx, c, &final, p); done || err == nil {
		t.Fatal("live aggregate key was ignored")
	}
	f.mu.Lock()
	delete(f.objects, "/api/v1/namespaces/app/secrets/secrets-2")
	f.mu.Unlock()
	for i := 0; i < 2; i++ {
		if done, err := ApplyDelete(ctx, c, &final, p); !done || err != nil {
			t.Fatal("finalize/retry failed", err)
		}
	}
	f.mu.Lock()
	alloc = f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)
	if alloc["lb_6109"] != nil || alloc["lb_6110"] != "unrelated-owner" {
		t.Fatal("wrong port released")
	}
	alloc["lb_6109"] = "new-owner"
	f.mu.Unlock()
	if done, err := ApplyDelete(ctx, c, &final, p); !done || err != nil {
		t.Fatal("completed retry must preserve new port owner", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for path := range sourcePaths {
		if f.objects[path] == nil {
			t.Fatal("source recovery resource was deleted", path)
		}
	}
}

func TestStandardDeleteReleaseReceiptFailureRetainsResponsibility(t *testing.T) {
	c, f := newDeleteFixture(t)
	it := deleteCommand()
	p := deleteProfile()
	if done, err := ApplyDelete(context.Background(), c, it, p); done || err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for path := range f.objects {
		if strings.HasPrefix(path, "/api/v1/namespaces/app-database/") && !strings.Contains(path, "/configmaps/") || strings.HasPrefix(path, "/api/v1/persistentvolumes/") || strings.HasPrefix(path, "/apis/traefik.io/") {
			delete(f.objects, path)
		}
	}
	f.denyReceipt = true
	f.mu.Unlock()
	final := *it
	final.Phase = api.CNPGDeleteFinalize
	if done, err := ApplyDelete(context.Background(), c, &final, p); done || err == nil {
		t.Fatal("lost receipt must not release name responsibility")
	}
	f.mu.Lock()
	f.denyReceipt = false
	f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)["lb_6109"] = "new-owner"
	f.mu.Unlock()
	if done, err := ApplyDelete(context.Background(), c, &final, p); done || err == nil {
		t.Fatal("must not infer or overwrite foreign reuse after lost receipt")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)["lb_6109"] != "new-owner" {
		t.Fatal("foreign reservation destroyed")
	}
}

func TestStandardDeleteIgnoresCreateSettingsAndDatabaseHealth(t *testing.T) {
	for _, kind := range []string{"changed-image", "changed-port-pool", "empty-create-settings", "unready-instance", "missing-pod", "no-live-pods", "missing-health-status", "hostpath-volume"} {
		t.Run(kind, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			p := deleteProfile()
			f.mu.Lock()
			root := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]
			switch kind {
			case "hostpath-volume":
				for _, suffix := range []string{"1", "2"} {
					spec := f.objects["/api/v1/persistentvolumes/pv-"+suffix]["spec"].(map[string]any)
					delete(spec, "local")
					spec["hostPath"] = map[string]any{"path": "/owned-fixture", "type": "DirectoryOrCreate"}
				}
			case "changed-image":
				p.Image = "postgres:next-create-image"
			case "changed-port-pool":
				p.PortRange = "6200-6300"
			case "empty-create-settings":
				p.Image = ""
				p.PortRange = ""
			case "unready-instance":
				root["status"].(map[string]any)["readyInstances"] = 1
			case "missing-pod":
				root["status"].(map[string]any)["readyInstances"] = 1
				delete(f.objects, "/api/v1/namespaces/app-database/pods/cnpg-1-2-2")
			case "no-live-pods":
				root["status"].(map[string]any)["readyInstances"] = 0
				delete(f.objects, "/api/v1/namespaces/app-database/pods/cnpg-1-2-1")
				delete(f.objects, "/api/v1/namespaces/app-database/pods/cnpg-1-2-2")
			case "missing-health-status":
				delete(root, "status")
			}
			f.mu.Unlock()
			if done, err := ApplyDelete(context.Background(), c, deleteCommand(), p); done || err != nil {
				t.Fatal("cleanup must start pending without new-create or DB-health requirements", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.deletes) != 1 || !strings.Contains(f.deletes[0], "/clusters/") {
				t.Fatal("must inventory before deleting only the owned root")
			}
			if f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)["lb_6109"] == nil || f.objects["/api/v1/persistentvolumes/pv-1"] == nil {
				t.Fatal("premature allocation release or explicit PV removal")
			}
		})
	}
}

func TestStandardDeleteBlocksUncertainInventoryBeforeWrites(t *testing.T) {
	for _, kind := range []string{"foreign-bootstrap", "wrong-recovery-secret", "mixed-bootstrap", "recovery-archiver", "foreign-cluster", "retain", "claim-uid", "node-notready", "node-boot-empty", "backup", "missing-root", "foreign-route", "unsupported-storage", "rbac", "expired-lease", "wrong-instance-config", "missing-instance-config", "missing-pvc", "unbound-pvc", "missing-allocation-namespace", "retain-profile", "bad-allocation-key", "foreign-reservation", "duplicate-reservation", "wrong-entrypoint", "missing-volume-source", "empty-local-path", "unsupported-volume-source"} {
		t.Run(kind, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			p := deleteProfile()
			f.mu.Lock()
			switch kind {
			case "missing-volume-source", "empty-local-path", "unsupported-volume-source":
				spec := f.objects["/api/v1/persistentvolumes/pv-1"]["spec"].(map[string]any)
				delete(spec, "local")
				if kind == "empty-local-path" {
					spec["local"] = map[string]any{"path": ""}
				} else if kind == "unsupported-volume-source" {
					spec["nfs"] = map[string]any{"server": "foreign", "path": "/foreign"}
				}
			case "wrong-recovery-secret", "mixed-bootstrap", "recovery-archiver":
				spec := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["spec"].(map[string]any)
				bootstrap := spec["bootstrap"].(map[string]any)
				bootstrap["recovery"] = map[string]any{"secret": map[string]any{"name": "cnpg-1-2-app"}}
				if kind != "mixed-bootstrap" {
					delete(bootstrap, "initdb")
				}
				if kind == "wrong-recovery-secret" {
					bootstrap["recovery"] = map[string]any{"secret": map[string]any{"name": "foreign-app"}}
				}
				if kind == "recovery-archiver" {
					spec["plugins"] = []any{map[string]any{"name": barmanPlugin, "isWALArchiver": true}}
				}
			case "foreign-bootstrap":
				f.objects["/api/v1/namespaces/app-database/secrets/cnpg-1-2-app"]["metadata"].(map[string]any)["labels"].(map[string]any)["nortezh.io/project-id"] = "foreign"
			case "foreign-cluster":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["metadata"].(map[string]any)["labels"].(map[string]any)["nortezh.io/database-id"] = "foreign"
			case "retain":
				f.objects["/api/v1/persistentvolumes/pv-1"]["spec"].(map[string]any)["persistentVolumeReclaimPolicy"] = "Retain"
			case "claim-uid":
				f.objects["/api/v1/persistentvolumes/pv-1"]["spec"].(map[string]any)["claimRef"].(map[string]any)["uid"] = "foreign"
			case "node-notready":
				f.objects["/api/v1/nodes/lb"]["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "Unknown"}}
			case "node-boot-empty":
				f.objects["/api/v1/nodes/lb"]["status"].(map[string]any)["nodeInfo"].(map[string]any)["bootID"] = ""
			case "backup":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/backups/retained"] = map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup", "metadata": map[string]any{"name": "retained"}, "spec": map[string]any{"cluster": map[string]any{"name": "cnpg-1-2"}}}
			case "missing-root":
				delete(f.objects, "/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2")
			case "foreign-route":
				f.objects["/apis/traefik.io/v1alpha1/namespaces/app-database/ingressroutetcps/cnpg-1-2"]["metadata"].(map[string]any)["labels"].(map[string]any)["kdb.io/lb-node"] = "foreign"
			case "unsupported-storage":
				p.StorageClass = "retained-csi"
			case "wrong-instance-config":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["spec"].(map[string]any)["instances"] = 1
			case "missing-instance-config":
				delete(f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["spec"].(map[string]any), "instances")
			case "missing-pvc":
				delete(f.objects, "/api/v1/namespaces/app-database/persistentvolumeclaims/cnpg-1-2-2")
			case "unbound-pvc":
				f.objects["/api/v1/namespaces/app-database/persistentvolumeclaims/cnpg-1-2-2"]["status"].(map[string]any)["phase"] = "Pending"
			case "missing-allocation-namespace":
				p.AllocationNamespace = ""
			case "retain-profile":
				p.PVCRetention = "retain"
			case "bad-allocation-key":
				data := f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)
				data["lb_0"] = data["lb_6109"]
				delete(data, "lb_6109")
			case "foreign-reservation":
				f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)["lb_6109"] = "foreign"
			case "duplicate-reservation":
				data := f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)
				data["lb_6110"] = data["lb_6109"]
			case "wrong-entrypoint":
				f.objects["/apis/traefik.io/v1alpha1/namespaces/app-database/ingressroutetcps/cnpg-1-2"]["spec"].(map[string]any)["entryPoints"] = []any{"tcp-6200"}
			case "expired-lease":
				f.expiredLease = true
				f.objects["/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/lb"]["spec"].(map[string]any)["renewTime"] = metav1.NewMicroTime(time.Now().Add(-time.Minute))
			case "rbac":
				f.denied = "/api/v1/persistentvolumes"
			}
			f.mu.Unlock()
			if done, err := ApplyDelete(context.Background(), c, deleteCommand(), p); done || err == nil {
				t.Fatal("uncertainty accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.deletes) != 0 || f.objects["/api/v1/namespaces/app-database/configmaps/cnpg-1-2-retirement"] != nil {
				t.Fatal("wrote before safe inventory")
			}
		})
	}
}

func TestStandardDeleteRejectsChangedUIDAndLateDependents(t *testing.T) {
	for _, kind := range []string{"cluster-uid", "changed-boot", "late-pod", "late-job", "late-cronjob", "retained-pv", "inventory-operation", "late-controller", "terminating-pod", "changed-volume-source", "empty-local-path"} {
		t.Run(kind, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			it := deleteCommand()
			p := deleteProfile()
			if done, err := ApplyDelete(context.Background(), c, it, p); done || err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			switch kind {
			case "cluster-uid":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"] = map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "cnpg-1-2", "namespace": "app-database", "uid": "foreign", "resourceVersion": "1"}}
			case "terminating-pod":
				f.objects["/api/v1/namespaces/app-database/pods/cnpg-1-2-1"]["metadata"].(map[string]any)["deletionTimestamp"] = metav1.NewTime(time.Now())
			case "changed-boot":
				f.objects["/api/v1/nodes/lb"]["status"].(map[string]any)["nodeInfo"].(map[string]any)["bootID"] = "boot-2"
			case "late-pod", "late-job", "late-cronjob":
				resource, kindName, version := "pods", "Pod", "v1"
				prefix := "/api/v1/"
				if kind != "late-pod" {
					prefix = "/apis/batch/v1/"
					version = "batch/v1"
					if kind == "late-job" {
						resource, kindName = "jobs", "Job"
					} else {
						resource, kindName = "cronjobs", "CronJob"
					}
				}
				f.objects[prefix+"namespaces/app-database/"+resource+"/late"] = map[string]any{"apiVersion": version, "kind": kindName, "metadata": map[string]any{"name": "late", "namespace": "app-database", "uid": "late", "labels": map[string]any{"cnpg.io/cluster": "cnpg-1-2"}}}
			case "late-controller":
				f.objects["/apis/apps/v1/namespaces/app-database/deployments/late"] = map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "late", "namespace": "app-database", "uid": "late", "labels": map[string]any{"cnpg.io/cluster": "cnpg-1-2"}}}
			case "changed-volume-source", "empty-local-path":
				spec := f.objects["/api/v1/persistentvolumes/pv-1"]["spec"].(map[string]any)
				delete(spec, "local")
				if kind == "empty-local-path" {
					spec["local"] = map[string]any{"path": ""}
				} else {
					spec["nfs"] = map[string]any{"server": "foreign", "path": "/foreign"}
				}
			case "retained-pv":
				f.objects["/api/v1/persistentvolumes/pv-1"]["spec"].(map[string]any)["persistentVolumeReclaimPolicy"] = "Retain"
			case "inventory-operation":
				it.OperationID = "c1111111-1111-4111-8111-111111111111"
			}
			before := len(f.deletes)
			f.mu.Unlock()
			if done, err := ApplyDelete(context.Background(), c, it, p); done || err == nil {
				t.Fatal("changed ownership/use accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.deletes) != before {
				t.Fatal("destructive action after uncertainty")
			}
		})
	}
}
