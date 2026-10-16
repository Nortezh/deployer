package main

import (
	"context"
	"reflect"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
)

func (w *Worker) cnpgRestore(ctx context.Context, it *api.DeployerCommandCNPGRestore) {
	if it == nil || it.Valid() != nil || w.Client == nil || w.CNPG.AllocationNamespace == "" || !w.CNPG.Enabled() {
		return
	}
	current, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil || current == nil {
		return
	}
	found := false
	for _, command := range *current {
		if command != nil && command.CNPGRestore != nil && command.ValidCNPG() == nil && reflect.DeepEqual(command.CNPGRestore, it) {
			found = true
			break
		}
	}
	if !found {
		return
	}
	host, port, ready, err := cnpg.ApplyRestore(ctx, w.Client, it, w.CNPG)
	if err != nil {
		return
	} // no failure/cleanup claim for an uncertain physical recovery
	result := &api.DeployerSetResultCNPGRestore{DeployerCommandCNPGRestore: *it, Status: api.CNPGBackupRunning}
	if ready {
		result.Status = api.CNPGBackupCompleted
		result.Host, result.Port = host, port
		result.QueryReady, result.TLSReady = true, true
	}
	w.results = append(w.results, &api.DeployerSetResultItem{CNPGRestore: result})
}
