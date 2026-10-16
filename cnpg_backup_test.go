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

func TestCNPGBackupRechecksIntentAndRetainsUnknownOutcome(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "fixture denial", 403) }))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	it := &api.DeployerCommandCNPGBackup{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: "b1111111-1111-4111-8111-111111111111"}, CreateOperationID: "a1111111-1111-4111-8111-111111111111"}
	changed := *it
	changed.OperationID = "f1111111-1111-4111-8111-111111111111"
	for _, mode := range []string{"absent", "changed", "mixed", "current"} {
		d := &rejectCNPGDeployer{}
		switch mode {
		case "changed":
			d.commands = api.GetCommandsResult{{CNPGBackup: &changed}}
		case "mixed":
			d.commands = api.GetCommandsResult{{CNPGBackup: it, SecretDelete: &api.DeployerCommandSecretDelete{SecretID: 3, ProjectID: 2, Revision: 1}}}
		case "current":
			d.commands = api.GetCommandsResult{{CNPGBackup: it}}
		}
		w := Worker{Deployer: d, Client: client, CNPG: cnpg.Profile{AllocationNamespace: "allocator", PortRange: "6100-6199", PVCRetention: "delete", StorageClass: "hostpath", Image: "registry/postgres@sha256:" + strings.Repeat("a", 64)}}
		before := calls.Load()
		w.cnpgBackup(context.Background(), it)
		if (calls.Load() > before) != (mode == "current") {
			t.Fatal("stale/mixed command reached Kubernetes", mode)
		}
		if len(w.results) != 0 {
			t.Fatal("transport denial reported native failure")
		}
	}
}
