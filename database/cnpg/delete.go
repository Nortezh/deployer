package cnpg

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"slices"
	"strconv"
	"strings"
	"time"
)

var errDeleteBlocked = errors.New("CNPG managed cleanup pending or blocked")

const longhornDriver = "driver.longhorn.io"
const csiDeleteFinalizer = "external-provisioner.volume.kubernetes.io/finalizer"

type deleteObject struct {
	Kind, Name string
	UID        types.UID
}
type deleteNode struct {
	UID    types.UID
	BootID string
}
type deleteInventory struct {
	Command                                           api.DeployerCommandCNPGDelete
	Objects                                           []deleteObject
	Nodes                                             map[string]deleteNode
	StorageClassUID, ProvisionerUID, AllocationMapUID types.UID
	AllocationKey                                     string
	StorageDriver, StorageClassName                   string
	CSIHandles                                        map[string]string
}

func deleteName(it *api.DeployerCommandCNPGDelete) string {
	return k8s.ResourceID(it.ProjectID, "cnpg-"+strconv.FormatInt(it.ID, 10))
}
func retirementName(name string) string { return name + "-retirement" }
func ownerUID(o metav1.Object, uid types.UID) bool {
	for _, ref := range o.GetOwnerReferences() {
		if ref.UID == uid && uid != "" {
			return true
		}
	}
	return false
}
func deleteOptions(o metav1.Object) metav1.DeleteOptions {
	uid, rv := o.GetUID(), o.GetResourceVersion()
	policy := metav1.DeletePropagationForeground
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &policy}
}

// ApplyDelete retires managed resources, not disconnected-node processes or forensic copies.
// The immutable inventory survives retries and remains a non-secret retirement marker.
func ApplyDelete(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGDelete, p Profile) (bool, error) {
	if c == nil || it == nil || it.Valid() != nil || p.AllocationNamespace == "" || p.PVCRetention != "delete" || p.StorageClass == "" {
		return false, errDeleteBlocked
	}
	name := deleteName(it)
	maps := c.Core().CoreV1().ConfigMaps(c.DBNamespace())
	cm, err := maps.Get(ctx, retirementName(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) && it.Phase == api.CNPGDeleteResources {
		inv, captureErr := captureDelete(ctx, c, it, p)
		if captureErr != nil {
			return false, errDeleteBlocked
		}
		data, marshalErr := json.Marshal(inv)
		if marshalErr != nil {
			return false, errDeleteBlocked
		}
		immutable := true
		cm, err = maps.Create(ctx, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retirementName(name), Namespace: c.DBNamespace(), Labels: map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10), "nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.OperationID}}, Immutable: &immutable, Data: map[string]string{"inventory": string(data)}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			cm, err = maps.Get(ctx, retirementName(name), metav1.GetOptions{})
		}
	}
	if err != nil || cm == nil || cm.UID == "" || cm.DeletionTimestamp != nil || cm.Immutable == nil || !*cm.Immutable || len(cm.OwnerReferences) != 0 || len(cm.Data) != 1 {
		return false, errDeleteBlocked
	}
	var inv deleteInventory
	if json.Unmarshal([]byte(cm.Data["inventory"]), &inv) != nil || !inventoryMatches(&inv, it) || cm.Labels["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) || cm.Labels["nortezh.io/project-id"] != strconv.FormatInt(it.ProjectID, 10) || cm.Labels["nortezh.io/operation-id"] != it.OperationID {
		return false, errDeleteBlocked
	}
	if err := checkDeleteProfile(ctx, c, p, &inv); err != nil {
		return false, errDeleteBlocked
	}
	// Recheck all standard dependents each poll, including terminating Pods and Jobs.
	if err := checkDeleteDependents(ctx, c, name, &inv); err != nil {
		return false, errDeleteBlocked
	}
	for _, obj := range inv.Objects {
		absent, err := retireObject(ctx, c, obj, it, it.Phase == api.CNPGDeleteResources)
		if err != nil {
			return false, errDeleteBlocked
		}
		if !absent {
			return false, nil
		}
	}
	if err := checkDeleteDependents(ctx, c, name, &inv); err != nil {
		return false, errDeleteBlocked
	}
	if it.Phase == api.CNPGDeleteResources {
		return true, nil
	}
	// Backend emits finalize only after every Secret target's current deletion ACK.
	// Independently check the local key/marker, then release only the captured port.
	aggregate, err := c.Core().CoreV1().Secrets(c.Namespace()).Get(ctx, k8s.ResourceID(it.ProjectID, "secrets"), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, errDeleteBlocked
	}
	if err == nil {
		if aggregate.DeletionTimestamp != nil || len(aggregate.OwnerReferences) != 0 || aggregate.Type != v1.SecretTypeOpaque || aggregate.Labels["id"] != aggregate.Name || aggregate.Labels["projectId"] != strconv.FormatInt(it.ProjectID, 10) {
			return false, errDeleteBlocked
		}
		if _, ok := aggregate.Data["secret-"+strconv.FormatInt(it.SecretID, 10)]; ok {
			return false, errDeleteBlocked
		}
		if _, ok := aggregate.Annotations["nortezh.io/secret-revision-"+strconv.FormatInt(it.SecretID, 10)]; ok {
			return false, errDeleteBlocked
		}
	}
	if err := releaseDeleteAllocation(ctx, c, p, name, it, &inv); err != nil {
		return false, errDeleteBlocked
	}
	return true, nil
}

