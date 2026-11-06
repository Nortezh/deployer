package cnpg

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"sync"
	"time"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

type deleteContainer struct {
	Name, ID string
	Restarts int32
	Started  time.Time
	Init     bool
}
type deleteExecutor struct {
	Name           string
	UID            types.UID
	Node, SpecHash string
	Containers     []deleteContainer
}

func deletePodSpecHash(pod *v1.Pod) string {
	raw, _ := json.Marshal(pod.Spec)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func captureDeleteExecutor(pod *v1.Pod, inv *deleteInventory) (*deleteExecutor, error) {
	if pod.DeletionTimestamp != nil || !ownerUID(pod, inv.Objects[0].UID) || pod.Spec.NodeName == "" || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "postgres" || pod.Spec.Containers[0].Image != inv.Binding.Image {
		return nil, errDeleteBlocked
	}
	e := &deleteExecutor{Name: pod.Name, UID: pod.UID, Node: pod.Spec.NodeName, SpecHash: deletePodSpecHash(pod)}
	containers := []v1.Container{pod.Spec.Containers[0]}
	init := false
	for _, container := range pod.Spec.InitContainers {
		if container.RestartPolicy != nil && *container.RestartPolicy == v1.ContainerRestartPolicyAlways {
			if container.Name != "plugin-barman-cloud" || container.Image != readinessSidecarImage || init {
				return nil, errDeleteBlocked
			}
			containers = append(containers, container)
			init = true
		}
	}
	if !init {
		return nil, errDeleteBlocked
	}
	for i, container := range containers {
		statuses := pod.Status.ContainerStatuses
		if i == 1 {
			statuses = pod.Status.InitContainerStatuses
		}
		var found *v1.ContainerStatus
		for j := range statuses {
			if statuses[j].Name == container.Name {
				found = &statuses[j]
			}
		}
		if found == nil || found.ContainerID == "" || found.State.Running == nil || found.State.Running.StartedAt.IsZero() || found.State.Running.StartedAt.After(time.Now()) {
			return nil, errDeleteBlocked
		}
		e.Containers = append(e.Containers, deleteContainer{Name: container.Name, ID: found.ContainerID, Restarts: found.RestartCount, Started: found.State.Running.StartedAt.Time, Init: i == 1})
	}
	if !executorMatches(pod, e, false) {
		return nil, errDeleteBlocked
	}
	return e, nil
}

func executorMatches(pod *v1.Pod, e *deleteExecutor, terminal bool, since ...time.Time) bool {
	if pod.Name != e.Name || pod.UID != e.UID || pod.Spec.NodeName != e.Node || deletePodSpecHash(pod) != e.SpecHash || len(e.Containers) != 2 || e.Containers[0].Name != "postgres" || e.Containers[1].Name != "plugin-barman-cloud" {
		return false
	}
	if terminal && !terminalDeletePod(pod) {
		return false
	}
	for _, container := range e.Containers {
		statuses := pod.Status.ContainerStatuses
		if container.Init {
			statuses = pod.Status.InitContainerStatuses
		}
		found := false
		for _, status := range statuses {
			if status.Name != container.Name {
				continue
			}
			found = true
			if status.ContainerID != container.ID || status.RestartCount != container.Restarts {
				return false
			}
			if status.State.Terminated != nil {
				t := status.State.Terminated
				if t.ContainerID != container.ID || !t.StartedAt.Time.Equal(container.Started) || t.FinishedAt.IsZero() || t.ExitCode != 0 || t.Signal != 0 || t.Reason != "Completed" || t.FinishedAt.Before(&t.StartedAt) || terminal && len(since) == 1 && t.FinishedAt.Time.Before(since[0]) {
					return false
				}
			} else if terminal || status.State.Running == nil || !status.State.Running.StartedAt.Time.Equal(container.Started) {
				return false
			}
		}
		if !found {
			return false
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == "plugin-barman-cloud" {
			continue
		}
		if status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 || status.State.Terminated.Signal != 0 || status.State.Terminated.Reason != "Completed" {
			return false
		}
	}
	return true
}

type deleteLogEvent struct {
	UID  string
	Kind string
}

// Parse instance-manager JSON records only; SQL/log substrings cannot certify shutdown.
func scanDeleteShutdown(ctx context.Context, reader io.Reader, uid string, since time.Time, events chan<- deleteLogEvent) {
	send := func(kind string) {
		select {
		case events <- deleteLogEvent{uid, kind}:
		case <-ctx.Done():
		}
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, retentionLogLimit+1))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	entry, shutdown, total := false, false, 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line) + 1
		if total > retentionLogLimit {
			send("lost")
			return
		}
		var record struct {
			Message   string `json:"msg"`
			Timestamp string `json:"ts"`
		}
		if json.Unmarshal(line, &record) != nil {
			send("lost")
			return
		}
		timestamp, err := time.Parse(time.RFC3339Nano, record.Timestamp)
		if err != nil || timestamp.Before(since) || timestamp.After(time.Now().Add(time.Second)) {
			continue
		}
		switch record.Message {
		case "Received termination signal", "Context has been cancelled, shutting down and exiting":
			entry = true
			send("entry")
		case "PostgreSQL instance shut down":
			if entry {
				shutdown = true
				send("shutdown")
			}
		case "Error while shutting down the PostgreSQL instance", "error while shutting down instance, proceeding", "error shutting down instance, proceeding":
			send("failed")
			return
		}
	}
	if scanner.Err() != nil || !entry || !shutdown {
		send("lost")
		return
	}
	send("complete")
}

