package cnpg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func backupCommand() *api.DeployerCommandCNPGBackup {
	return &api.DeployerCommandCNPGBackup{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{
		Version: 1, ID: 1, ProjectID: 2, OperationID: "b1111111-1111-4111-8111-111111111111"},
		CreateOperationID: "a1111111-1111-4111-8111-111111111111"}
}

type backupFixture struct {
	mu                     sync.Mutex
	cluster, store, backup map[string]any
	app                    *v1.Secret
	appReads               int
	changeApp              func(*v1.Secret)
	lastRequest            string
	creates                int
	lostResponse           bool
	forbidden              bool
	storeNotFound          bool
	backupLookupBarrier    chan struct{}
	backupLookups          int
}

func newBackupFixture(t *testing.T) (*backupFixture, *k8s.Client, Profile) {
	t.Helper()
	p := Profile{AllocationNamespace: "allocator", PortRange: "6100-6199", PVCRetention: "delete", StorageClass: "hostpath", Image: "registry/postgres:17@sha256:" + strings.Repeat("a", 64)}
	f := &backupFixture{cluster: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "cnpg-1-2", "namespace": "app-database", "uid": "c1111111-1111-4111-8111-111111111111", "labels": map[string]any{"nortezh.io/database-id": "1"}},
		"spec": map[string]any{"imageName": p.Image, "storage": map[string]any{"storageClass": p.StorageClass}, "bootstrap": map[string]any{"initdb": map[string]any{"secret": map[string]any{"name": "cnpg-1-2-app"}}},
			"plugins": []any{map[string]any{"name": barmanPlugin, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "backup-store"}}}}},
		store: map[string]any{"apiVersion": "barmancloud.cnpg.io/v1", "kind": "ObjectStore", "metadata": map[string]any{"name": "backup-store", "uid": "e1111111-1111-4111-8111-111111111111"}, "spec": map[string]any{}},
		app: &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cnpg-1-2-app", Namespace: "app-database", UID: "source-secret-uid", Labels: map[string]string{"nortezh.io/database-id": "1", "nortezh.io/project-id": "2", "nortezh.io/operation-id": backupCommand().CreateOperationID}},
			Immutable:  func() *bool { v := true; return &v }(), Type: v1.SecretTypeBasicAuth,
			Data: map[string][]byte{"username": []byte("app"), "password": []byte(bootstrapPassword)},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/backups/") && r.Method == http.MethodGet && f.backupLookupBarrier != nil {
			f.mu.Lock()
			waitForConcurrentMiss := f.backupLookups < 2
			if waitForConcurrentMiss {
				f.backupLookups++
				barrier := f.backupLookupBarrier
				if f.backupLookups == 2 {
					close(barrier)
				}
				f.mu.Unlock()
				<-barrier
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
				return
			}
			f.mu.Unlock()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastRequest = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		reply := func(x any) {
			if err := json.NewEncoder(w).Encode(x); err != nil {
				t.Error(err)
			}
		}
		if f.forbidden {
			w.WriteHeader(403)
			reply(map[string]any{"kind": "Status", "apiVersion": "v1", "reason": "Forbidden", "code": 403})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters/cnpg-1-2"):
			reply(f.cluster)
		case strings.HasSuffix(r.URL.Path, "/secrets/cnpg-1-2-app"):
			f.appReads++
			if f.appReads == 2 && f.changeApp != nil {
				f.changeApp(f.app)
			}
			if f.app == nil {
				w.WriteHeader(http.StatusNotFound)
				reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
				return
			}
			reply(f.app)
		case strings.HasSuffix(r.URL.Path, "/objectstores/backup-store"):
			if f.storeNotFound {
				w.WriteHeader(http.StatusNotFound)
				reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
				return
			}
			reply(f.store)
		case strings.HasSuffix(r.URL.Path, "/backups") && r.Method == http.MethodPost:
			if f.backup != nil {
				w.WriteHeader(409)
				reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "AlreadyExists", "code": 409})
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&f.backup); err != nil {
				t.Error(err)
				return
			}
			f.backup["metadata"].(map[string]any)["uid"] = "d1111111-1111-4111-8111-111111111111"
			f.creates++
			if f.lostResponse {
				w.WriteHeader(504)
				reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "Timeout", "code": 504})
				return
			}
			w.WriteHeader(201)
			reply(f.backup)
		case strings.Contains(r.URL.Path, "/backups/") && f.backup != nil:
			reply(f.backup)
		default:
			w.WriteHeader(404)
			reply(map[string]any{"apiVersion": "v1", "kind": "Status", "reason": "NotFound", "code": 404})
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

func completedBackupStatus() map[string]any {
	return map[string]any{"phase": "completed", "majorVersion": int64(17), "backupId": "20261015T120000", "endWal": "000000010000000000000004", "endLSN": "0/4000028"}
}

