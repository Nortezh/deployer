package cnpg

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Nortezh/api"
)

const deleteRootPath = "/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"
const deleteRecordPath = "/api/v1/namespaces/app-database/configmaps/cnpg-1-2-retirement"

func TestDeleteNativeShutdownEvidence(t *testing.T) {
	for _, mode := range []string{"smart-fast", "shutdown-failed", "late-shutdown-failure", "sidecar-failed", "stale-log", "wrong-order", "stale-container", "stale-terminal", "restart", "watch-denied", "watch-lost", "log-denied", "log-lost", "unknown-writer", "no-terminal"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			f.stopMode = mode
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if mode == "no-terminal" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			}
			defer cancel()
			done, err := ApplyDelete(ctx, c, deleteCommand(), deleteProfile())
			if mode == "smart-fast" {
				if err != nil || done || f.stops != 1 || len(f.deletes) != 1 {
					t.Fatal("supported fast fallback did not retire only the root", err)
				}
				return
			}
			if err == nil || done || DeleteFailureStage(err) != api.CNPGDeleteWriters {
				t.Fatal("unsupported/missing shutdown evidence advanced", err)
			}
			if len(f.deletes) != 0 || f.archive.deletes != 0 {
				t.Fatal("mutated resources/archives without shutdown proof")
			}
			// Once native Pods disappear, missing observations cannot be guessed from absence on restart.
			if f.stops == 1 {
				if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err == nil {
					t.Fatal("restart fabricated lost native evidence")
				}
			}
		})
	}
}

func TestDeleteArchiveProfilesAndPaginationErrors(t *testing.T) {
	for _, mode := range []string{"versioned", "suspended", "lock", "replication", "denied-versioning", "denied-lock", "denied-replication", "list", "foreign key", "missing token", "per-key", "unsupported profile"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			p := deleteProfile()
			switch mode {
			case "versioned":
				f.archive.versioning = "Enabled"
			case "suspended":
				f.archive.versioning = "Suspended"
			case "lock":
				f.archive.lock = true
			case "replication":
				f.archive.replication = true
			case "denied-versioning":
				f.archive.denied = "versioning"
			case "denied-lock":
				f.archive.denied = "object-lock"
			case "denied-replication":
				f.archive.denied = "replication"
			case "foreign key", "missing token":
				f.archive.malformed = mode
				f.archive.pageSize = 1
			case "list", "per-key":
				f.archive.failure = mode
			case "unsupported profile":
				p.ArchiveDeleteProvider = "r2"
			}
			done, err := ApplyDelete(context.Background(), c, deleteCommand(), p)
			if err == nil || done || len(f.deletes) != 0 {
				t.Fatal("unsupported/unknown archive outcome advanced resource retirement", err)
			}
			if mode != "per-key" && mode != "missing token" && f.archive.deletes != 0 {
				t.Fatal("object mutation before complete page/profile validation")
			}
			if f.archive.objects["staging/source/wals/control"] != "source" || f.archive.objects["staging/cnpg-1-2-neighbor/wals/control"] != "neighbor" {
				t.Fatal("source/neighbor scope changed")
			}
		})
	}
}

func TestDeleteSameOperationPartialRestartReceipt(t *testing.T) {
	c, f := newDeleteFixture(t)
	it, p := deleteCommand(), deleteProfile()
	f.archive.pageSize = 1
	f.archive.objects["staging/cnpg-1-2/wals/three"] = "wal-only"
	f.archive.failBatch = 2
	if done, err := ApplyDelete(context.Background(), c, it, p); done || err == nil || DeleteFailureStage(err) != api.CNPGDeleteArchives {
		t.Fatal("partial object error was accepted", err)
	}
	if f.archive.deletes != 2 || len(f.deletes) != 0 || f.stops != 1 {
		t.Fatal("partial cleanup lost its native-stop/reservation boundary")
	}
	before := f.objects[deleteRecordPath]["data"].(map[string]any)["inventory"]
	f.archive.failBatch = 0
	f.loseProgressACK = true
	if done, err := ApplyDelete(context.Background(), c, it, p); done || err != nil {
		t.Fatal("same operation did not resume partial cleanup", err)
	}
	if f.stops != 1 || f.objects[deleteRecordPath]["data"].(map[string]any)["inventory"] != before {
		t.Fatal("retry repeated stop or rebound immutable scope")
	}
	// Fake stock GC/reclaim; the product never explicitly deletes PVC/PV/owned children.
	f.mu.Lock()
	for path := range f.objects {
		if strings.Contains(path, "/persistentvolumeclaims/") || strings.Contains(path, "/persistentvolumes/") {
			delete(f.objects, path)
		}
	}
	f.mu.Unlock()
	for attempt := 0; attempt < 5; attempt++ {
		done, err := ApplyDelete(context.Background(), c, it, p)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if attempt == 4 {
			t.Fatal("resource stage never completed")
		}
	}
	receipt, err := DeleteReceipt(context.Background(), c, it)
	if err != nil || receipt.Valid() != nil || !receipt.WriterStopped || !receipt.ArchiveAbsent {
		t.Fatal("missing archive-aware durable receipt", err)
	}
	replay, err := DeleteReceipt(context.Background(), c, it)
	if err != nil || *replay != *receipt {
		t.Fatal("root-free replay changed receipt", err)
	}
	if f.archive.objects["staging/source/wals/control"] != "source" || f.archive.objects["staging/cnpg-1-2-neighbor/wals/control"] != "neighbor" {
		t.Fatal("source or neighbor was removed")
	}
	final := *it
	final.Phase = api.CNPGDeleteFinalize
	wrong := *receipt
	wrong.ArchiveBindingHash = strings.Repeat("f", 64)
	final.Receipt = &wrong
	if done, err := ApplyDelete(context.Background(), c, &final, p); done || err == nil {
		t.Fatal("foreign finalize receipt released reservation")
	}
	final.Receipt = receipt
	for range 2 {
		if done, err := ApplyDelete(context.Background(), c, &final, p); !done || err != nil {
			t.Fatal("same receipt finalize/replay failed", err)
		}
	}
}

