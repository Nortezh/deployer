package cnpg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type readinessFixture struct {
	mu                   sync.Mutex
	cluster, store       *unstructured.Unstructured
	app, ca, credentials *v1.Secret
	pod                  *v1.Pod
	allocation           *v1.ConfigMap
	node                 *v1.Node
	reads                map[string]int
	writes               int
	retiring             bool
	failPath             string
	after                func(string, int)
	queries              *atomic.Int64
}

func readinessCommand() *api.DeployerCommandCNPGBackupReadiness {
	return &api.DeployerCommandCNPGBackupReadiness{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: backupCommand().CreateOperationID}, CheckID: "b1111111-1111-4111-8111-111111111111", ProfileRevision: 1}
}

func newReadinessFixture(t *testing.T, archived int64) (*readinessFixture, *k8s.Client, Profile) {
	t.Helper()
	host, port, ca, queries := testPostgresTLSQuery(t, bootstrapPassword, readinessWALQuery, strconv.FormatInt(archived, 10))
	p := Profile{AllocationNamespace: "allocator", PortRange: port + "-" + port, PVCRetention: "delete", StorageClass: "hostpath", Image: "registry/postgres:17@sha256:" + strings.Repeat("a", 64), BackupObjectStore: "backup-store", BackupProfileRevision: 1}
	created := metav1.NewTime(time.Now().Add(-time.Hour).UTC().Truncate(time.Second))
	root := "c1111111-1111-4111-8111-111111111111"
	controller := true
	owner := []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "cnpg-1-2", UID: types.UID(root), Controller: &controller}}
	f := &readinessFixture{reads: map[string]int{}, queries: queries}
	f.cluster = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cnpg-1-2", "namespace": "app-database", "uid": root, "generation": int64(1), "resourceVersion": "1", "creationTimestamp": created.Format(time.RFC3339), "labels": map[string]any{"nortezh.io/database-id": "1"}},
		"spec":     map[string]any{"instances": int64(2), "imageName": p.Image, "storage": map[string]any{"storageClass": p.StorageClass}, "bootstrap": map[string]any{"initdb": map[string]any{"database": "app", "owner": "app", "secret": map[string]any{"name": "cnpg-1-2-app"}}}, "plugins": []any{map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "backup-store", "serverName": "cnpg-1-2"}}}},
		"status":   map[string]any{"phase": "Cluster in healthy state", "readyInstances": int64(2), "currentPrimary": "cnpg-1-2-1", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}, map[string]any{"type": "ContinuousArchiving", "status": "True", "reason": "ContinuousArchivingSuccess"}}, "pluginStatus": []any{map[string]any{"name": barmanPlugin, "version": "0.15.1"}}},
	}}
	f.store = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": map[string]any{"name": "backup-store", "namespace": "app-database", "uid": "e1111111-1111-4111-8111-111111111111", "generation": int64(1), "resourceVersion": "1", "creationTimestamp": created.Format(time.RFC3339)}, "spec": map[string]any{"configuration": map[string]any{"destinationPath": "s3://cnpg-backups/staging", "endpointURL": "http://minio.minio.svc.cluster.local:9000", "s3Credentials": map[string]any{"accessKeyId": map[string]any{"name": "s3-creds", "key": "ACCESS_KEY_ID"}, "secretAccessKey": map[string]any{"name": "s3-creds", "key": "ACCESS_SECRET_KEY"}}, "wal": map[string]any{"compression": "gzip"}, "data": map[string]any{"compression": "gzip"}}}}}
	immutable := true
	f.app = &v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cnpg-1-2-app", Namespace: "app-database", UID: "app-uid", ResourceVersion: "1", CreationTimestamp: created, Labels: map[string]string{"nortezh.io/database-id": "1", "nortezh.io/project-id": "2", "nortezh.io/operation-id": readinessCommand().OperationID}}, Immutable: &immutable, Type: v1.SecretTypeBasicAuth, Data: map[string][]byte{"username": []byte("app"), "password": []byte(bootstrapPassword)}}
	f.credentials = &v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: "app-database", UID: "credential-uid", ResourceVersion: "7", CreationTimestamp: created}, Immutable: &immutable, Data: map[string][]byte{"ACCESS_KEY_ID": []byte("fixture-access-key"), "ACCESS_SECRET_KEY": []byte("fixture-secret-key")}}
	f.credentials.ManagedFields = []metav1.ManagedFieldsEntry{{Time: &created, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{}}`)}}}
	f.ca = &v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cnpg-1-2-ca", Namespace: "app-database", UID: "ca-uid", ResourceVersion: "1", CreationTimestamp: created, OwnerReferences: owner}, Data: map[string][]byte{"ca.crt": ca}}
	f.pod = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "cnpg-1-2-1", Namespace: "app-database", UID: "primary-pod-uid", ResourceVersion: "1", CreationTimestamp: created, OwnerReferences: owner}, Status: v1.PodStatus{Phase: v1.PodRunning, ContainerStatuses: []v1.ContainerStatus{{Name: "postgres", Ready: true, State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: created}}}, {Name: "plugin-barman-cloud", Ready: true, State: v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: created}}}}}}
	f.pod.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionTrue}}
	f.pod.Spec.Containers = []v1.Container{{Name: "postgres", Image: p.Image}, {Name: "plugin-barman-cloud", Image: readinessSidecarImage}}
	f.allocation = &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationMap, Namespace: "allocator", UID: "allocation-uid"}, Data: map[string]string{"lb_" + port: allocationOwner("app-database", "cnpg-1-2", 1)}}
	f.node = &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lb", UID: "node-uid", Labels: map[string]string{"kdb/role": "lb"}, Annotations: map[string]string{"kdb.io/host": host}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		f.reads[r.URL.Path]++
		if f.after != nil {
			f.after(r.URL.Path, f.reads[r.URL.Path])
		}
		if r.Method != http.MethodGet {
			f.writes++
			w.WriteHeader(500)
			return
		}
		if f.failPath != "" && strings.Contains(r.URL.Path, f.failPath) {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "apiVersion": "v1", "reason": "Forbidden", "code": 403, "message": "fixture-secret-key must not be echoed"})
			return
		}
		var object any
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters/cnpg-1-2"):
			object = f.cluster
		case strings.HasSuffix(r.URL.Path, "/objectstores/backup-store"):
			object = f.store
		case strings.HasSuffix(r.URL.Path, "/secrets/cnpg-1-2-app"):
			object = f.app
		case strings.HasSuffix(r.URL.Path, "/secrets/cnpg-1-2-ca"):
			object = f.ca
		case strings.HasSuffix(r.URL.Path, "/secrets/s3-creds"):
			object = f.credentials
		case strings.HasSuffix(r.URL.Path, "/pods/cnpg-1-2-1"):
			object = f.pod
		case strings.HasSuffix(r.URL.Path, "/configmaps/"+allocationMap):
			object = f.allocation
		case strings.HasSuffix(r.URL.Path, "/nodes/lb"):
			object = f.node
		case strings.HasSuffix(r.URL.Path, "/configmaps/cnpg-1-2-retirement") && f.retiring:
			object = &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cnpg-1-2-retirement"}}
		default:
			w.WriteHeader(404)
			object = map[string]any{"kind": "Status", "apiVersion": "v1", "reason": "NotFound", "code": 404}
		}
		if err := json.NewEncoder(w).Encode(object); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	return f, client, p
}

func TestBackupReadinessNativeObservationAndRetry(t *testing.T) {
	f, c, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
	first, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
	if err != nil || first == nil || !first.Available || first.Valid() != nil || first.Evidence.ServerName != "cnpg-1-2" {
		t.Fatal("native evidence not observed", err)
	}
	second, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
	if err != nil || second == nil || !second.Available || *second.Evidence != *first.Evidence {
		t.Fatal("retry changed native evidence", err)
	}
	if f.writes != 0 || f.queries.Load() != 2 {
		t.Fatal("readiness wrote resources or skipped authenticated SQL")
	}
	data, _ := json.Marshal(first)
	for _, forbidden := range []string{bootstrapPassword, "fixture-access-key", "fixture-secret-key", "endpointURL", "destinationPath"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("credential/configuration value leaked")
		}
	}
}

func TestBackupReadinessUnavailableNativePrerequisites(t *testing.T) {
	for name, mutate := range map[string]func(*readinessFixture, *Profile){
		"profile missing":      func(f *readinessFixture, p *Profile) { p.BackupProfileRevision = 0 },
		"profile revision":     func(f *readinessFixture, p *Profile) { p.BackupProfileRevision++ },
		"store config missing": func(f *readinessFixture, p *Profile) { p.BackupObjectStore = "" },
		"retiring":             func(f *readinessFixture, p *Profile) { f.retiring = true },
		"forbidden":            func(f *readinessFixture, p *Profile) { f.failPath = "/objectstores/" },
		"wrong source": func(f *readinessFixture, p *Profile) {
			f.cluster.SetLabels(map[string]string{"nortezh.io/database-id": "9"})
		},
		"wrong source operation": func(f *readinessFixture, p *Profile) { f.app.Labels["nortezh.io/operation-id"] = "foreign" },
		"mutable bootstrap":      func(f *readinessFixture, p *Profile) { f.app.Immutable = nil },
		"owned bootstrap":        func(f *readinessFixture, p *Profile) { f.app.OwnerReferences = f.ca.OwnerReferences },
		"source profile": func(f *readinessFixture, p *Profile) {
			_ = unstructured.SetNestedField(f.cluster.Object, "foreign-image", "spec", "imageName")
		},
		"missing plugin": func(f *readinessFixture, p *Profile) {
			unstructured.RemoveNestedField(f.cluster.Object, "spec", "plugins")
		},
		"disabled plugin": func(f *readinessFixture, p *Profile) {
			f.cluster.Object["spec"].(map[string]any)["plugins"].([]any)[0].(map[string]any)["enabled"] = false
		},
		"foreign prefix": func(f *readinessFixture, p *Profile) {
			f.cluster.Object["spec"].(map[string]any)["plugins"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["serverName"] = "foreign"
		},
		"old plugin version": func(f *readinessFixture, p *Profile) {
			f.cluster.Object["status"].(map[string]any)["pluginStatus"].([]any)[0].(map[string]any)["version"] = "0.14.0"
		},
		"archiving false": func(f *readinessFixture, p *Profile) {
			f.cluster.Object["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["status"] = "False"
		},
		"stale generation condition": func(f *readinessFixture, p *Profile) {
			f.cluster.Object["status"].(map[string]any)["conditions"].([]any)[1].(map[string]any)["observedGeneration"] = int64(2)
		},
		"one instance": func(f *readinessFixture, p *Profile) {
			_ = unstructured.SetNestedField(f.cluster.Object, int64(1), "status", "readyInstances")
		},
		"retention": func(f *readinessFixture, p *Profile) {
			_ = unstructured.SetNestedField(f.store.Object, "7d", "spec", "retentionPolicy")
		},
		"unsupported inline config": func(f *readinessFixture, p *Profile) {
			_ = unstructured.SetNestedField(f.store.Object, "fixture-secret-key", "spec", "configuration", "password")
		},
		"unsupported env": func(f *readinessFixture, p *Profile) {
			_ = unstructured.SetNestedSlice(f.store.Object, []any{map[string]any{"name": "AWS_SECRET_ACCESS_KEY", "value": "fixture-secret-key"}}, "spec", "instanceSidecarConfiguration", "env")
		},
		"missing credential key": func(f *readinessFixture, p *Profile) { delete(f.credentials.Data, "ACCESS_SECRET_KEY") },
		"mutable credential without provenance": func(f *readinessFixture, p *Profile) {
			f.credentials.Immutable = nil
			f.credentials.ManagedFields = nil
		},
		"immutable credential without provenance": func(f *readinessFixture, p *Profile) { f.credentials.ManagedFields = nil },
		"changed store without provenance":        func(f *readinessFixture, p *Profile) { f.store.SetGeneration(2) },
		"primary plugin not ready":                func(f *readinessFixture, p *Profile) { f.pod.Status.ContainerStatuses[1].Ready = false },
		"primary unowned":                         func(f *readinessFixture, p *Profile) { f.pod.OwnerReferences = nil },
		"primary image mismatch":                  func(f *readinessFixture, p *Profile) { f.pod.Spec.Containers[0].Image = "foreign-image" },
		"sidecar image mismatch":                  func(f *readinessFixture, p *Profile) { f.pod.Spec.Containers[1].Image = "foreign-image" },
		"missing allocation":                      func(f *readinessFixture, p *Profile) { f.allocation.Data = nil },
		"bad CA":                                  func(f *readinessFixture, p *Profile) { f.ca.Data["ca.crt"] = []byte("invalid CA") },
		"wrong password":                          func(f *readinessFixture, p *Profile) { f.app.Data["password"] = []byte("wrong-password") },
		"wrong TLS hostname":                      func(f *readinessFixture, p *Profile) { f.node.Annotations["kdb.io/host"] = "localhost" },
	} {
		t.Run(name, func(t *testing.T) {
			f, c, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
			mutate(f, &p)
			r, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
			if err != nil || r == nil || r.Available || r.Evidence != nil || r.FailureCode != api.CNPGFailureBackupReadiness || r.Valid() != nil {
				t.Fatal("unsupported prerequisite advertised readiness", err)
			}
			if f.writes != 0 {
				t.Fatal("readiness mutated Kubernetes")
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), "fixture-secret-key") {
				t.Fatal("upstream error leaked")
			}
		})
	}
}

func TestBackupReadinessArchiveAndSnapshotGuards(t *testing.T) {
	for _, name := range []string{"no archive", "pre-configuration archive", "future archive", "changed source", "changed ownership", "changed store", "changed credentials", "changed primary", "new retirement"} {
		t.Run(name, func(t *testing.T) {
			archived := time.Now().Add(-time.Minute).UnixMicro()
			switch name {
			case "no archive":
				archived = 0
			case "pre-configuration archive":
				archived = time.Now().Add(-2 * time.Hour).UnixMicro()
			case "future archive":
				archived = time.Now().Add(time.Hour).UnixMicro()
			}
			f, c, p := newReadinessFixture(t, archived)
			f.after = func(path string, n int) {
				switch {
				case name == "changed source" && strings.HasSuffix(path, "/clusters/cnpg-1-2") && n == 2:
					f.cluster.SetGeneration(2)
				case name == "changed ownership" && strings.HasSuffix(path, "/clusters/cnpg-1-2") && n == 2:
					f.cluster.SetLabels(map[string]string{"nortezh.io/database-id": "9"})
				case name == "changed store" && strings.HasSuffix(path, "/objectstores/backup-store") && n == 2:
					f.store.SetGeneration(2)
				case name == "changed credentials" && strings.HasSuffix(path, "/secrets/s3-creds") && n == 3:
					f.credentials.ResourceVersion = "8"
				case name == "changed primary" && strings.HasSuffix(path, "/pods/cnpg-1-2-1") && n == 2:
					f.pod.UID = "replacement-pod"
				case name == "new retirement" && strings.HasSuffix(path, "/clusters/cnpg-1-2") && n == 2:
					f.retiring = true
				}
			}
			r, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
			if err != nil || r == nil || r.Available || r.Evidence != nil || r.Valid() != nil || f.writes != 0 {
				t.Fatal("unproven archive/snapshot advertised readiness", err)
			}
		})
	}
}
