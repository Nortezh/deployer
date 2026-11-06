package cnpg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"io"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

type backupCreateFixture struct {
	mu                                               sync.Mutex
	profile                                          *readinessFixture
	command                                          *api.DeployerCommandCNPGCreate
	objects                                          map[string]map[string]any
	clusterCreates, otherWrites, storeReads, patches int
	losePatchResponse, denyPatch                     bool
	storeMissing, storeDenied, loseCreateResponse    bool
}

// Tests the shared create/render boundary; Kubernetes and SQL remain synthetic.
func newBackupCreateFixture(t *testing.T, command *api.DeployerCommandCNPGCreate) (*backupCreateFixture, *k8s.Client, Profile) {
	t.Helper()
	profile, _, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
	f := &backupCreateFixture{profile: profile, command: command, objects: map[string]map[string]any{}}
	name := createName(command)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		reply := func(value any) {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Error(err)
			}
		}
		status := func(code int, reason string) {
			w.WriteHeader(code)
			reply(map[string]any{"apiVersion": "v1", "kind": "Status", "code": code, "reason": reason})
		}
		if r.Method == http.MethodPatch {
			f.patches++
			if f.denyPatch {
				status(403, "Forbidden")
				return
			}
			object := f.objects[r.URL.Path]
			if object == nil {
				status(404, "NotFound")
				return
			}
			before, _ := json.Marshal(object)
			body, _ := io.ReadAll(r.Body)
			patch, err := jsonpatch.DecodePatch(body)
			if err != nil {
				t.Fatal(err)
			}
			after, err := patch.Apply(before)
			if err != nil {
				status(409, "Conflict")
				return
			}
			if err := json.Unmarshal(after, &object); err != nil {
				t.Fatal(err)
			}
			object["metadata"].(map[string]any)["generation"] = float64(2)
			f.objects[r.URL.Path] = object
			if f.losePatchResponse {
				f.losePatchResponse = false
				status(504, "Timeout")
				return
			}
			reply(object)
			return
		}
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/clusters") || strings.HasSuffix(r.URL.Path, "/ingressroutetcps")) {
			var object map[string]any
			if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
				t.Error(err)
				status(400, "BadRequest")
				return
			}
			metadata := object["metadata"].(map[string]any)
			key := r.URL.Path + "/" + metadata["name"].(string)
			if f.objects[key] != nil {
				status(409, "AlreadyExists")
				return
			}
			if object["kind"] == "Cluster" {
				f.clusterCreates++
				metadata["uid"], metadata["resourceVersion"], metadata["generation"] = "c1111111-1111-4111-8111-111111111111", "1", int64(1)
				if plugins, _, _ := unstructured.NestedSlice(object, "spec", "plugins"); len(plugins) != 0 {
					t.Error("archiver enabled before actual UID binding")
				}
				object["status"] = map[string]any{"phase": "Setting up primary", "readyInstances": int64(1), "currentPrimary": name + "-1"}
			}
			f.objects[key] = object
			if object["kind"] == "Cluster" && f.loseCreateResponse {
				f.loseCreateResponse = false
				status(504, "Timeout")
				return
			}
			w.WriteHeader(201)
			reply(object)
			return
		}
		if r.Method != http.MethodGet {
			f.otherWrites++
			status(403, "Forbidden")
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters"):
			items := []any{}
			for path, obj := range f.objects {
				if strings.Contains(path, "/clusters/") {
					items = append(items, obj)
				}
			}
			reply(map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "ClusterList", "items": items})
		case strings.HasSuffix(r.URL.Path, "/nodes"):
			reply(&v1.NodeList{Items: []v1.Node{*profile.node}})
		case strings.HasSuffix(r.URL.Path, "/configmaps/"+allocationMap):
			data := map[string]string{}
			for key := range profile.allocation.Data {
				data[key] = allocationOwner("app-database", name, command.ID)
			}
			reply(&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationMap, Namespace: "allocator", ResourceVersion: "1"}, Data: data})
		case r.URL.Path == "/api/v1/namespaces/app/secrets/secrets-2":
			reply(&v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "secrets-2", Namespace: "app", Labels: map[string]string{"id": "secrets-2", "projectId": "2"}, Annotations: map[string]string{"nortezh.io/secret-revision-3": "1"}}, Type: v1.SecretTypeOpaque, Data: map[string][]byte{"secret-3": []byte(bootstrapPassword)}})
		case strings.HasSuffix(r.URL.Path, "/secrets/"+name+"-app"):
			app := profile.app.DeepCopy()
			app.Name = name + "-app"
			app.Labels = map[string]string{"nortezh.io/database-id": strconv.FormatInt(command.ID, 10), "nortezh.io/project-id": "2", "nortezh.io/operation-id": command.OperationID, "nortezh.io/secret-id": "3", "nortezh.io/secret-revision": "1"}
			reply(app)
		case strings.HasSuffix(r.URL.Path, "/objectstores/backup-store"):
			f.storeReads++
			if f.storeMissing {
				status(404, "NotFound")
				return
			}
			if f.storeDenied {
				status(403, "Forbidden")
				return
			}
			reply(profile.store)
		case strings.HasSuffix(r.URL.Path, "/secrets/s3-creds"):
			reply(profile.credentials)
		default:
			if object := f.objects[r.URL.Path]; object != nil {
				reply(object)
				return
			}
			status(404, "NotFound")
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("KUBE_PROXY_URL", server.URL)
	c, err := k8s.NewClientForConfig("app", &rest.Config{Host: server.URL, QPS: 10000, Burst: 10000})
	if err != nil {
		t.Fatal(err)
	}
	return f, c, p
}

