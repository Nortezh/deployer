package k8s

import (
	"fmt"

	"github.com/Nortezh/api"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"
)

// Env type
type Env map[string]string

func (env Env) envVars() []v1.EnvVar {
	var rs []v1.EnvVar
	for k, v := range env {
		rs = append(rs, v1.EnvVar{
			Name:  k,
			Value: v,
		})
	}
	return rs
}

func (env Env) envVarsWithSecrets(secretName string, refs []api.DeployerCommandDeploymentSecretEnv) ([]v1.EnvVar, error) {
	rs := env.envVars()
	seen := make(map[string]bool, len(env)+len(refs))
	for name := range env {
		seen[name] = true
	}
	for _, ref := range refs {
		if seen[ref.EnvName] {
			return nil, fmt.Errorf("duplicate environment variable %q", ref.EnvName)
		}
		seen[ref.EnvName] = true
		rs = append(rs, v1.EnvVar{
			Name: ref.EnvName,
			ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{Name: secretName},
				Key:                  fmt.Sprintf("secret-%d", ref.SecretID),
				Optional:             pointer.Bool(false),
			}},
		})
	}
	return rs, nil
}
