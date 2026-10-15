package cnpg

import (
	"context"
	"crypto/subtle"
	"errors"
	"strconv"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var errBootstrapUnavailable = errors.New("CNPG bootstrap credential unavailable or mismatched")

// EnsureBootstrapSecret prepares only the credential Secret, not a Cluster or a ready result.
// Dedicated dispatch rechecks current intent and the synced credential revision first.
func EnsureBootstrapSecret(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGCreate) (string, error) {
	if c == nil || it == nil || it.Valid() != nil || c.Namespace() == "" {
		return "", errBootstrapUnavailable
	}
	name := createName(it) + "-app"
	if len(validation.IsDNS1123Subdomain(name)) != 0 || len(validation.IsDNS1123Label(it.Name)) != 0 {
		return "", errBootstrapUnavailable
	}
	// A standard retirement record prevents reuse of this immutable database identity.
	_, retirementErr := c.Core().CoreV1().ConfigMaps(c.DBNamespace()).Get(ctx, retirementName(createName(it)), metav1.GetOptions{})
	if !apierrors.IsNotFound(retirementErr) {
		return "", errBootstrapUnavailable
	}
	password, err := c.ReadSyncedSecretKey(ctx, it.ProjectID, it.SecretID, it.SecretRevision)
	if err != nil {
		return "", errBootstrapUnavailable
	}
	labels := map[string]string{
		"nortezh.io/database-id":     strconv.FormatInt(it.ID, 10),
		"nortezh.io/project-id":      strconv.FormatInt(it.ProjectID, 10),
		"nortezh.io/operation-id":    it.OperationID,
		"nortezh.io/secret-id":       strconv.FormatInt(it.SecretID, 10),
		"nortezh.io/secret-revision": strconv.FormatInt(it.SecretRevision, 10),
	}
	secrets := c.Core().CoreV1().Secrets(c.DBNamespace())
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		immutable := true
		secret, err = secrets.Create(ctx, &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.DBNamespace(), Labels: labels},
			Type:       v1.SecretTypeBasicAuth,
			Immutable:  &immutable,
			Data: map[string][]byte{
				v1.BasicAuthUsernameKey: []byte(it.Username),
				v1.BasicAuthPasswordKey: password,
			},
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			secret, err = secrets.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil || secret == nil {
		return "", errBootstrapUnavailable
	}
	for key, value := range labels {
		if secret.Labels[key] != value {
			return "", errBootstrapUnavailable
		}
	}
	if secret.Name != name || secret.Namespace != c.DBNamespace() || secret.DeletionTimestamp != nil || len(secret.OwnerReferences) != 0 ||
		secret.Type != v1.SecretTypeBasicAuth || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 2 || len(secret.StringData) != 0 ||
		subtle.ConstantTimeCompare(secret.Data[v1.BasicAuthUsernameKey], []byte(it.Username)) != 1 ||
		subtle.ConstantTimeCompare(secret.Data[v1.BasicAuthPasswordKey], password) != 1 {
		return "", errBootstrapUnavailable
	}
	return name, nil
}