func stoppedObservations(inv *deleteInventory, progress *deleteProgress) bool {
	for _, e := range inv.Executors {
		o := progress.Pods[string(e.UID)]
		if !o.Entry || !o.Shutdown || !o.LogComplete || !o.Terminal || o.Failed {
			return false
		}
	}
	return true
}

func stopDeleteWriters(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGDelete, p Profile, cm *v1.ConfigMap, inv *deleteInventory, progress *deleteProgress) (bool, error) {
	if stoppedObservations(inv, progress) {
		root, err := deleteRoot(ctx, c, inv)
		if err != nil || root == nil || root.GetDeletionTimestamp() != nil || root.GetAnnotations()["cnpg.io/hibernation"] != "on" || validateDeleteArchive(ctx, c, p, inv) != nil || checkDeleteDependents(ctx, c, deleteName(it), inv, true) != nil {
			return false, errDeleteBlocked
		}
		if verifyDeleteStopped(ctx, c, inv) != nil {
			return false, nil
		}
		progress.Stage = "stopped"
		return true, saveDeleteProgress(ctx, c, cm, inv, progress)
	}
	for _, observation := range progress.Pods {
		if observation.Failed {
			return false, errDeleteBlocked
		}
	}
	if validateDeleteArchive(ctx, c, p, inv) != nil || checkDeleteDependents(ctx, c, deleteName(it), inv, progress.Stage == "stopping") != nil {
		return false, errDeleteBlocked
	}
	pods := c.Core().CoreV1().Pods(c.DBNamespace())
	list, err := pods.List(ctx, metav1.ListOptions{})
	if err != nil || list.ResourceVersion == "" {
		return false, errDeleteBlocked
	}
	for _, e := range inv.Executors {
		o := progress.Pods[string(e.UID)]
		if o.LogComplete && o.Terminal {
			continue
		}
		found := false
		for i := range list.Items {
			if list.Items[i].UID == e.UID {
				found = true
				if !executorMatches(&list.Items[i], &e, false) {
					return false, errDeleteBlocked
				}
				if terminalDeletePod(&list.Items[i]) {
					if progress.Stage == "" || !executorMatches(&list.Items[i], &e, true, progress.Started) {
						return false, errDeleteBlocked
					}
					o.Terminal = true
					progress.Pods[string(e.UID)] = o
				}
			}
		}
		if !found {
			return false, errDeleteBlocked
		} // NotFound cannot reconstruct missing observations.
	}
	observeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher, err := pods.Watch(observeCtx, metav1.ListOptions{ResourceVersion: list.ResourceVersion})
	if err != nil {
		return false, errDeleteBlocked
	}
	defer watcher.Stop()
	if progress.Stage == "" {
		progress.Stage, progress.Started = "stopping", time.Now().UTC().Add(-time.Second)
		for _, e := range inv.Executors {
			progress.Pods[string(e.UID)] = deleteObservation{}
		}
	}
	events := make(chan deleteLogEvent, 32)
	var streams []io.ReadCloser
	var readers sync.WaitGroup
	defer func() {
		cancel()
		for _, stream := range streams {
			_ = stream.Close()
		}
		readers.Wait()
	}()
	for _, e := range inv.Executors {
		if progress.Pods[string(e.UID)].LogComplete {
			continue
		}
		since := metav1.NewTime(progress.Started)
		stream, err := pods.GetLogs(e.Name, &v1.PodLogOptions{Container: "postgres", Follow: true, SinceTime: &since}).Stream(observeCtx)
		if err != nil {
			return false, errDeleteBlocked
		}
		streams = append(streams, stream)
		readers.Add(1)
		go func(uid string, reader io.Reader) {
			defer readers.Done()
			scanDeleteShutdown(observeCtx, reader, uid, progress.Started, events)
		}(string(e.UID), stream)
	}
	// Recheck the current named executions after opening name-addressed log endpoints.
	for _, e := range inv.Executors {
		if progress.Pods[string(e.UID)].LogComplete && progress.Pods[string(e.UID)].Terminal {
			continue
		}
		pod, err := pods.Get(ctx, e.Name, metav1.GetOptions{})
		if err != nil || !executorMatches(pod, &e, false) {
			return false, errDeleteBlocked
		}
	}
	if err := saveDeleteProgress(ctx, c, cm, inv, progress); err != nil {
		return false, err
	}
	root, err := deleteRoot(ctx, c, inv)
	if err != nil || root == nil || root.GetDeletionTimestamp() != nil {
		return false, errDeleteBlocked
	}
	if root.GetAnnotations()["cnpg.io/hibernation"] != "on" {
		annotations := root.GetAnnotations()
		annotations["cnpg.io/hibernation"] = "on"
		patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(root.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": root.GetResourceVersion()}, {"op": "add", "path": "/metadata/annotations", "value": annotations}})
		_, err = c.Dynamic().Resource(clusterGVR).Namespace(c.DBNamespace()).Patch(ctx, root.GetName(), types.JSONPatchType, patch, metav1.PatchOptions{})
		if err != nil {
			seen, getErr := deleteRoot(ctx, c, inv)
			if getErr != nil || seen == nil || seen.GetAnnotations()["cnpg.io/hibernation"] != "on" {
				return false, errDeleteBlocked
			}
		}
	}
	for !stoppedObservations(inv, progress) {
		select {
		case <-ctx.Done():
			return false, errDeleteBlocked
		case event := <-events:
			o := progress.Pods[event.UID]
			switch event.Kind {
			case "entry":
				o.Entry = true
			case "shutdown":
				o.Entry, o.Shutdown = true, true
			case "complete":
				o.Entry, o.Shutdown, o.LogComplete = true, true, true
			case "failed":
				o.Failed = true
			case "lost":
				return false, errDeleteBlocked
			}
			progress.Pods[event.UID] = o
			if err := saveDeleteProgress(ctx, c, cm, inv, progress); err != nil {
				return false, err
			}
			if o.Failed {
				return false, errDeleteBlocked
			}
		case event, ok := <-watcher.ResultChan():
			if !ok || event.Type == watch.Error {
				return false, errDeleteBlocked
			}
			if event.Type == watch.Bookmark {
				continue
			}
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				return false, errDeleteBlocked
			}
			relevant := pod.Labels["cnpg.io/cluster"] == inv.Binding.Server || ownerUID(pod, inv.Objects[0].UID)
			for _, object := range inv.Objects {
				if object.Kind == "Job" && ownerUID(pod, object.UID) {
					relevant = true
				}
			}
			for _, volume := range pod.Spec.Volumes {
				for _, object := range inv.Objects {
					if object.Kind == "PVC" && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == object.Name {
						relevant = true
					}
				}
			}
			if !relevant {
				continue
			}
			var executor *deleteExecutor
			for i := range inv.Executors {
				if pod.UID == inv.Executors[i].UID {
					executor = &inv.Executors[i]
				}
			}
			if executor == nil || !executorMatches(pod, executor, false) {
				return false, errDeleteBlocked
			}
			if terminalDeletePod(pod) {
				if !executorMatches(pod, executor, true, progress.Started) {
					return false, errDeleteBlocked
				}
				o := progress.Pods[string(pod.UID)]
				o.Terminal = true
				progress.Pods[string(pod.UID)] = o
				if err := saveDeleteProgress(ctx, c, cm, inv, progress); err != nil {
					return false, err
				}
			} else if event.Type == watch.Deleted {
				return false, errDeleteBlocked
			}
		}
	}
	if checkDeleteProfile(ctx, c, p, inv) != nil || checkDeleteDependents(ctx, c, deleteName(it), inv, true) != nil {
		return false, errDeleteBlocked
	}
	root, err = deleteRoot(ctx, c, inv)
	if err != nil || root == nil || root.GetDeletionTimestamp() != nil || root.GetAnnotations()["cnpg.io/hibernation"] != "on" {
		return false, errDeleteBlocked
	}
	if verifyDeleteStopped(ctx, c, inv) != nil {
		return false, nil
	}
	progress.Stage = "stopped"
	return true, saveDeleteProgress(ctx, c, cm, inv, progress)
}

// Immutable inventory must include exactly the executions for which stop evidence is accepted.
func validDeleteExecutors(inv *deleteInventory) bool {
	seen := map[types.UID]bool{}
	for _, e := range inv.Executors {
		if seen[e.UID] || e.UID == "" || e.Name == "" || e.Node == "" || len(e.SpecHash) != 64 || len(e.Containers) != 2 {
			return false
		}
		known := false
		for _, o := range inv.Objects {
			known = known || o.Kind == "Pod" && o.UID == e.UID && o.Name == e.Name
		}
		if !known || reflect.DeepEqual(inv.Nodes[e.Node], deleteNode{}) {
			return false
		}
		seen[e.UID] = true
	}
	return true
}
