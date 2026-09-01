package k8s

import (
	"testing"

	"github.com/Nortezh/api"
	v1 "k8s.io/api/core/v1"
)

func TestEnvVarsWithSecrets(t *testing.T) {
	secretName := ResourceID(2, "secrets")
	got, err := (Env{"PLAIN": "value"}).envVarsWithSecrets(secretName, []api.DeployerCommandDeploymentSecretEnv{
		{EnvName: "TOKEN", SecretID: 42},
	})
	if err != nil {
		t.Fatal(err)
	}

	vars := make(map[string]*v1.EnvVar, len(got))
	for i := range got {
		vars[got[i].Name] = &got[i]
	}
	if plain := vars["PLAIN"]; plain == nil || plain.Value != "value" || plain.ValueFrom != nil {
		t.Fatalf("literal env changed: %#v", plain)
	}
	secret := vars["TOKEN"]
	if secret == nil || secret.Value != "" || secret.ValueFrom == nil || secret.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("secret env not rendered as SecretKeyRef: %#v", secret)
	}
	ref := secret.ValueFrom.SecretKeyRef
	if ref.Name != "secrets-2" || ref.Key != "secret-42" || ref.Optional == nil || *ref.Optional {
		t.Fatalf("unexpected required SecretKeyRef: %#v", ref)
	}
}

func TestEnvVarsWithSecretsRejectsDuplicateNames(t *testing.T) {
	tests := []struct {
		name string
		env  Env
		refs []api.DeployerCommandDeploymentSecretEnv
	}{
		{
			name: "literal and secret",
			env:  Env{"TOKEN": "literal"},
			refs: []api.DeployerCommandDeploymentSecretEnv{{EnvName: "TOKEN", SecretID: 1}},
		},
		{
			name: "two secrets",
			refs: []api.DeployerCommandDeploymentSecretEnv{
				{EnvName: "TOKEN", SecretID: 1},
				{EnvName: "TOKEN", SecretID: 2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.env.envVarsWithSecrets("secrets-2", tt.refs); err == nil {
				t.Fatal("expected duplicate environment variable error")
			}
		})
	}
}