func TestDeleteCapturesOnlyOwnedTerminalBackups(t *testing.T) {
	for _, phase := range []string{"completed", "failed", "running", "foreign"} {
		t.Run(phase, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			uid := "c1111111-1111-4111-8111-111111111111"
			if phase == "foreign" {
				uid = "foreign"
			}
			f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/backups/target"] = map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup", "metadata": map[string]any{"name": "target", "namespace": "app-database", "uid": "backup-uid", "resourceVersion": "1", "labels": map[string]any{"nortezh.io/database-id": "1", "nortezh.io/project-id": "2"}, "ownerReferences": []any{map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "cnpg-1-2", "uid": uid}}}, "spec": map[string]any{"cluster": map[string]any{"name": "cnpg-1-2"}, "method": "plugin", "pluginConfiguration": map[string]any{"name": barmanPlugin}}, "status": map[string]any{"phase": phase}}
			done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile())
			if phase == "running" || phase == "foreign" {
				if done || err == nil || f.stops != 0 || len(f.deletes) != 0 || f.archive.deletes != 0 {
					t.Fatal("unknown/active Backup was adopted", err)
				}
			} else if done || err != nil || len(f.deletes) != 1 || !strings.HasSuffix(f.deletes[0], "/backups/target") || f.objects[deleteRootPath] == nil {
				t.Fatal("terminal target Backup did not retire before root", err)
			}
		})
	}
}

func TestDeleteBindingAndExecutorDriftBeforeWrites(t *testing.T) {
	for _, mode := range []string{"store-generation", "destination", "credential-rv", "source-prefix", "unknown-sidecar", "ephemeral", "container-id-missing", "retired-before-capture", "active-job", "archive-binding", "prefix", "credential-selector", "archiver-generation"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			root := f.objects[deleteRootPath]
			switch mode {
			case "store-generation":
				f.objects["/apis/barmancloud.cnpg.io/v1/namespaces/app-database/objectstores/backup-store"]["metadata"].(map[string]any)["generation"] = 2
			case "destination":
				f.objects["/apis/barmancloud.cnpg.io/v1/namespaces/app-database/objectstores/backup-store"]["spec"].(map[string]any)["configuration"].(map[string]any)["destinationPath"] = "s3://foreign"
			case "credential-rv":
				f.objects["/api/v1/namespaces/app-database/secrets/s3-creds"]["metadata"].(map[string]any)["resourceVersion"] = "8"
			case "source-prefix":
				root["spec"].(map[string]any)["bootstrap"] = map[string]any{"recovery": map[string]any{"database": "app", "owner": "app", "source": "selected-backup", "secret": map[string]any{"name": "cnpg-1-2-app"}, "recoveryTarget": map[string]any{"backupID": "20261015T120000", "targetImmediate": true}}}
				root["spec"].(map[string]any)["externalClusters"] = []any{map[string]any{"name": "selected-backup", "plugin": map[string]any{"name": barmanPlugin, "enabled": true, "isWALArchiver": false, "parameters": map[string]any{"barmanObjectName": "backup-store", "serverName": "cnpg-1-2"}}}}
			case "archiver-generation":
				root["metadata"].(map[string]any)["generation"] = 4
			case "archive-binding", "prefix", "credential-selector":
				var binding archiveBinding
				_ = json.Unmarshal([]byte(root["metadata"].(map[string]any)["annotations"].(map[string]any)[archiveBindingAnnotation].(string)), &binding)
				switch mode {
				case "credential-selector":
					binding.Credentials[0].Key = "ACCESS_SECRET_KEY"
				case "prefix":
					binding.Prefix = "staging/source/"
				default:
					binding.CreateOperationID = "e1111111-1111-4111-8111-111111111111"
				}
				root["metadata"].(map[string]any)["annotations"].(map[string]any)[archiveBindingAnnotation] = encodeBinding(&binding)
			case "retired-before-capture":
				root["metadata"].(map[string]any)["annotations"].(map[string]any)["cnpg.io/hibernation"] = "on"
			case "active-job":
				f.objects["/apis/batch/v1/namespaces/app-database/jobs/target"] = map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"name": "target", "uid": "job-uid", "labels": map[string]any{"cnpg.io/cluster": "cnpg-1-2"}}, "status": map[string]any{"active": 1}}
			default:
				pod := f.objects["/api/v1/namespaces/app-database/pods/cnpg-1-2-1"]
				spec := pod["spec"].(map[string]any)
				if mode == "unknown-sidecar" {
					spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "unknown", "image": "foreign"})
				}
				if mode == "ephemeral" {
					spec["ephemeralContainers"] = []any{map[string]any{"name": "debug", "image": "foreign"}}
				}
				if mode == "container-id-missing" {
					pod["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["containerID"] = ""
				}
			}
			if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err == nil {
				t.Fatal("unsafe binding/executor accepted")
			}
			if len(f.deletes) != 0 || f.stops != 0 || f.archive.deletes != 0 || f.objects[deleteRecordPath] != nil {
				t.Fatal("mutated before safe inventory")
			}
		})
	}
}

