package cnpg

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Both bootstrap modes must use this database's immutable provisioning Secret.
// Recovery keeps the selected read-only source separate from the target archiver.
func backupBootstrap(cluster *unstructured.Unstructured, name, store string) string {
	bootstrap, _, err := unstructured.NestedMap(cluster.Object, "spec", "bootstrap")
	if err != nil || len(bootstrap) != 1 {
		return ""
	}
	if _, ok := bootstrap["initdb"]; ok {
		if str(cluster, "spec", "bootstrap", "initdb", "secret", "name") == name+"-app" {
			return "initdb"
		}
		return ""
	}
	recovery, ok := bootstrap["recovery"].(map[string]any)
	if !ok || !onlyKeys(recovery, "database", "owner", "secret", "source", "recoveryTarget") || recovery["source"] != "selected-backup" || str(cluster, "spec", "bootstrap", "recovery", "secret", "name") != name+"-app" {
		return ""
	}
	target, ok := recovery["recoveryTarget"].(map[string]any)
	if !ok || len(target) != 2 || !onlyKeys(target, "backupID", "targetImmediate") || target["targetImmediate"] != true {
		return ""
	}
	id, ok := target["backupID"].(string)
	if _, err := time.Parse("20060102T150405", id); !ok || len(id) != 15 || err != nil {
		return ""
	}
	external, _, err := unstructured.NestedSlice(cluster.Object, "spec", "externalClusters")
	if err != nil || len(external) != 1 {
		return ""
	}
	source, ok := external[0].(map[string]any)
	if !ok || len(source) != 2 || !onlyKeys(source, "name", "plugin") || source["name"] != "selected-backup" {
		return ""
	}
	plugin, ok := source["plugin"].(map[string]any)
	if !ok || !onlyKeys(plugin, "name", "enabled", "isWALArchiver", "parameters") || plugin["name"] != barmanPlugin || plugin["enabled"] != true || plugin["isWALArchiver"] != false {
		return ""
	}
	// shortcut: recovery reads share the supported ObjectStore, verify disjoint physical scopes before adding cross-store backup readiness.
	params, ok := plugin["parameters"].(map[string]any)
	if !ok || len(params) != 2 || !onlyKeys(params, "barmanObjectName", "serverName") || store == "" || params["barmanObjectName"] != store {
		return ""
	}
	server, ok := params["serverName"].(string)
	if !ok || server == name || len(validation.IsDNS1123Subdomain(server)) != 0 {
		return ""
	}
	return "recovery"
}
