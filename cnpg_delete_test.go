package main

import (
	"context"
	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
	"github.com/deploys-app/deployer/k8s"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCNPGDeleteRechecksCurrentPhaseBeforeWrites(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "private raw error", 403) }))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	it := &api.DeployerCommandCNPGDelete{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: api.CNPGDeleteVersion, ID: 1, ProjectID: 2, OperationID: "b1111111-1111-4111-8111-111111111111"}, Phase: api.CNPGDeleteResources, SecretID: 3, SecretRevision: 1, CreateOperationID: "a1111111-1111-4111-8111-111111111111"}
	profile := cnpg.Profile{AllocationNamespace: "allocator", PortRange: "6109-6110", PVCRetention: "delete", StorageClass: "local-path", Image: "postgres:17.5@sha256:" + strings.Repeat("a", 64)}
	final := *it
	final.Phase = api.CNPGDeleteFinalize
	final.Receipt = &api.CNPGDeleteReceipt{InventoryUID: "d1111111-1111-4111-8111-111111111111", ClusterUID: "c1111111-1111-4111-8111-111111111111", ArchiveBindingHash: strings.Repeat("a", 64), WriterStopped: true, ArchiveAbsent: true}
	old := *it
	old.OperationID = "c1111111-1111-4111-8111-111111111111"
	foreign := *it
	foreign.ProjectID++
	for _, commands := range []api.GetCommandsResult{{}, {{CNPGDelete: &final}}, {{CNPGDelete: &old}}, {{CNPGDelete: &foreign}}, {{CNPGDelete: it, DatabaseDelete: &api.DeployerCommandDatabaseMetadata{}}}} {
		w := Worker{Deployer: &rejectCNPGDeployer{commands: commands}, Client: client, CNPG: profile}
		w.cnpgDelete(context.Background(), it)
		if calls.Load() != 0 || len(w.results) != 0 {
			t.Fatal("stale/foreign/mixed/other phase reached Kubernetes")
		}
	}
	w := Worker{Deployer: &rejectCNPGDeployer{commands: api.GetCommandsResult{{CNPGDelete: it}}}, Client: client, CNPG: profile}
	w.cnpgDelete(context.Background(), it)
	if calls.Load() == 0 || len(w.results) != 1 || w.results[0].CNPGDelete.ResourceAbsent || w.results[0].CNPGDelete.FailureCode != api.CNPGFailureCleanup || w.results[0].CNPGDelete.Phase != it.Phase || w.results[0].CNPGDelete.Valid() != nil || w.results[0].CNPGDelete.FailureStage != api.CNPGDeleteScope || w.results[0].CNPGDelete.CreateOperationID != it.CreateOperationID || w.results[0].CNPGDelete.SecretID != it.SecretID || w.results[0].CNPGDelete.SecretRevision != it.SecretRevision || w.results[0].CNPGDelete.Receipt != nil {
		t.Fatal("unsafe result/error disclosure")
	}
	w = Worker{Deployer: &rejectCNPGDeployer{commands: api.GetCommandsResult{{CNPGDelete: &final}}}, Client: client, CNPG: profile}
	w.cnpgDelete(context.Background(), &final)
	if len(w.results) != 1 || w.results[0].CNPGDelete.Valid() != nil || w.results[0].CNPGDelete.FailureStage != api.CNPGDeleteFinalize || w.results[0].CNPGDelete.ResourceAbsent || *w.results[0].CNPGDelete.Receipt != *final.Receipt {
		t.Fatal("finalize failure lost the accepted receipt or leaked a false success")
	}
}