func TestDeleteProgressCheckpointLossBlocksStop(t *testing.T) {
	c, f := newDeleteFixture(t)
	f.denyReceipt = true
	if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err == nil {
		t.Fatal("denied progress checkpoint accepted")
	}
	if f.stops != 0 || len(f.deletes) != 0 || f.archive.deletes != 0 {
		t.Fatal("stop/mutation before durable progress")
	}
	f.denyReceipt = false
	f.loseProgressACK = true
	if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err != nil || f.stops != 1 {
		t.Fatal("lost progress ACK did not reconcile exact marker", err)
	}
}

func TestDeleteRejectsProgressAndPostStopDrift(t *testing.T) {
	for _, mode := range []string{"binding-hash", "inventory-uid", "stage", "missing-observation", "root-bootstrap", "restart-hibernation", "late-backup"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			f.archive.failure = "list"
			if _, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); err == nil {
				t.Fatal("fixture did not stop before archive cleanup")
			}
			switch mode {
			case "root-bootstrap":
				f.objects[deleteRootPath]["spec"].(map[string]any)["bootstrap"] = map[string]any{"initdb": map[string]any{"secret": map[string]any{"name": "foreign"}}}
			case "restart-hibernation":
				delete(f.objects[deleteRootPath]["metadata"].(map[string]any)["annotations"].(map[string]any), "cnpg.io/hibernation")
			case "late-backup":
				f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/backups/late"] = map[string]any{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Backup", "metadata": map[string]any{"name": "late", "uid": "late"}, "spec": map[string]any{"cluster": map[string]any{"name": "cnpg-1-2"}}}
			default:
				annotations := f.objects[deleteRecordPath]["metadata"].(map[string]any)["annotations"].(map[string]any)
				var progress deleteProgress
				_ = json.Unmarshal([]byte(annotations[deleteProgressAnnotation].(string)), &progress)
				switch mode {
				case "binding-hash":
					progress.BindingHash = strings.Repeat("e", 64)
				case "inventory-uid":
					progress.InventoryUID = "foreign"
				case "stage":
					progress.Stage = "resources"
					progress.Pods = nil
				case "missing-observation":
					for uid := range progress.Pods {
						delete(progress.Pods, uid)
						break
					}
				}
				raw, _ := json.Marshal(progress)
				annotations[deleteProgressAnnotation] = string(raw)
			}
			f.archive.failure = ""
			if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err == nil {
				t.Fatal("post-stop drift advanced", mode)
			}
			if len(f.deletes) != 0 || f.archive.deletes != 0 {
				t.Fatal("destructive mutation after drift")
			}
		})
	}
}

func TestDeleteShutdownParserBoundsAndExactRecords(t *testing.T) {
	for _, mode := range []string{"success", "substring", "old", "overflow", "truncated", "final-error"} {
		t.Run(mode, func(t *testing.T) {
			since := time.Now().UTC().Add(-time.Second)
			timestamp := time.Now().UTC().Format(time.RFC3339Nano)
			if mode == "old" {
				timestamp = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
			}
			line := func(msg string) string {
				raw, _ := json.Marshal(map[string]string{"ts": timestamp, "msg": msg})
				return string(raw) + "\n"
			}
			logs := line("Received termination signal") + line("PostgreSQL instance shut down")
			switch mode {
			case "substring":
				logs = line("Received termination signal") + line("SQL says PostgreSQL instance shut down")
			case "overflow":
				logs += strings.Repeat("x", retentionLogLimit+1)
			case "truncated":
				logs += "{bad"
			case "final-error":
				logs += line("error shutting down instance, proceeding")
			}
			events := make(chan deleteLogEvent, 10)
			scanDeleteShutdown(context.Background(), strings.NewReader(logs), "pod", since, events)
			close(events)
			complete, failed := false, false
			for event := range events {
				complete = complete || event.Kind == "complete"
				failed = failed || event.Kind == "failed" || event.Kind == "lost"
			}
			if complete != (mode == "success") || failed != (mode != "success") {
				t.Fatalf("parser evidence wrong: complete=%v failed=%v", complete, failed)
			}
		})
	}
}
