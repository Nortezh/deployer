package k8s

import (
	v1 "k8s.io/api/core/v1"
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
