package main

import (
	"context"
	"reflect"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
)

func (w *Worker) cnpgCreate(ctx context.Context, it *api.DeployerCommandCNPGCreate) {
	if it == nil || it.Valid() != nil || w.Client == nil || w.CNPG.AllocationNamespace == "" || !w.CNPG.Enabled() {
		return
	}
	// Re-poll the authenticated Backend before writes; this is current intent, not an OS fence.
	current, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil || current == nil {
		return
	}
	found := false
	for _, command := range *current {
		if command != nil && command.CNPGCreate != nil && command.ValidCNPG() == nil && reflect.DeepEqual(command.CNPGCreate, it) {
			found = true
			break
		}
	}
	if !found {
		return
	}
	host, port, ready, err := cnpg.ApplyCreate(ctx, w.Client, it, w.CNPG)
	if err == nil && !ready {
		return
	}
	result := &api.DeployerSetResultCNPGCreate{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity}
	if err != nil {
		result.FailureCode = api.CNPGFailureReconcile
	} else {
		result.Success, result.QueryReady, result.TLSReady = true, true, true
		result.Host, result.Port = host, port
	}
	w.results = append(w.results, &api.DeployerSetResultItem{CNPGCreate: result})
}
