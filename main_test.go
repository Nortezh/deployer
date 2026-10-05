package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
)

func TestDatabaseProviderRouting(t *testing.T) {
	for _, tt := range []struct {
		name, command string
		kind          string
	}{
		{"old postgres", `{"databaseCreate":{"id":1,"type":"postgres"}}`, "Postgres"},
		{"explicit kdb", `{"databaseCreate":{"id":1,"type":"postgres","provider":"kdb"}}`, "Postgres"},
		{"redis", `{"databaseCreate":{"id":1,"type":"redis"}}`, "Redis"},
		{"mongo", `{"databaseCreate":{"id":1,"type":"mongo"}}`, "Mongo"},
		{"cnpg", `{"databaseCreate":{"id":1,"type":"postgres","provider":"cnpg"}}`, ""},
		{"unknown", `{"databaseCreate":{"id":1,"type":"postgres","provider":"other"}}`, ""},
		{"non-postgres provider", `{"databaseCreate":{"id":1,"type":"redis","provider":"kdb"}}`, ""},
		{"unknown type", `{"databaseCreate":{"id":1,"type":"other"}}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var command api.DeployerCommandItem
			if err := json.Unmarshal([]byte(tt.command), &command); err != nil {
				t.Fatal(err)
			}
			eng := legacyEngineFor(command.DatabaseCreate.Type, command.DatabaseCreate.Provider)
			if eng == nil && tt.kind != "" || eng != nil && (tt.kind == "" || eng.Kind() != tt.kind) {
				t.Fatalf("route %s: got %v, want %q", tt.command, eng, tt.kind)
			}
			// An unsupported command must not attempt any Kubernetes call or acknowledge success.
			if tt.kind == "" {
				w := &Worker{}
				w.databaseCreate(context.Background(), command.DatabaseCreate)
				if len(w.results) != 0 {
					t.Fatal("unsupported create acknowledged")
				}
			}
		})
	}

	for _, provider := range []api.DatabaseProvider{"", api.DatabaseProviderKDB, api.DatabaseProviderCNPG, "other"} {
		it := &api.DeployerCommandDatabaseMetadata{ID: 1, Type: api.DatabaseTypePostgres, Provider: provider}
		if provider == "" || provider == api.DatabaseProviderKDB {
			if legacyEngineFor(it.Type, it.Provider) == nil {
				t.Fatalf("legacy delete rejected: %q", provider)
			}
			continue
		}
		w := &Worker{}
		w.databaseDelete(context.Background(), it)
		if len(w.results) != 0 {
			t.Fatalf("unsupported delete acknowledged: %q", provider)
		}
	}
}

func TestCNPGSpikeFailsClosedWithoutAClient(t *testing.T) {
	it := &api.DeployerCommandDatabaseCreate{ID: 1, Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG}
	w := &Worker{}
	w.databaseCreate(context.Background(), it)
	w.databaseDelete(context.Background(), &api.DeployerCommandDatabaseMetadata{ID: 1, Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG})
	if len(w.results) != 0 {
		t.Fatal("unconfigured CNPG was acknowledged")
	}
	w.CNPG = cnpg.Profile{HostSuffix: ".localhost", Port: "6109", EntryPoint: "tcp-6109", NodeName: "local", StorageClass: "local-path", Image: "pinned"}
	w.databaseCreate(context.Background(), it)
	if len(w.results) != 1 || w.results[0].DatabaseCreate.Success || w.results[0].DatabaseCreate.FailureCode != "CNPG_CONFIG_REQUIRED" {
		t.Fatal("invalid CNPG create did not return a bounded failure")
	}
}

func TestLegacyDatabaseResultJSON(t *testing.T) {
	result := api.DeployerSetResultItem{DatabaseCreate: &api.DeployerSetResultItemDatabaseCreate{
		ID: 1, Success: true, Host: "db.example", Port: 5432,
	}}
	got, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"databaseCreate":{"id":1,"success":true,"host":"db.example","port":5432}}`
	if string(got) != want {
		t.Fatalf("legacy result: got %s, want %s", got, want)
	}
	var oldBackend struct {
		DatabaseCreate struct {
			ID      int64  `json:"id"`
			Success bool   `json:"success"`
			Host    string `json:"host"`
			Port    int    `json:"port"`
		} `json:"databaseCreate"`
	}
	if err := json.Unmarshal(got, &oldBackend); err != nil || !oldBackend.DatabaseCreate.Success || oldBackend.DatabaseCreate.Host != "db.example" {
		t.Fatalf("old backend decoder: %+v, %v", oldBackend, err)
	}
}

func TestSecretResultIsRedacted(t *testing.T) {
	got, err := json.Marshal(&api.DeployerSetResultItem{
		SecretUpsert: &api.DeployerSetResultItemSecret{SecretID: 1, Revision: 2, Success: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"secretUpsert":{"secretId":1,"revision":2,"success":true}}`; string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
