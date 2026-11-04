package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
	"github.com/deploys-app/deployer/k8s"
)

func retentionCommand() *api.DeployerCommandCNPGBackupRetention {
	c := &api.DeployerCommandCNPGBackupRetention{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: "f1111111-1111-4111-8111-111111111111"}, CreateOperationID: "a1111111-1111-4111-8111-111111111111", SourceClusterUID: "c1111111-1111-4111-8111-111111111111"}
	for i := range 8 {
		c.Backups = append(c.Backups, api.CNPGBackupRetentionItem{OperationID: fmt.Sprintf("b1111111-1111-4111-8111-%012d", i+1), BackupUID: fmt.Sprintf("d1111111-1111-4111-8111-%012d", i+1), Material: api.CNPGBackupMaterial{BarmanBackupID: fmt.Sprintf("202611%02dT020000", i+1), MajorVersion: 17, Image: "registry/postgres:17@sha256:" + strings.Repeat("a", 64), StorageClass: "hostpath", ObjectStoreName: "backup-store", ObjectStoreUID: "e1111111-1111-4111-8111-111111111111", ServerName: "cnpg-1-2", EndWAL: "000000010000000000000004", EndLSN: "0/4000028", Archive: &api.CNPGBackupArchive{EndpointURL: "http://minio.test:9000", DestinationPath: "s3://cnpg-backups/staging", ProfileRevision: 1, ConfigurationHash: strings.Repeat("a", 64)}}})
	}
	c.ReplacementOperationID = c.Backups[7].OperationID
	return c
}

func TestCNPGBackupRetentionRechecksCurrentIntent(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "fixture denial", http.StatusForbidden)
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	it := retentionCommand()
	if err := it.Valid(); err != nil {
		t.Fatal(err)
	}
	changed := *it
	changed.OperationID = "f2222222-2222-4222-8222-222222222222"
	for _, mode := range []string{"absent", "changed", "mixed", "current"} {
		d := &rejectCNPGDeployer{}
		switch mode {
		case "changed":
			d.commands = api.GetCommandsResult{{CNPGBackupRetention: &changed}}
		case "mixed":
			d.commands = api.GetCommandsResult{{CNPGBackupRetention: it, SecretDelete: &api.DeployerCommandSecretDelete{SecretID: 3, ProjectID: 2, Revision: 1}}}
		case "current":
			d.commands = api.GetCommandsResult{{CNPGBackupRetention: it}}
		}
		w := Worker{Deployer: d, Client: client, CNPG: cnpg.Profile{AllocationNamespace: "allocator", PortRange: "6100-6199", PVCRetention: "delete", StorageClass: "hostpath", Image: it.Backups[0].Material.Image, BackupObjectStore: "backup-store", BackupProfileRevision: 1}}
		before := calls.Load()
		w.cnpgBackupRetention(context.Background(), it)
		if (calls.Load() > before) != (mode == "current") {
			t.Fatal("stale/mixed retention reached Kubernetes", mode)
		}
		if len(w.results) != 0 {
			t.Fatal("unknown transport changed retention phase")
		}
	}
}
