package main

import (
	"context"
	"errors"
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
	result := &api.DeployerSetResultCNPGRestore{DeployerCommandCNPGRestore: *it, Status: api.CNPGBackupRunning}
	if err != nil {
		var failed *cnpg.RestoreFailure
		if !errors.As(err, &failed) {
			return // uncertain recovery remains active
		}
		result.Status, result.FailureCode = api.CNPGBackupFailed, api.CNPGFailureRestore
		result.TargetClusterUID, result.RecoveryJobUID = failed.TargetClusterUID, failed.RecoveryJobUID
		if result.Valid() == nil {
			w.results = append(w.results, &api.DeployerSetResultItem{CNPGRestore: result})
		}
		return
	}
	if ready {
		result.Status = api.CNPGBackupCompleted
		result.Host, result.Port = host, port
		result.QueryReady, result.TLSReady = true, true
	}
	w.results = append(w.results, &api.DeployerSetResultItem{CNPGRestore: result})
}