func inventoryMatches(inv *deleteInventory, it *api.DeployerCommandCNPGDelete) bool {
	expected := *it
	expected.Phase = api.CNPGDeleteResources
	if inv.Command != expected || len(inv.Nodes) == 0 || inv.AllocationKey == "" || inv.StorageClassUID == "" || inv.AllocationMapUID == "" {
		return false
	}
	if inv.StorageDriver == "" {
		if inv.ProvisionerUID == "" || inv.StorageClassName != "" || len(inv.CSIHandles) != 0 {
			return false
		}
	} else if inv.StorageDriver != longhornDriver || inv.ProvisionerUID != "" || inv.StorageClassName == "" || len(inv.CSIHandles) != 2 {
		return false
	}
	if len(inv.Objects) < 3 || inv.Objects[0].Kind != "Cluster" || inv.Objects[0].Name != deleteName(it) {
		return false
	}
	allowed := map[string]bool{"Cluster": true, "Route": true, "Bootstrap": true, "PVC": true, "PV": true, "Pod": true, "Job": true, "Service": true, "Secret": true}
	kinds := map[string]bool{}
	seen := map[string]bool{}
	uids := map[types.UID]bool{}
	for _, o := range inv.Objects {
		key := o.Kind + "/" + o.Name
		if o.UID == "" || o.Name == "" || seen[key] || uids[o.UID] || !allowed[o.Kind] || (inv.StorageDriver != "" && o.Kind == "PV" && inv.CSIHandles[o.Name] == "") {
			return false
		}
		seen[key] = true
		uids[o.UID] = true
		kinds[o.Kind] = true
	}
	handles := map[string]bool{}
	for name, handle := range inv.CSIHandles {
		if !seen["PV/"+name] || handle == "" || handles[handle] {
			return false
		}
		handles[handle] = true
	}
	return seen["Route/"+deleteName(it)] && seen["Bootstrap/"+deleteName(it)+"-app"] && kinds["PVC"] && kinds["PV"]
}

