package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
	"github.com/deploys-app/deployer/k8s"
)

type rejectCNPGDeployer struct {
	api.Deployer
	commands    api.GetCommandsResult
	resultCalls int
}

func (d *rejectCNPGDeployer) GetCommands(context.Context, *api.Empty) (*api.GetCommandsResult, error) {
	return &d.commands, nil
}

func (d *rejectCNPGDeployer) SetResults(context.Context, *api.DeployerSetResult) (*api.Empty, error) {
	d.resultCalls++
	return &api.Empty{}, nil
}

func TestRunRejectsDedicatedCNPGWithoutAnyKubernetesCall(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected Kubernetes call", http.StatusForbidden)
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("test")
	if err != nil {
		t.Fatal(err)
	}

	identity := api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: "a1111111-1111-4111-8111-111111111111"}
	valid := &api.DeployerCommandCNPGCreate{DeployerCommandCNPGIdentity: identity, Name: "db", Username: "app", StorageSize: 1024, SecretID: 3, SecretRevision: 1}
	for _, base := range []*api.DeployerCommandItem{
		{CNPGCreate: valid}, {CNPGDelete: &identity}, {CNPGCreate: &api.DeployerCommandCNPGCreate{}}, {CNPGDelete: &api.DeployerCommandCNPGIdentity{}},
	} {
		for i := -1; i < reflect.TypeOf(*base).NumField(); i++ {
			command := *base
			if i >= 0 {
				field := reflect.ValueOf(&command).Elem().Field(i)
				field.Set(reflect.New(field.Type().Elem()))
			}
			// Exercise decoding at the actual shared API envelope boundary.
			data, err := json.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			var decoded api.DeployerCommandItem
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			d := &rejectCNPGDeployer{commands: api.GetCommandsResult{nil, &decoded}}
			worker := Worker{Deployer: d, Client: client, location: &api.LocationItem{}, CNPG: cnpg.Profile{HostSuffix: ".localhost", Port: "6109", EntryPoint: "tcp-6109", NodeName: "local", StorageClass: "local-path", Image: "pinned"}}
			worker.Run()
			if calls.Load() != 0 || d.resultCalls != 0 || len(worker.results) != 0 {
				t.Fatalf("dedicated/mixed command executed: %s, calls=%d results=%d", data, calls.Load(), d.resultCalls)
			}
		}
	}
}

func TestLegacyCNPGRejectsWithoutAnyKubernetesCall(t *testing.T) {
	for _, mode := range []string{"run", "direct"} {
		for _, command := range []struct {
			name string
			item api.DeployerCommandItem
		}{
			{"valid create", api.DeployerCommandItem{DatabaseCreate: &api.DeployerCommandDatabaseCreate{
				ID: 1, ProjectID: 2, Name: "db", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG,
				StorageSize: 1024, PostgresConfig: &api.DatabaseConfigPostgres{User: "app", Password: "fixture-only"},
			}}},
			{"missing credentials", api.DeployerCommandItem{DatabaseCreate: &api.DeployerCommandDatabaseCreate{
				ID: 1, ProjectID: 2, Name: "db", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG,
			}}},
			{"delete", api.DeployerCommandItem{DatabaseDelete: &api.DeployerCommandDatabaseMetadata{
				ID: 1, ProjectID: 2, Name: "db", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG,
			}}},
		} {
			t.Run(mode+"/"+command.name, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					http.Error(w, "unexpected Kubernetes call", http.StatusForbidden)
				}))
				defer server.Close()
				t.Setenv("KUBE_PROXY_URL", server.URL)
				client, err := k8s.NewLocalClient("test")
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(command.item)
				if err != nil {
					t.Fatal(err)
				}
				var decoded api.DeployerCommandItem
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				d := &rejectCNPGDeployer{commands: api.GetCommandsResult{nil, &decoded}}
				worker := Worker{Deployer: d, Client: client, location: &api.LocationItem{}, CNPG: cnpg.Profile{
					HostSuffix: ".localhost", Port: "6109", EntryPoint: "tcp-6109", NodeName: "local", StorageClass: "local-path", Image: "pinned",
				}}
				if !worker.CNPG.Enabled() {
					t.Fatal("fixture must exercise an enabled profile")
				}
				if mode == "run" {
					worker.Run()
				} else if decoded.DatabaseCreate != nil {
					worker.databaseCreate(context.Background(), decoded.DatabaseCreate)
				} else {
					worker.databaseDelete(context.Background(), decoded.DatabaseDelete)
				}
				if calls.Load() != 0 || d.resultCalls != 0 || len(worker.results) != 0 {
					t.Fatalf("legacy CNPG executed: calls=%d resultCalls=%d results=%d", calls.Load(), d.resultCalls, len(worker.results))
				}
			})
		}
	}
}

func TestRunContinuesLegacyOnlyBatchAfterCNPGRejection(t *testing.T) {
	// A legacy-only Secret command must still reach its normal handler.
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "fixture denial", http.StatusForbidden)
	}))
	defer server.Close()
	t.Setenv("KUBE_PROXY_URL", server.URL)
	client, err := k8s.NewLocalClient("test")
	if err != nil {
		t.Fatal(err)
	}
	d := &rejectCNPGDeployer{commands: api.GetCommandsResult{
		{DatabaseCreate: &api.DeployerCommandDatabaseCreate{ID: 1, ProjectID: 2, Name: "db", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG, StorageSize: 1024, PostgresConfig: &api.DatabaseConfigPostgres{User: "app", Password: "fixture-only"}}},
		{DatabaseDelete: &api.DeployerCommandDatabaseMetadata{ID: 1, ProjectID: 2, Name: "db", Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG}},
		{CNPGDelete: &api.DeployerCommandCNPGIdentity{}},
		{SecretDelete: &api.DeployerCommandSecretDelete{SecretID: 3, ProjectID: 2, Name: "TOKEN", Revision: 1}},
	}}
	worker := Worker{Deployer: d, Client: client, location: &api.LocationItem{}, CNPG: cnpg.Profile{
		HostSuffix: ".localhost", Port: "6109", EntryPoint: "tcp-6109", NodeName: "local", StorageClass: "local-path", Image: "pinned",
	}}
	worker.Run()
	if calls.Load() == 0 || d.resultCalls != 1 {
		t.Fatalf("legacy sibling not processed: calls=%d results=%d", calls.Load(), d.resultCalls)
	}
}
