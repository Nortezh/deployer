package main

import (
	"context"
	"reflect"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
)

func (w *Worker) cnpgBackup(ctx context.Context, it *api.DeployerCommandCNPGBackup) {
	if it == nil || it.Valid() != nil || w.Client == nil || w.CNPG.AllocationNamespace == "" || !w.CNPG.Enabled() {
		return
	}
	current, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil || current == nil {
		return
	}
	for _, command := range *current {
		if command == nil || command.CNPGBackup == nil || command.ValidCNPG() != nil || !reflect.DeepEqual(command.CNPGBackup, it) {
			continue
		}
		result, err := cnpg.ApplyBackup(ctx, w.Client, it, w.CNPG)
		// Transport/profile/identity errors are unknown outcomes, not native backup failures.
		if err == nil && result != nil {
			w.results = append(w.results, &api.DeployerSetResultItem{CNPGBackup: result})
		}
		return
	}
}