func captureDelete(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGDelete, p Profile) (*deleteInventory, error) {
	name := deleteName(it)
	ns := c.DBNamespace()
	inv := &deleteInventory{Command: *it, Nodes: map[string]deleteNode{}}
	storage, err := c.Core().StorageV1().StorageClasses().Get(ctx, p.StorageClass, metav1.GetOptions{})
	if err != nil || storage.UID == "" || storage.DeletionTimestamp != nil {
		return nil, errDeleteBlocked
	}
	inv.StorageClassUID = storage.UID
	if storage.Provisioner == longhornDriver {
		inv.StorageDriver, inv.StorageClassName, inv.CSIHandles = longhornDriver, p.StorageClass, map[string]string{}
	} else if p.StorageClass != "local-path" || storage.Provisioner != "rancher.io/local-path" {
		return nil, errDeleteBlocked
	}
	// shortcut: partial creates without a root stay blocked; require a no-late-write fence before adding absent-root cleanup.
	cluster, err := c.Dynamic().Resource(clusterGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || cluster.GetUID() == "" || cluster.GetDeletionTimestamp() != nil || cluster.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) {
		return nil, errDeleteBlocked
	}
	sc, _, _ := unstructured.NestedString(cluster.Object, "spec", "storage", "storageClass")
	bootstrapConfig, _, bootstrapErr := unstructured.NestedMap(cluster.Object, "spec", "bootstrap")
	bootstrap := str(cluster, "spec", "bootstrap", "initdb", "secret", "name")
	if _, recovery := bootstrapConfig["recovery"]; recovery {
		// Recovery reads externalClusters; cleanup still inventories only target-owned resources.
		bootstrap = str(cluster, "spec", "bootstrap", "recovery", "secret", "name")
	}
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	if bootstrapErr != nil || len(bootstrapConfig) != 1 || sc != p.StorageClass || bootstrap != name+"-app" || instances != 2 {
		return nil, errDeleteBlocked
	}
	backup, backupSet, backupErr := unstructured.NestedMap(cluster.Object, "spec", "backup")
	plugins, pluginsSet, pluginsErr := unstructured.NestedSlice(cluster.Object, "spec", "plugins")
	if backupErr != nil || pluginsErr != nil || (backupSet && len(backup) > 0) || (pluginsSet && len(plugins) > 0) {
		return nil, errDeleteBlocked
	}
	inv.Objects = append(inv.Objects, deleteObject{"Cluster", name, cluster.GetUID()})
	route, err := c.Dynamic().Resource(routeGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || route.GetUID() == "" || route.GetDeletionTimestamp() != nil || route.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) {
		return nil, errDeleteBlocked
	}
	inv.Objects = append(inv.Objects, deleteObject{"Route", name, route.GetUID()})
	app, err := c.Core().CoreV1().Secrets(ns).Get(ctx, name+"-app", metav1.GetOptions{})
	if err != nil || app.UID == "" || app.DeletionTimestamp != nil || len(app.OwnerReferences) != 0 || app.Immutable == nil || !*app.Immutable || app.Type != v1.SecretTypeBasicAuth {
		return nil, errDeleteBlocked
	}
	for k, v := range map[string]string{"nortezh.io/database-id": strconv.FormatInt(it.ID, 10), "nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.CreateOperationID, "nortezh.io/secret-id": strconv.FormatInt(it.SecretID, 10), "nortezh.io/secret-revision": strconv.FormatInt(it.SecretRevision, 10)} {
		if app.Labels[k] != v {
			return nil, errDeleteBlocked
		}
	}
	inv.Objects = append(inv.Objects, deleteObject{"Bootstrap", app.Name, app.UID})
	pvcs, err := c.Core().CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + name})
	if err != nil || len(pvcs.Items) != 2 {
		return nil, errDeleteBlocked
	}
	for _, pvc := range pvcs.Items {
		if pvc.UID == "" || !ownerUID(&pvc, cluster.GetUID()) || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != p.StorageClass || pvc.Status.Phase != v1.ClaimBound || pvc.Spec.VolumeName == "" || pvc.DeletionTimestamp != nil {
			return nil, errDeleteBlocked
		}
		pv, err := c.Core().CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil || !deletePVMatches(pv, &pvc, p.StorageClass, inv.StorageDriver) {
			return nil, errDeleteBlocked
		}
		inv.Objects = append(inv.Objects, deleteObject{"PVC", pvc.Name, pvc.UID}, deleteObject{"PV", pv.Name, pv.UID})
		if inv.StorageDriver == longhornDriver {
			inv.CSIHandles[pv.Name] = pv.Spec.CSI.VolumeHandle
			continue
		}
		affinity := pv.Spec.NodeAffinity
		if affinity == nil || affinity.Required == nil || len(affinity.Required.NodeSelectorTerms) != 1 {
			return nil, errDeleteBlocked
		}
		term := affinity.Required.NodeSelectorTerms[0]
		if len(term.MatchExpressions) != 1 || len(term.MatchFields) != 0 {
			return nil, errDeleteBlocked
		}
		expr := term.MatchExpressions[0]
		if expr.Key != "kubernetes.io/hostname" || expr.Operator != v1.NodeSelectorOpIn || len(expr.Values) != 1 {
			return nil, errDeleteBlocked
		}
		inv.Nodes[expr.Values[0]] = deleteNode{}
	}
	pods, err := c.Core().CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + name})
	if err != nil {
		return nil, errDeleteBlocked
	}
	jobs, err := c.Core().BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=" + name})
	if err != nil {
		return nil, errDeleteBlocked
	}
	jobUIDs := map[types.UID]bool{}
	for _, job := range jobs.Items {
		if job.UID == "" || !ownerUID(&job, cluster.GetUID()) {
			return nil, errDeleteBlocked
		}
		jobUIDs[job.UID] = true
		inv.Objects = append(inv.Objects, deleteObject{"Job", job.Name, job.UID})
	}
	for _, pod := range pods.Items {
		owned := ownerUID(&pod, cluster.GetUID())
		for uid := range jobUIDs {
			owned = owned || ownerUID(&pod, uid)
		}
		if !owned || pod.UID == "" || pod.Spec.NodeName == "" {
			return nil, errDeleteBlocked
		}
		inv.Objects = append(inv.Objects, deleteObject{"Pod", pod.Name, pod.UID})
		inv.Nodes[pod.Spec.NodeName] = deleteNode{}
	}
	services, err := c.Core().CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errDeleteBlocked
	}
	for _, o := range services.Items {
		belongs := ownerUID(&o, cluster.GetUID())
		if !belongs && strings.HasPrefix(o.Name, name+"-") {
			return nil, errDeleteBlocked
		}
		if belongs {
			if o.UID == "" {
				return nil, errDeleteBlocked
			}
			inv.Objects = append(inv.Objects, deleteObject{"Service", o.Name, o.UID})
		}
	}
	secrets, err := c.Core().CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errDeleteBlocked
	}
	for _, o := range secrets.Items {
		if o.Name == app.Name {
			continue
		}
		belongs := ownerUID(&o, cluster.GetUID())
		if !belongs && strings.HasPrefix(o.Name, name+"-") {
			return nil, errDeleteBlocked
		}
		if belongs {
			if o.UID == "" {
				return nil, errDeleteBlocked
			}
			inv.Objects = append(inv.Objects, deleteObject{"Secret", o.Name, o.UID})
		}
	}
	cm, err := c.Core().CoreV1().ConfigMaps(p.AllocationNamespace).Get(ctx, allocationMap, metav1.GetOptions{})
	if err != nil {
		return nil, errDeleteBlocked
	}
	inv.AllocationMapUID = cm.UID
	owner := allocationOwner(ns, name, it.ID)
	for key, value := range cm.Data {
		if value == owner {
			if inv.AllocationKey != "" {
				return nil, errDeleteBlocked
			}
			inv.AllocationKey = key
		}
	}
	if inv.AllocationKey == "" {
		return nil, errDeleteBlocked
	}
	// Allocator keys are node_port; route must point to that same owned reservation.
	node, port, ok := splitDeleteAllocation(inv.AllocationKey)
	if !ok || route.GetLabels()["kdb.io/lb-node"] != node {
		return nil, errDeleteBlocked
	}
	entries, _, _ := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	if len(entries) != 1 || entries[0] != "tcp-"+port {
		return nil, errDeleteBlocked
	}
	inv.Nodes[node] = deleteNode{}
	for node := range inv.Nodes {
		n, err := c.Core().CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil || !deleteNodeReady(n) {
			return nil, errDeleteBlocked
		}
		inv.Nodes[node] = deleteNode{n.UID, n.Status.NodeInfo.BootID}
	}
	if inv.StorageDriver == "" {
		provisioner, err := c.Core().AppsV1().Deployments("kube-system").Get(ctx, "local-path-provisioner", metav1.GetOptions{})
		if err != nil {
			return nil, errDeleteBlocked
		}
		inv.ProvisionerUID = provisioner.UID
	}
	if !inventoryMatches(inv, it) || checkDeleteProfile(ctx, c, p, inv) != nil || checkDeleteDependents(ctx, c, name, inv) != nil {
		return nil, errDeleteBlocked
	}
	return inv, nil
}

