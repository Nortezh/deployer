package cnpg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"path"
	"time"

	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

const readinessPluginVersion = "0.15.1"
const readinessSidecarImage = "ghcr.io/cloudnative-pg/plugin-barman-cloud-sidecar:v0.15.1@sha256:2287a793a804db751edd0d07d44cd549e357677e98a800eb12e2cf7c0d53f9b0"

// Ordered accessKeyId, secretAccessKey selectors; never include Secret values.
type readinessCredential struct {
	Name, Key, UID, ResourceVersion string
}

type readinessProfile struct {
	Format, Namespace, Image, StorageClass, Plugin, PluginVersion, SidecarImage string
	ObjectStoreName, ObjectStoreUID                                             string
	Spec                                                                        map[string]any
	Credentials                                                                 []readinessCredential
}

func onlyKeys(m map[string]any, keys ...string) bool {
	for key := range m {
		found := false
		for _, allowed := range keys {
			found = found || key == allowed
		}
		if !found {
			return false
		}
	}
	return true
}

// Require timestamps for changed fields; absence is unknown, not permission to
// reuse WAL success from before a destination/credential change.
func configurationTime(o metav1.Object, initial bool, fields ...string) (time.Time, bool) {
	since := o.GetCreationTimestamp().Time
	if since.IsZero() || since.After(time.Now()) {
		return time.Time{}, false
	}
	found := false
	for _, managed := range o.GetManagedFields() {
		if managed.Subresource != "" || managed.FieldsV1 == nil {
			continue
		}
		var names map[string]json.RawMessage
		if json.Unmarshal(managed.FieldsV1.Raw, &names) != nil {
			return time.Time{}, false
		}
		for _, field := range fields {
			if _, ok := names[field]; !ok {
				continue
			}
			if managed.Time == nil || managed.Time.Time.IsZero() || managed.Time.Time.After(time.Now()) {
				return time.Time{}, false
			}
			found = true
			if managed.Time.Time.After(since) {
				since = managed.Time.Time
			}
		}
	}
	return since, initial || found
}

