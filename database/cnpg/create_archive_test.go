package cnpg

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProducerActivationRejectsDriftWithoutRebinding(t *testing.T) {
	for _, mode := range []string{"patch-denied", "binding-removed", "fence-removed", "fence-changed", "store-generation", "destination", "credential", "profile", "create-operation", "cluster-uid", "cluster-generation", "bootstrap-uid", "preexisting-archiver", "foreign-physical-prefix", "foreign-alias-store"} {
		t.Run(mode, func(t *testing.T) {
			it := bootstrapCommand()
			f, c, p := newBackupCreateFixture(t, it)
			f.loseCreateResponse = true
			if _, _, _, err := ApplyCreate(context.Background(), c, it, p); err == nil {
				t.Fatal("fixture did not lose create ACK")
			}
			switch mode {
			case "patch-denied":
				f.denyPatch = true
			case "binding-removed":
				delete(f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["metadata"].(map[string]any)["annotations"].(map[string]any), archiveBindingAnnotation)
			case "fence-removed":
				delete(f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["metadata"].(map[string]any)["annotations"].(map[string]any), reconcileLoopAnnotation)
			case "fence-changed":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["metadata"].(map[string]any)["annotations"].(map[string]any)[reconcileLoopAnnotation] = "enabled"
			case "store-generation":
				f.profile.store.SetGeneration(2)
			case "destination":
				_ = unstructured.SetNestedField(f.profile.store.Object, "s3://cnpg-backups/changed", "spec", "configuration", "destinationPath")
			case "credential":
				f.profile.credentials.ResourceVersion = "8"
			case "profile":
				p.BackupProfileRevision++
			case "create-operation":
				it.OperationID = "e1111111-1111-4111-8111-111111111111"
			case "cluster-uid":
				// A finalized binding cannot follow a replacement root.
				root := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]
				var binding archiveBinding
				_ = json.Unmarshal([]byte(root["metadata"].(map[string]any)["annotations"].(map[string]any)[archiveBindingAnnotation].(string)), &binding)
				binding.ClusterUID = "d1111111-1111-4111-8111-111111111111"
				root["metadata"].(map[string]any)["annotations"].(map[string]any)[archiveBindingAnnotation] = encodeBinding(&binding)
			case "cluster-generation":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["metadata"].(map[string]any)["generation"] = int64(3)
			case "bootstrap-uid":
				f.profile.app.UID = "replaced-bootstrap"
			case "preexisting-archiver":
				root := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]
				root["spec"].(map[string]any)["plugins"] = []any{map[string]any{"name": barmanPlugin, "isWALArchiver": true}}
			case "foreign-physical-prefix", "foreign-alias-store":
				storeName := "backup-store"
				if mode == "foreign-alias-store" {
					alias := f.profile.store.DeepCopy()
					alias.SetName("alias-store")
					f.objects["/apis/barmancloud.cnpg.io/v1/namespaces/app-database/objectstores/alias-store"] = alias.Object
					storeName = "alias-store"
				}
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/foreign"] = map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "foreign", "uid": "foreign"}, "spec": map[string]any{"plugins": []any{map[string]any{"name": barmanPlugin, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": storeName, "serverName": "cnpg-1-2"}}}}}
			}
			before, _ := json.Marshal(f.cluster().Object)
			if _, _, ready, err := ApplyCreate(context.Background(), c, it, p); err == nil || ready {
				t.Fatal("drift silently activated/rebound archive")
			}
			after, _ := json.Marshal(f.cluster().Object)
			if string(before) != string(after) || f.clusterCreates != 1 {
				t.Fatal("drift changed the persisted root")
			}
			if mode != "patch-denied" && f.patches != 0 {
				t.Fatal("patch attempted after drift")
			}
			if mode == "patch-denied" && f.cluster().GetAnnotations()[reconcileLoopAnnotation] != "disabled" {
				t.Fatal("denied activation removed the pre-Pod fence")
			}
		})
	}
}

func TestBoundProducerRejectsRefencing(t *testing.T) {
	it := bootstrapCommand()
	f, c, p := newBackupCreateFixture(t, it)
	if _, _, _, err := ApplyCreate(context.Background(), c, it, p); err != nil {
		t.Fatal(err)
	}
	root := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]
	root["metadata"].(map[string]any)["annotations"].(map[string]any)[reconcileLoopAnnotation] = "disabled"
	before, _ := json.Marshal(f.cluster().Object)
	if _, _, _, err := ApplyCreate(context.Background(), c, it, p); err == nil {
		t.Fatal("bound root with a new fence was accepted")
	}
	after, _ := json.Marshal(f.cluster().Object)
	if string(before) != string(after) || f.patches != 1 {
		t.Fatal("refenced root was changed")
	}
}

func TestProducerLegacyArchiverIsNotEnrolled(t *testing.T) {
	it := bootstrapCommand()
	f, c, p := newBackupCreateFixture(t, it)
	p.BackupProfileRevision = 0
	if _, _, _, err := ApplyCreate(context.Background(), c, it, p); err != nil {
		t.Fatal(err)
	}
	root := f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]
	root["spec"].(map[string]any)["plugins"] = []any{map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": "backup-store", "serverName": "cnpg-1-2"}}}
	p.BackupProfileRevision = 1
	if _, _, _, err := ApplyCreate(context.Background(), c, it, p); err != nil {
		t.Fatal(err)
	}
	if f.cluster().GetAnnotations()[archiveBindingAnnotation] != "" || f.patches != 0 {
		t.Fatal("legacy archiver silently enrolled")
	}
	data, _ := json.Marshal(f.cluster().Object)
	if strings.Contains(string(data), "fixture-secret-key") {
		t.Fatal("credential value leaked")
	}
}