func (f *backupCreateFixture) cluster() *unstructured.Unstructured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return (&unstructured.Unstructured{Object: f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/"+createName(f.command)]}).DeepCopy()
}

func TestCreateBarmanBindingAndLostResponseRetry(t *testing.T) {
	it := bootstrapCommand()
	f, c, p := newBackupCreateFixture(t, it)
	f.loseCreateResponse = true
	if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err == nil || ready {
		t.Fatal("lost response was treated as confirmed creation")
	}
	if plugins, _, _ := unstructured.NestedSlice(f.cluster().Object, "spec", "plugins"); len(plugins) != 0 {
		t.Fatal("lost-create response enabled unbound archiving")
	}
	f.losePatchResponse = true
	if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err == nil || ready {
		t.Fatal("lost activation ACK was treated as confirmed")
	}
	plugins, _, err := unstructured.NestedSlice(f.cluster().Object, "spec", "plugins")
	var binding archiveBinding
	if json.Unmarshal([]byte(f.cluster().GetAnnotations()[archiveBindingAnnotation]), &binding) != nil || binding.ClusterUID != string(f.cluster().GetUID()) || binding.CreateOperationID != it.OperationID || err != nil || !reflect.DeepEqual(plugins, archivePlugins(&binding)) {
		t.Fatal("archiver and actual UID binding were not committed together")
	}
	before, _ := json.Marshal(f.cluster().Object)
	for range 2 {
		if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err != nil || ready {
			t.Fatal("retry did not reconcile the same bound root", err)
		}
	}
	after, _ := json.Marshal(f.cluster().Object)
	if string(before) != string(after) || f.clusterCreates != 1 || f.otherWrites != 0 || f.patches != 1 {
		t.Fatal("retry changed/recreated/rebound the Cluster")
	}
	f.storeDenied = true
	if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err == nil || ready {
		t.Fatal("lost required profile observation accepted")
	}
	for _, secret := range []string{bootstrapPassword, "fixture-access-key", "fixture-secret-key"} {
		if strings.Contains(string(after), secret) {
			t.Fatal("credential value entered the Cluster manifest")
		}
	}
}

func TestCreateWithoutBackupPrerequisitesAndNoSilentBackfill(t *testing.T) {
	for _, mode := range []string{"disabled", "incomplete", "missing store setting", "missing store", "forbidden store", "unsupported store", "missing credentials", "unknown credential provenance"} {
		t.Run(mode, func(t *testing.T) {
			it := bootstrapCommand()
			f, c, p := newBackupCreateFixture(t, it)
			store, credentials := f.profile.store.DeepCopy(), f.profile.credentials.DeepCopy()
			switch mode {
			case "disabled":
				p.BackupProfileRevision = 0
				p.BackupObjectStore = ""
			case "incomplete":
				p.BackupProfileRevision = 0
			case "missing store setting":
				p.BackupObjectStore = ""
			case "missing store":
				f.storeMissing = true
			case "forbidden store":
				f.storeDenied = true
			case "unsupported store":
				_ = unstructured.SetNestedField(f.profile.store.Object, "7d", "spec", "retentionPolicy")
			case "missing credentials":
				delete(f.profile.credentials.Data, "ACCESS_SECRET_KEY")
			case "unknown credential provenance":
				f.profile.credentials.ManagedFields = nil
			}
			if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err != nil || ready {
				t.Fatal("optional backup preflight blocked ordinary provisioning", err)
			}
			if _, exists, _ := unstructured.NestedSlice(f.cluster().Object, "spec", "plugins"); exists {
				t.Fatal("unverified backup profile attached")
			}
			before, _ := json.Marshal(f.cluster().Object)
			reads := f.storeReads
			p.BackupProfileRevision = 1
			p.BackupObjectStore = "backup-store"
			f.storeMissing, f.storeDenied = false, false
			f.profile.store, f.profile.credentials = store, credentials
			if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err != nil || ready {
				t.Fatal("existing Cluster could not reconcile", err)
			}
			after, _ := json.Marshal(f.cluster().Object)
			if string(before) != string(after) || f.clusterCreates != 1 || f.otherWrites != 0 || f.storeReads != reads {
				t.Fatal("existing Cluster was silently patched/backfilled")
			}
		})
	}
}

func TestRestoreCreationSeparatesArchiveFromSelectedSource(t *testing.T) {
	for _, mode := range []string{"shared store", "different store", "source prefix collision"} {
		t.Run(mode, func(t *testing.T) {
			it := bootstrapCommand()
			it.ID = 3
			it.OperationID = "f1111111-1111-4111-8111-111111111111"
			f, c, p := newBackupCreateFixture(t, it)
			sourceStore, sourceServer := "backup-store", "cnpg-1-2"
			if mode == "different store" {
				sourceStore = "source-store"
			}
			if mode == "source prefix collision" {
				sourceServer = createName(it)
			}
			recovery := &api.DeployerCommandCNPGRestore{DeployerCommandCNPGCreate: *it, Material: api.CNPGBackupMaterial{ObjectStoreName: sourceStore, ServerName: sourceServer, BarmanBackupID: "20261015T120000"}}
			cfg := &api.DeployerCommandDatabaseCreate{ID: it.ID, ProjectID: it.ProjectID, Name: "cnpg-3", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG, StorageSize: it.StorageSize, Resources: it.Resources, PostgresConfig: &api.DatabaseConfigPostgres{User: "app", Password: bootstrapPassword, Database: "app"}}
			// Shared rendering only; public ApplyRestore still validates the selected
			// source/backup and fresh target credentials before reaching this seam.
			if _, _, ready, err := apply(context.Background(), c, cfg, p, recovery, it); err != nil || ready {
				t.Fatal("recovery rendering failed", err)
			}
			external, _, err := unstructured.NestedSlice(f.cluster().Object, "spec", "externalClusters")
			wantSource := []any{map[string]any{"name": "selected-backup", "plugin": map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": false, "parameters": map[string]any{"barmanObjectName": sourceStore, "serverName": sourceServer}}}}
			if err != nil || !reflect.DeepEqual(external, wantSource) || str(f.cluster(), "spec", "bootstrap", "recovery", "recoveryTarget", "backupID") != "20261015T120000" {
				t.Fatal("selected recovery source/backup was changed")
			}
			plugins, exists, err := unstructured.NestedSlice(f.cluster().Object, "spec", "plugins")
			if mode == "source prefix collision" || mode == "different store" {
				if exists || err != nil {
					t.Fatal("target archive reused the selected source prefix")
				}
			} else {
				want := []any{map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "backup-store", "serverName": "cnpg-3-2"}}}
				if err != nil || !reflect.DeepEqual(plugins, want) {
					t.Fatal("target did not receive its own archive")
				}
			}
			if f.clusterCreates != 1 || f.otherWrites != 0 {
				t.Fatal("recovery rendering mutated other resources")
			}
		})
	}
}