func deletePVMatches(pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, storage, driver string) bool {
	if pv == nil {
		return false
	}
	handle := ""
	if pv.Spec.CSI != nil {
		handle = pv.Spec.CSI.VolumeHandle
	}
	return pv.UID != "" && pv.DeletionTimestamp == nil && pv.Spec.PersistentVolumeReclaimPolicy == v1.PersistentVolumeReclaimDelete && pv.Spec.StorageClassName == storage && deletePVSourceMatches(pv, driver, handle) && pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.UID == pvc.UID && pv.Spec.ClaimRef.Name == pvc.Name && pv.Spec.ClaimRef.Namespace == pvc.Namespace
}

func deletePVSourceMatches(pv *v1.PersistentVolume, driver, handle string) bool {
	if driver == longhornDriver {
		return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == driver && handle != "" && pv.Spec.CSI.VolumeHandle == handle && slices.Contains(pv.Finalizers, csiDeleteFinalizer)
	}
	return driver == "" && pv.Spec.StorageClassName == "local-path" && (pv.Spec.HostPath != nil || (pv.Spec.Local != nil && pv.Spec.Local.Path != ""))
}

func deleteNodeReady(n *v1.Node) bool {
	if n == nil || n.UID == "" || n.DeletionTimestamp != nil || n.Status.NodeInfo.BootID == "" || !strings.HasPrefix(n.Status.NodeInfo.KubeletVersion, "v1.") {
		return false
	}
	for _, condition := range n.Status.Conditions {
		if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}

func checkDeleteProfile(ctx context.Context, c *k8s.Client, p Profile, inv *deleteInventory) error {
	for name, expected := range inv.Nodes {
		n, err := c.Core().CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil || !deleteNodeReady(n) || n.UID != expected.UID || n.Status.NodeInfo.BootID != expected.BootID {
			return errDeleteBlocked
		}
		lease, err := c.Core().CoordinationV1().Leases("kube-node-lease").Get(ctx, name, metav1.GetOptions{})
		if err != nil || !ownerUID(lease, n.UID) || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != name || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 || *lease.Spec.LeaseDurationSeconds > 60 {
			return errDeleteBlocked
		}
		age := time.Since(lease.Spec.RenewTime.Time)
		if age < -10*time.Second || age > time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second {
			return errDeleteBlocked
		}
	}
	sc, err := c.Core().StorageV1().StorageClasses().Get(ctx, p.StorageClass, metav1.GetOptions{})
	if err != nil || sc.UID != inv.StorageClassUID || sc.DeletionTimestamp != nil || sc.ReclaimPolicy == nil || *sc.ReclaimPolicy != v1.PersistentVolumeReclaimDelete || sc.VolumeBindingMode == nil {
		return errDeleteBlocked
	}
	if inv.StorageDriver == longhornDriver {
		if p.StorageClass != inv.StorageClassName || sc.Provisioner != longhornDriver || *sc.VolumeBindingMode != "Immediate" {
			return errDeleteBlocked
		}
	} else {
		if sc.Provisioner != "rancher.io/local-path" || *sc.VolumeBindingMode != "WaitForFirstConsumer" {
			return errDeleteBlocked
		}
		d, err := c.Core().AppsV1().Deployments("kube-system").Get(ctx, "local-path-provisioner", metav1.GetOptions{})
		if err != nil || d.UID != inv.ProvisionerUID || d.DeletionTimestamp != nil || d.Status.AvailableReplicas < 1 || len(d.Spec.Template.Spec.Containers) != 1 || !strings.HasPrefix(d.Spec.Template.Spec.Containers[0].Image, "rancher/local-path-provisioner:") {
			return errDeleteBlocked
		}
	}
	for _, resource := range []string{"backups", "scheduledbackups"} {
		list, err := c.Dynamic().Resource(clusterGVR.GroupVersion().WithResource(resource)).Namespace(c.DBNamespace()).List(ctx, metav1.ListOptions{})
		if err != nil {
			return errDeleteBlocked
		}
		for _, obj := range list.Items {
			cluster, _, _ := unstructured.NestedString(obj.Object, "spec", "cluster", "name")
			rootUID := inv.Objects[0].UID
			if cluster == deleteName(&inv.Command) || ownerUID(&obj, rootUID) {
				return errDeleteBlocked
			}
		}
	}
	return nil
}

func checkDeleteDependents(ctx context.Context, c *k8s.Client, name string, inv *deleteInventory) error {
	known := map[types.UID]bool{}
	claims := map[string]bool{}
	var root types.UID
	for _, o := range inv.Objects {
		known[o.UID] = true
		if o.Kind == "Cluster" {
			root = o.UID
		}
		if o.Kind == "PVC" {
			claims[o.Name] = true
		}
	}
	pods, err := c.Core().CoreV1().Pods(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, pod := range pods.Items {
		relevant := pod.Labels["cnpg.io/cluster"] == name || ownerUID(&pod, root)
		for _, obj := range inv.Objects {
			if obj.Kind == "Job" && ownerUID(&pod, obj.UID) {
				relevant = true
			}
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && claims[volume.PersistentVolumeClaim.ClaimName] {
				relevant = true
			}
		}
		if relevant && (!known[pod.UID] || pod.DeletionTimestamp != nil) {
			return errDeleteBlocked
		}
	}
	pvcs, err := c.Core().CoreV1().PersistentVolumeClaims(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, pvc := range pvcs.Items {
		if (pvc.Labels["cnpg.io/cluster"] == name || ownerUID(&pvc, root) || claims[pvc.Name] || strings.HasPrefix(pvc.Name, name+"-")) && !known[pvc.UID] {
			return errDeleteBlocked
		}
	}
	services, err := c.Core().CoreV1().Services(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, o := range services.Items {
		if (ownerUID(&o, root) || o.Labels["cnpg.io/cluster"] == name || strings.HasPrefix(o.Name, name+"-")) && !known[o.UID] {
			return errDeleteBlocked
		}
	}
	secrets, err := c.Core().CoreV1().Secrets(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, o := range secrets.Items {
		if (ownerUID(&o, root) || o.Labels["cnpg.io/cluster"] == name || strings.HasPrefix(o.Name, name+"-")) && !known[o.UID] {
			return errDeleteBlocked
		}
	}
	jobs, err := c.Core().BatchV1().Jobs(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, job := range jobs.Items {
		if (job.Labels["cnpg.io/cluster"] == name || ownerUID(&job, root)) && !known[job.UID] {
			return errDeleteBlocked
		}
	}
	for _, resource := range []string{"deployments", "statefulsets", "daemonsets", "replicasets"} {
		controllers, err := c.Dynamic().Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: resource}).Namespace(c.DBNamespace()).List(ctx, metav1.ListOptions{})
		if err != nil {
			return errDeleteBlocked
		}
		for _, o := range controllers.Items {
			if ownerUID(&o, root) || o.GetLabels()["cnpg.io/cluster"] == name || strings.HasPrefix(o.GetName(), name+"-") {
				return errDeleteBlocked
			}
		}
	}
	cron, err := c.Core().BatchV1().CronJobs(c.DBNamespace()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, job := range cron.Items {
		if job.Labels["cnpg.io/cluster"] == name || ownerUID(&job, root) {
			return errDeleteBlocked
		}
	}
	pvs, err := c.Core().CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	for _, pv := range pvs.Items {
		tracked := false
		for i, obj := range inv.Objects {
			if obj.Kind == "PV" && obj.Name == pv.Name {
				if pv.UID != obj.UID || i == 0 || inv.Objects[i-1].Kind != "PVC" || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Name != inv.Objects[i-1].Name || pv.Spec.ClaimRef.UID != inv.Objects[i-1].UID {
					return errDeleteBlocked
				}
				tracked = true
			}
		}
		relevant := pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == c.DBNamespace() && (claims[pv.Spec.ClaimRef.Name] || strings.HasPrefix(pv.Spec.ClaimRef.Name, name+"-"))
		for name, handle := range inv.CSIHandles {
			if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == inv.StorageDriver && pv.Spec.CSI.VolumeHandle == handle && pv.Name != name {
				return errDeleteBlocked
			}
		}
		if tracked || relevant {
			if !known[pv.UID] || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != c.DBNamespace() || !claims[pv.Spec.ClaimRef.Name] || !known[pv.Spec.ClaimRef.UID] || pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete || !deletePVSourceMatches(&pv, inv.StorageDriver, inv.CSIHandles[pv.Name]) || (inv.StorageDriver != "" && pv.Spec.StorageClassName != inv.StorageClassName) {
				return errDeleteBlocked
			}
		}
	}
	return nil
}

// PV removal is the supported provisioner's completion signal, never DELETE/NotFound of PVC alone.
func retireObject(ctx context.Context, c *k8s.Client, obj deleteObject, it *api.DeployerCommandCNPGDelete, remove bool) (bool, error) {
	ns := c.DBNamespace()
	var o metav1.Object
	var err error
	switch obj.Kind {
	case "Cluster":
		o, err = c.Dynamic().Resource(clusterGVR).Namespace(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	case "Route":
		o, err = c.Dynamic().Resource(routeGVR).Namespace(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	case "Bootstrap", "Secret":
		o, err = c.Core().CoreV1().Secrets(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	case "PVC":
		o, err = c.Core().CoreV1().PersistentVolumeClaims(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	case "PV":
		o, err = c.Core().CoreV1().PersistentVolumes().Get(ctx, obj.Name, metav1.GetOptions{})
	case "Pod":
		o, err = c.Core().CoreV1().Pods(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	case "Job":
		o, err = c.Core().BatchV1().Jobs(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	case "Service":
		o, err = c.Core().CoreV1().Services(ns).Get(ctx, obj.Name, metav1.GetOptions{})
	default:
		return false, errDeleteBlocked
	}
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil || o.GetUID() != obj.UID || o.GetResourceVersion() == "" {
		return false, errDeleteBlocked
	}
	if obj.Kind == "Cluster" || obj.Kind == "Route" || obj.Kind == "Bootstrap" {
		if o.GetLabels()["nortezh.io/database-id"] != strconv.FormatInt(it.ID, 10) {
			return false, errDeleteBlocked
		}
		if obj.Kind == "Bootstrap" {
			for k, v := range map[string]string{"nortezh.io/project-id": strconv.FormatInt(it.ProjectID, 10), "nortezh.io/operation-id": it.CreateOperationID, "nortezh.io/secret-id": strconv.FormatInt(it.SecretID, 10), "nortezh.io/secret-revision": strconv.FormatInt(it.SecretRevision, 10)} {
				if o.GetLabels()[k] != v {
					return false, errDeleteBlocked
				}
			}
		}
	}
	// Only roots and the bootstrap/route are explicitly deleted. Stock GC/reclaim owns children/PVs.
	if !remove || o.GetDeletionTimestamp() != nil || (obj.Kind != "Cluster" && obj.Kind != "Route" && obj.Kind != "Bootstrap") {
		return false, nil
	}
	opts := deleteOptions(o)
	switch obj.Kind {
	case "Cluster":
		err = c.Dynamic().Resource(clusterGVR).Namespace(ns).Delete(ctx, obj.Name, opts)
	case "Route":
		err = c.Dynamic().Resource(routeGVR).Namespace(ns).Delete(ctx, obj.Name, opts)
	case "Bootstrap":
		err = c.Core().CoreV1().Secrets(ns).Delete(ctx, obj.Name, opts)
	}
	return false, err
}
