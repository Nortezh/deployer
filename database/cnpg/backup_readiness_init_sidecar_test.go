package cnpg

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

// CNPG1.27.4/plugin0.15.1 on native k3s1.31.11 injects the archiver as
// a restartable init container, not a regular application container.
func TestBackupReadinessRestartableInitSidecar(t *testing.T) {
	f, c, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
	always := v1.ContainerRestartPolicyAlways
	sidecar := f.pod.Spec.Containers[1]
	sidecar.RestartPolicy = &always
	f.pod.Spec.InitContainers = []v1.Container{sidecar}
	f.pod.Spec.Containers = f.pod.Spec.Containers[:1]
	f.pod.Status.InitContainerStatuses = []v1.ContainerStatus{f.pod.Status.ContainerStatuses[1]}
	f.pod.Status.ContainerStatuses = f.pod.Status.ContainerStatuses[:1]
	r, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
	if err != nil || r == nil || !r.Available || r.Valid() != nil || f.queries.Load() != 1 {
		t.Fatal("healthy restartable native sidecar rejected", err)
	}
	f.pod.Spec.InitContainers[0].RestartPolicy = nil
	if readinessPodHealthy(f.pod, p.Image) {
		t.Fatal("one-shot init container accepted as running archiver")
	}
	f.pod.Spec.InitContainers[0].RestartPolicy = &always
	f.pod.Status.InitContainerStatuses[0].Ready = false
	if readinessPodHealthy(f.pod, p.Image) {
		t.Fatal("unready native sidecar accepted")
	}
	f.pod.Status.InitContainerStatuses[0].Ready = true
	f.pod.Status.InitContainerStatuses[0].State.Running = nil
	if readinessPodHealthy(f.pod, p.Image) {
		t.Fatal("non-running native sidecar accepted")
	}
	f.pod.Status.InitContainerStatuses[0].State.Running = &v1.ContainerStateRunning{}
	f.pod.Spec.InitContainers[0].Image = "unverified:latest"
	if readinessPodHealthy(f.pod, p.Image) {
		t.Fatal("unpinned native sidecar image accepted")
	}
	f.pod.Spec.InitContainers[0].Image = readinessSidecarImage
	f.pod.Spec.Containers = append(f.pod.Spec.Containers, sidecar)
	if readinessPodHealthy(f.pod, p.Image) {
		t.Fatal("conflicting regular and init archivers accepted")
	}
}