// The initial supported profile is explicit S3 credentials, gzip/parallel options
// and no custom sidecar environment/arguments or automatic retention.
func readinessStore(ctx context.Context, c *k8s.Client, p Profile, store *unstructured.Unstructured) (string, []*v1.Secret, time.Time, bool) {
	if store.GetName() != p.BackupObjectStore || store.GetNamespace() != c.DBNamespace() || store.GetUID() == "" || store.GetDeletionTimestamp() != nil || store.GetGeneration() < 1 {
		return "", nil, time.Time{}, false
	}
	since, ok := configurationTime(store, store.GetGeneration() == 1, "f:spec")
	if !ok {
		return "", nil, time.Time{}, false
	}
	spec, _, err := unstructured.NestedMap(store.Object, "spec")
	if err != nil || !onlyKeys(spec, "configuration", "retentionPolicy", "instanceSidecarConfiguration") {
		return "", nil, time.Time{}, false
	}
	if retention, exists := spec["retentionPolicy"]; exists && retention != "" {
		return "", nil, time.Time{}, false
	}
	if sidecar, exists := spec["instanceSidecarConfiguration"]; exists {
		m, ok := sidecar.(map[string]any)
		if !ok || !onlyKeys(m, "logLevel", "retentionPolicyIntervalSeconds", "resources", "env", "additionalContainerArgs") {
			return "", nil, time.Time{}, false
		}
		for _, key := range []string{"env", "additionalContainerArgs"} {
			if value, exists := m[key]; exists {
				items, ok := value.([]any)
				if !ok || len(items) != 0 {
					return "", nil, time.Time{}, false
				}
			}
		}
		if value, exists := m["resources"]; exists {
			resources, ok := value.(map[string]any)
			if !ok || len(resources) != 0 {
				return "", nil, time.Time{}, false
			}
		}
		if value, exists := m["logLevel"]; exists && value != "info" {
			return "", nil, time.Time{}, false
		}
		if value, exists := m["retentionPolicyIntervalSeconds"]; exists && value != int64(1800) {
			return "", nil, time.Time{}, false
		}
	}
	config, ok := spec["configuration"].(map[string]any)
	if !ok || !onlyKeys(config, "destinationPath", "endpointURL", "s3Credentials", "wal", "data") {
		return "", nil, time.Time{}, false
	}
	destination, dOK := config["destinationPath"].(string)
	endpoint, eOK := config["endpointURL"].(string)
	d, dErr := url.Parse(destination)
	e, eErr := url.Parse(endpoint)
	if !dOK || !eOK || dErr != nil || eErr != nil || d.Scheme != "s3" || d.Host == "" || d.Port() != "" || d.User != nil || d.RawQuery != "" || d.Fragment != "" || d.RawPath != "" || d.Path != "" && d.Path != path.Clean(d.Path) ||
		(e.Scheme != "http" && e.Scheme != "https") || e.Hostname() == "" || e.User != nil || e.RawQuery != "" || e.Fragment != "" || e.Path != "" && e.Path != "/" {
		return "", nil, time.Time{}, false
	}
	for _, section := range []string{"wal", "data"} {
		if value, exists := config[section]; exists {
			m, ok := value.(map[string]any)
			if !ok || section == "wal" && !onlyKeys(m, "compression", "maxParallel") || section == "data" && !onlyKeys(m, "compression", "jobs", "immediateCheckpoint") {
				return "", nil, time.Time{}, false
			}
			for key, value := range m {
				switch key {
				case "compression":
					if value != "gzip" {
						return "", nil, time.Time{}, false
					}
				case "immediateCheckpoint":
					if _, ok := value.(bool); !ok {
						return "", nil, time.Time{}, false
					}
				default:
					if number, ok := value.(int64); !ok || number <= 0 {
						return "", nil, time.Time{}, false
					}
				}
			}
		}
	}
	selectors, ok := config["s3Credentials"].(map[string]any)
	if !ok || len(selectors) != 2 || !onlyKeys(selectors, "accessKeyId", "secretAccessKey") {
		return "", nil, time.Time{}, false
	}
	fingerprint := readinessProfile{Format: "nortezh.cnpg.backup-profile.v1", Namespace: c.DBNamespace(), Image: p.Image, StorageClass: p.StorageClass, Plugin: barmanPlugin, PluginVersion: readinessPluginVersion, SidecarImage: readinessSidecarImage, ObjectStoreName: store.GetName(), ObjectStoreUID: string(store.GetUID()), Spec: spec}
	var secrets []*v1.Secret
	for _, role := range []string{"accessKeyId", "secretAccessKey"} {
		selector, ok := selectors[role].(map[string]any)
		if !ok || len(selector) != 2 || !onlyKeys(selector, "name", "key") {
			return "", nil, time.Time{}, false
		}
		name, nOK := selector["name"].(string)
		key, kOK := selector["key"].(string)
		if !nOK || !kOK || len(validation.IsDNS1123Subdomain(name)) != 0 || len(validation.IsConfigMapKey(key)) != 0 {
			return "", nil, time.Time{}, false
		}
		secret, err := c.Core().CoreV1().Secrets(c.DBNamespace()).Get(ctx, name, metav1.GetOptions{})
		if err != nil || secret.UID == "" || secret.ResourceVersion == "" || secret.Namespace != c.DBNamespace() || secret.Name != name || secret.DeletionTimestamp != nil || len(secret.Data[key]) == 0 {
			return "", nil, time.Time{}, false
		}
		changed, ok := configurationTime(secret, false, "f:data", "f:stringData")
		if !ok {
			return "", nil, time.Time{}, false
		}
		if changed.After(since) {
			since = changed
		}
		fingerprint.Credentials = append(fingerprint.Credentials, readinessCredential{Name: name, Key: key, UID: string(secret.UID), ResourceVersion: secret.ResourceVersion})
		secrets = append(secrets, secret)
	}
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return "", nil, time.Time{}, false
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), secrets, since, true
}
