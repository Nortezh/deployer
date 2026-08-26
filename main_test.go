package main

import (
	"encoding/json"
	"testing"

	"github.com/Nortezh/api"
)

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
