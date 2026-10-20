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

func TestCNPGBackupReadinessRechecksIntentAndReportsUnavailable(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected Kubernetes call", 403)
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	it := &api.DeployerCommandCNPGBackupReadiness{DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: "a1111111-1111-4111-8111-111111111111"}, CheckID: "b1111111-1111-4111-8111-111111111111", ProfileRevision: 1}
	for _, mode := range []string{"absent", "changed-check", "changed-profile", "mixed", "current"} {
		t.Run(mode, func(t *testing.T) {
			d := &rejectCNPGDeployer{}
			changed := *it
			switch mode {
			case "changed-check":
				changed.CheckID = "f1111111-1111-4111-8111-111111111111"
				d.commands = api.GetCommandsResult{{CNPGBackupReadiness: &changed}}
			case "changed-profile":
				changed.ProfileRevision++
				d.commands = api.GetCommandsResult{{CNPGBackupReadiness: &changed}}
			case "mixed":
				d.commands = api.GetCommandsResult{{CNPGBackupReadiness: it, SecretDelete: &api.DeployerCommandSecretDelete{SecretID: 3, ProjectID: 2, Revision: 1}}}
			case "current":
				d.commands = api.GetCommandsResult{{CNPGBackupReadiness: it}}
			}
			w := Worker{Deployer: d, Client: client, CNPG: cnpg.Profile{AllocationNamespace: "allocator", PortRange: "6100-6199", PVCRetention: "delete", StorageClass: "hostpath", Image: "registry/postgres@sha256:" + strings.Repeat("a", 64)}}
			w.cnpgBackupReadiness(context.Background(), it)
			if calls.Load() != 0 {
				t.Fatal("unconfigured/stale/mixed check reached Kubernetes")
			}
			if mode != "current" {
				if len(w.results) != 0 {
					t.Fatal("stale/mixed observation emitted")
				}
				return
			}
			if len(w.results) != 1 || w.results[0].ValidCNPG() != nil {
				t.Fatal("current unavailable profile was not reported")
			}
			r := w.results[0].CNPGBackupReadiness
			if r == nil || r.Available || r.Evidence != nil || r.DeployerCommandCNPGBackupReadiness != *it || r.FailureCode != api.CNPGFailureBackupReadiness {
				t.Fatal("readiness was confused with backup failure")
			}
		})
	}
}
