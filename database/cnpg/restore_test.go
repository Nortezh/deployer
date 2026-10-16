package cnpg

import (
	"context"
	"testing"

	"github.com/Nortezh/api"
)

func TestRestoreRejectsChangedSelectedBackupBeforeTargetWrites(t *testing.T) {
	f, c, p := newBackupFixture(t)
	first, err := ApplyBackup(context.Background(), c, backupCommand(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.backup["status"] = completedBackupStatus()
	f.mu.Unlock()
	completed, err := ApplyBackup(context.Background(), c, &first.DeployerCommandCNPGBackup, p)
	if err != nil {
		t.Fatal(err)
	}
	target := api.DeployerCommandCNPGCreate{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 3, ProjectID: 2, OperationID: "f1111111-1111-4111-8111-111111111111"}, Name: "restored", Username: "app", Database: "app", StorageSize: 1024, SecretID: 4, SecretRevision: 1}
	for _, change := range []func(*api.DeployerCommandCNPGRestore){
		func(m *api.DeployerCommandCNPGRestore) { m.Material.BarmanBackupID = "20261016T120000" },
		func(m *api.DeployerCommandCNPGRestore) {
			m.Backup.SourceClusterUID = "91111111-1111-4111-8111-111111111111"
		},
		func(m *api.DeployerCommandCNPGRestore) {
			m.Material.Image = "registry/other:17@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
	} {
		it := api.DeployerCommandCNPGRestore{DeployerCommandCNPGCreate: target, RestoreOperationID: "81111111-1111-4111-8111-111111111111", Backup: completed.DeployerCommandCNPGBackup, Material: *completed.Material}
		change(&it)
		if _, _, ready, err := ApplyRestore(context.Background(), c, &it, p); err == nil || ready {
			t.Fatal("changed selection reached target creation")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 {
		t.Fatal("restore created another Backup")
	}
}
