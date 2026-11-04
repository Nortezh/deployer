package cnpg

import (
	"context"
	"testing"
	"time"

	"github.com/Nortezh/api"
)

func recoveredBackupSpec(spec map[string]any) {
	initial := spec["bootstrap"].(map[string]any)["initdb"].(map[string]any)
	initial["source"] = "selected-backup"
	initial["recoveryTarget"] = map[string]any{"backupID": "20261014T120000", "targetImmediate": true}
	spec["bootstrap"] = map[string]any{"recovery": initial}
	spec["externalClusters"] = []any{map[string]any{"name": "selected-backup", "plugin": map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": false, "parameters": map[string]any{"barmanObjectName": "backup-store", "serverName": "cnpg-source"}}}}
}

func TestRestoredSourceBackupAndReadiness(t *testing.T) {
	t.Run("backup", func(t *testing.T) {
		f, c, p := newBackupFixture(t)
		recoveredBackupSpec(f.cluster["spec"].(map[string]any))
		first, err := ApplyBackup(context.Background(), c, backupCommand(), p)
		if err != nil || first == nil {
			t.Fatal("restored source could not create own backup", err)
		}
		f.backup["status"] = completedBackupStatus()
		done, err := ApplyBackup(context.Background(), c, &first.DeployerCommandCNPGBackup, p)
		if err != nil || done == nil || done.Status != api.CNPGBackupCompleted || done.Material.ServerName != "cnpg-1-2" || done.Material.Archive == nil {
			t.Fatal("restored source completion missing own archive", err)
		}
	})
	t.Run("readiness", func(t *testing.T) {
		f, c, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
		recoveredBackupSpec(f.cluster.Object["spec"].(map[string]any))
		observed, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
		if err != nil || observed == nil || !observed.Available || observed.Evidence.ServerName != "cnpg-1-2" {
			t.Fatal("restored source not read-only backup ready", err)
		}
	})
}

func TestRestoredSourceRejectsAmbiguousBootstrapAndSourceReuse(t *testing.T) {
	for _, mode := range []string{"mixed-bootstrap", "foreign-secret", "source-prefix", "different-source-store", "unknown-source", "latest-fallback", "read-write-source", "target-source-prefix"} {
		t.Run(mode, func(t *testing.T) {
			f, c, p := newBackupFixture(t)
			spec := f.cluster["spec"].(map[string]any)
			recoveredBackupSpec(spec)
			recovery := spec["bootstrap"].(map[string]any)["recovery"].(map[string]any)
			source := spec["externalClusters"].([]any)[0].(map[string]any)
			plugin := source["plugin"].(map[string]any)
			params := plugin["parameters"].(map[string]any)
			switch mode {
			case "mixed-bootstrap":
				spec["bootstrap"].(map[string]any)["initdb"] = map[string]any{}
			case "foreign-secret":
				recovery["secret"] = map[string]any{"name": "cnpg-source-app"}
			case "source-prefix":
				params["serverName"] = "cnpg-1-2"
			case "different-source-store":
				params["barmanObjectName"] = "unknown-store"
			case "unknown-source":
				recovery["source"] = "foreign"
			case "latest-fallback":
				delete(recovery, "recoveryTarget")
			case "read-write-source":
				plugin["isWALArchiver"] = true
			case "target-source-prefix":
				spec["plugins"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["serverName"] = "cnpg-source"
			}
			if result, err := ApplyBackup(context.Background(), c, backupCommand(), p); err == nil || result != nil || f.creates != 0 {
				t.Fatal("unsupported restored source admitted backup")
			}
		})
	}
}

func TestBackupArchiveBindingRejectsConfigurationDriftAndPreservesLegacy(t *testing.T) {
	for _, mode := range []string{"destination", "endpoint", "credential-revision", "profile-revision", "recovery-bootstrap", "source-secret-replacement", "historical-unbound"} {
		t.Run(mode, func(t *testing.T) {
			f, c, p := newBackupFixture(t)
			if mode == "recovery-bootstrap" {
				recoveredBackupSpec(f.cluster["spec"].(map[string]any))
			}
			first, err := ApplyBackup(context.Background(), c, backupCommand(), p)
			if err != nil || first == nil {
				t.Fatal(err)
			}
			f.backup["status"] = completedBackupStatus()
			config := f.store["spec"].(map[string]any)["configuration"].(map[string]any)
			switch mode {
			case "destination":
				config["destinationPath"] = "s3://other-backups/new-prefix"
			case "endpoint":
				config["endpointURL"] = "http://other-minio:9000"
			case "credential-revision":
				f.credentials.ResourceVersion = "8"
			case "profile-revision":
				p.BackupProfileRevision++
			case "recovery-bootstrap":
				f.cluster["spec"].(map[string]any)["bootstrap"].(map[string]any)["recovery"].(map[string]any)["recoveryTarget"].(map[string]any)["backupID"] = "20261013T120000"
			case "source-secret-replacement":
				f.app.UID = "replaced-source-secret"
			case "historical-unbound":
				delete(f.backup["metadata"].(map[string]any)["annotations"].(map[string]any), "nortezh.io/backup-archive")
			}
			result, err := ApplyBackup(context.Background(), c, &first.DeployerCommandCNPGBackup, p)
			if mode == "historical-unbound" {
				if err != nil || result == nil || result.Status != api.CNPGBackupCompleted || result.Material.Archive != nil {
					t.Fatal("historical material was guessed or abandoned", err)
				}
			} else if err == nil || result != nil {
				t.Fatal("same-UID configuration drift produced completion")
			}
			if f.creates != 1 {
				t.Fatal("drift recreated Backup")
			}
		})
	}
}
