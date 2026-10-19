package cnpg

import (
	"context"
	"strings"
	"testing"

	"github.com/Nortezh/api"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRestoreRevalidatesSourceSecretBeforeTargetAccess(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "name", "namespace", "uid", "database", "project", "operation", "type", "mutable", "no-immutable", "deleting", "owner", "username", "missing-password", "empty-password", "extra-data", "string-data"} {
		t.Run(mode, func(t *testing.T) {
			f, c, p := newBackupFixture(t)
			first, err := ApplyBackup(context.Background(), c, backupCommand(), p)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.backup["status"] = completedBackupStatus()
			f.mu.Unlock()
			completed, err := ApplyBackup(context.Background(), c, &first.DeployerCommandCNPGBackup, p)
			if err != nil || completed.Material == nil {
				t.Fatal("backup fixture not complete")
			}
			it := &api.DeployerCommandCNPGRestore{
				DeployerCommandCNPGCreate: api.DeployerCommandCNPGCreate{
					DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 3, ProjectID: 2, OperationID: "f1111111-1111-4111-8111-111111111111"},
					Name:                        "restored", Username: "app", Database: "app", StorageSize: 1024, SecretID: 4, SecretRevision: 1,
				},
				RestoreOperationID: "81111111-1111-4111-8111-111111111111", Backup: completed.DeployerCommandCNPGBackup, Material: *completed.Material,
			}
			if err := it.Valid(); err != nil {
				t.Fatal("invalid restore fixture", err)
			}
			f.mu.Lock()
			f.appReads = 0
			// Change only the second read, after ApplyBackup has accepted the source.
			f.changeApp = func(s *v1.Secret) {
				switch mode {
				case "missing":
					f.app = nil
				case "name":
					s.Name = "foreign"
				case "namespace":
					s.Namespace = "foreign"
				case "uid":
					s.UID = ""
				case "database":
					s.Labels["nortezh.io/database-id"] = "9"
				case "project":
					s.Labels["nortezh.io/project-id"] = "9"
				case "operation":
					s.Labels["nortezh.io/operation-id"] = "foreign"
				case "type":
					s.Type = v1.SecretTypeOpaque
				case "mutable":
					v := false
					s.Immutable = &v
				case "no-immutable":
					s.Immutable = nil
				case "deleting":
					now := metav1.Now()
					s.DeletionTimestamp = &now
				case "owner":
					s.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}}
				case "username":
					s.Data["username"] = []byte("foreign")
				case "missing-password":
					delete(s.Data, "password")
				case "empty-password":
					s.Data["password"] = nil
				case "extra-data":
					s.Data["extra"] = []byte("fixture")
				case "string-data":
					s.StringData = map[string]string{"password": "fixture"}
				}
			}
			f.mu.Unlock()
			_, _, ready, err := ApplyRestore(context.Background(), c, it, p)
			if ready || err != errBootstrapUnavailable || strings.Contains(err.Error(), bootstrapPassword) {
				t.Fatal("unsafe restore result")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			wantPath := "/api/v1/namespaces/app-database/secrets/cnpg-1-2-app"
			// A valid source reaches the next boundary: the absent target synced credential.
			if mode == "valid" {
				wantPath = "/api/v1/namespaces/app/secrets/secrets-2"
			}
			if f.appReads != 2 || f.lastRequest != wantPath || f.creates != 1 {
				t.Fatal("invalid source reached target access or changed backup history")
			}
		})
	}
}
