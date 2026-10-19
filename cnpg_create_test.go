package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
	"github.com/deploys-app/deployer/k8s"
)

func TestCNPGCreateRechecksCurrentIntentBeforeWrites(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unexpected", 403) }))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	it := &api.DeployerCommandCNPGCreate{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: "a1111111-1111-4111-8111-111111111111"}, Name: "orders", Username: "app", StorageSize: 1024, SecretID: 3, SecretRevision: 1}
	p := cnpg.Profile{AllocationNamespace: "allocator", PortRange: "6200-6201", PVCRetention: "delete", StorageClass: "test", Image: "registry/postgres@sha256:" + strings.Repeat("a", 64)}
	for _, commands := range []api.GetCommandsResult{
		{}, {{CNPGDelete: &api.DeployerCommandCNPGDelete{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity}}}, {{CNPGCreate: it, DatabaseDelete: &api.DeployerCommandDatabaseMetadata{}}},
	} {
		d := &rejectCNPGDeployer{commands: commands}
		w := Worker{Deployer: d, Client: client, CNPG: p}
		w.cnpgCreate(context.Background(), it)
		if calls.Load() != 0 || len(w.results) != 0 {
			t.Fatal("obsolete or mixed fresh intent reached Kubernetes")
		}
	}
	// Even matching current intent must not dispatch the local-spike helper path.
	spike := cnpg.Profile{HostSuffix: ".localhost", Port: "6109", EntryPoint: "tcp-6109", NodeName: "local", StorageClass: "local-path", Image: "pinned"}
	if !spike.Enabled() {
		t.Fatal("spike fixture must be enabled")
	}
	spikeWorker := Worker{Deployer: &rejectCNPGDeployer{commands: api.GetCommandsResult{{CNPGCreate: it}}}, Client: client, CNPG: spike}
	spikeWorker.cnpgCreate(context.Background(), it)
	if calls.Load() != 0 || len(spikeWorker.results) != 0 {
		t.Fatal("local spike reached Kubernetes or acknowledged create")
	}
	// Matching current intent may enter reconciliation; remote errors stay sanitized.
	d := &rejectCNPGDeployer{commands: api.GetCommandsResult{{CNPGCreate: it}}}
	w := Worker{Deployer: d, Client: client, CNPG: p}
	w.cnpgCreate(context.Background(), it)
	if calls.Load() == 0 || len(w.results) != 1 || w.results[0].CNPGCreate.FailureCode != api.CNPGFailureReconcile || w.results[0].CNPGCreate.Success {
		t.Fatal("dispatch/failure boundary not connected")
	}
}
