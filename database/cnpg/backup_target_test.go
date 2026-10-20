package cnpg

import (
	"context"
	"testing"

	"github.com/Nortezh/api"
)

func TestBackupTargetsPrimaryAndPreservesExistingTarget(t *testing.T) {
	f, c, p := newBackupFixture(t)
	first, err := ApplyBackup(context.Background(), c, backupCommand(), p)
	if err != nil || first == nil {
		t.Fatal("new backup not created", err)
	}
	f.mu.Lock()
	spec := f.backup["spec"].(map[string]any)
	target := spec["target"]
	f.mu.Unlock()
	if target != "primary" {
		t.Fatal("new backup must use primary so stock pg_backup_stop waits for WAL")
	}
	// Existing immutable Backup resources keep their original target and identity.
	for _, legacy := range []string{"", "prefer-standby"} {
		f.mu.Lock()
		if legacy == "" {
			delete(spec, "target")
		} else {
			spec["target"] = legacy
		}
		f.backup["status"] = completedBackupStatus()
		f.mu.Unlock()
		result, err := ApplyBackup(context.Background(), c, &first.DeployerCommandCNPGBackup, p)
		if err != nil || result == nil || result.Status != api.CNPGBackupCompleted || result.BackupUID != first.BackupUID {
			t.Fatal("existing backup reconciliation changed", err)
		}
		f.mu.Lock()
		unchanged := spec["target"] == legacy || legacy == "" && spec["target"] == nil
		creates := f.creates
		f.mu.Unlock()
		if !unchanged || creates != 1 {
			t.Fatal("existing immutable target was rewritten or backup duplicated")
		}
	}
}