// HTTP fixtures prove reconciliation only, not physical Barman/MinIO execution.
func TestBackupCreateRetryAndCompletion(t *testing.T) {
	f, c, p := newBackupFixture(t)
	it := backupCommand()
	ctx := context.Background()
	first, err := ApplyBackup(ctx, c, it, p)
	if err != nil || first == nil || first.Status != api.CNPGBackupRunning || first.BackupUID == "" {
		t.Fatal("first checkpoint missing", err)
	}
	checkpoint := first.DeployerCommandCNPGBackup
	f.mu.Lock()
	f.backup["status"] = completedBackupStatus()
	f.mu.Unlock()
	for _, command := range []*api.DeployerCommandCNPGBackup{it, &checkpoint, &checkpoint} {
		result, err := ApplyBackup(ctx, c, command, p)
		if err != nil || result == nil || result.Status != api.CNPGBackupCompleted || result.Material.BarmanBackupID != "20261015T120000" || result.Material.ServerName != "cnpg-1-2" {
			t.Fatal("native completion not reported", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 {
		t.Fatal("retry created duplicate Backup", f.creates)
	}
}

func TestBackupConcurrentCreateRacesJoinExistingBackup(t *testing.T) {
	f, c, p := newBackupFixture(t)
	f.backupLookupBarrier = make(chan struct{})
	it := backupCommand()
	type outcome struct {
		result *api.DeployerSetResultCNPGBackup
		err    error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			result, err := ApplyBackup(context.Background(), c, it, p)
			outcomes <- outcome{result, err}
		}()
	}
	var first *api.DeployerSetResultCNPGBackup
	for range 2 {
		got := <-outcomes
		if got.err != nil || got.result == nil || got.result.Status != api.CNPGBackupRunning {
			t.Fatal("concurrent create did not reconcile", got.err)
		}
		if first == nil {
			first = got.result
		} else if got.result.BackupUID != first.BackupUID || got.result.SourceClusterUID != first.SourceClusterUID {
			t.Fatal("concurrent create returned different native identity")
		}
	}
	if f.creates != 1 {
		t.Fatal("concurrent retry created duplicate Backup", f.creates)
	}
}

func TestBackupLostCreateResponseDoesNotDuplicate(t *testing.T) {
	f, c, p := newBackupFixture(t)
	f.lostResponse = true
	if r, err := ApplyBackup(context.Background(), c, backupCommand(), p); err == nil || r != nil {
		t.Fatal("ambiguous create became a receipt")
	}
	if r, err := ApplyBackup(context.Background(), c, backupCommand(), p); err != nil || r == nil || r.Status != api.CNPGBackupRunning {
		t.Fatal("retry did not find original backup", err)
	}
	if f.creates != 1 {
		t.Fatal("ambiguous create duplicated backup")
	}
}

func TestBackupUnknownAndNativeFailure(t *testing.T) {
	for _, mode := range []string{"missing-checkpointed", "replacement-source", "replacement-backup", "foreign-owner", "wrong-plugin", "missing-material", "failed-no-stop", "failed", "transport"} {
		t.Run(mode, func(t *testing.T) {
			f, c, p := newBackupFixture(t)
			first, err := ApplyBackup(context.Background(), c, backupCommand(), p)
			if err != nil {
				t.Fatal(err)
			}
			it := first.DeployerCommandCNPGBackup
			f.mu.Lock()
			switch mode {
			case "missing-checkpointed":
				f.backup = nil
			case "replacement-source":
				f.cluster["metadata"].(map[string]any)["uid"] = "f1111111-1111-4111-8111-111111111111"
			case "replacement-backup":
				f.backup["metadata"].(map[string]any)["uid"] = "f1111111-1111-4111-8111-111111111111"
			case "foreign-owner":
				f.backup["metadata"].(map[string]any)["ownerReferences"] = []any{}
			case "wrong-plugin":
				f.backup["spec"].(map[string]any)["method"] = "barmanObjectStore"
			case "missing-material":
				f.backup["status"] = map[string]any{"phase": "completed", "backupId": "20261015T120000"}
			case "failed-no-stop":
				f.backup["status"] = map[string]any{"phase": "failed", "error": "DO_NOT_ECHO"}
			case "failed":
				f.backup["status"] = map[string]any{"phase": "failed", "stoppedAt": "2026-10-15T12:00:00Z", "error": "DO_NOT_ECHO"}
			case "transport":
				f.forbidden = true
			}
			f.mu.Unlock()
			r, err := ApplyBackup(context.Background(), c, &it, p)
			switch mode {
			case "missing-material":
				if err != nil || r == nil || r.Status != api.CNPGBackupRunning {
					t.Fatal("uncertainty unlocked operation", err)
				}
			case "failed-no-stop", "failed":
				if err != nil || r == nil || r.Status != api.CNPGBackupFailed || r.FailureCode != api.CNPGFailureBackup || r.BackupUID != it.BackupUID || r.SourceClusterUID != it.SourceClusterUID {
					t.Fatal("checkpointed native terminal failure missing", err)
				}
				retry, err := ApplyBackup(context.Background(), c, &it, p)
				if err != nil || retry == nil || retry.Status != r.Status || retry.BackupUID != r.BackupUID || f.creates != 1 {
					t.Fatal("failed retry changed native backup identity", err)
				}
			default:
				if err == nil || r != nil {
					t.Fatal("unknown/mismatch produced a receipt")
				}
			}
			if f.creates != 1 {
				t.Fatal("checkpointed backup was recreated")
			}
		})
	}
}

func TestBackupRequiresConfiguredArchiverAndNoRetention(t *testing.T) {
	for _, mode := range []string{"no-plugin", "missing-objectstore", "rotation", "image", "source-root"} {
		t.Run(mode, func(t *testing.T) {
			f, c, p := newBackupFixture(t)
			it := backupCommand()
			switch mode {
			case "no-plugin":
				delete(f.cluster["spec"].(map[string]any), "plugins")
			case "missing-objectstore":
				f.storeNotFound = true
			case "rotation":
				f.store["spec"].(map[string]any)["retentionPolicy"] = "30d"
			case "image":
				p.Image = "registry/postgres@sha256:" + strings.Repeat("b", 64)
			case "source-root":
				it.CreateOperationID = "f1111111-1111-4111-8111-111111111111"
			}
			if r, err := ApplyBackup(context.Background(), c, it, p); err == nil || r != nil || f.creates != 0 {
				t.Fatal("unsupported source wrote backup")
			}
		})
	}
}
