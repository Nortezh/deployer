package k8s

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

type Secret struct {
	ID        string
	ProjectID string
	Data      map[string][]byte
	Type      v1.SecretType
}

func (c *Client) CreateSecret(ctx context.Context, obj Secret) error {
	s := c.client.CoreV1().Secrets(c.namespace)

	secret, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	if secret == nil {
		secret = &v1.Secret{}
	}
	secret.ObjectMeta.Name = obj.ID
	secret.ObjectMeta.Labels = map[string]string{
		"id":        obj.ID,
		"projectId": obj.ProjectID,
	}
	secret.ObjectMeta.Annotations = map[string]string{}
	secret.Type = obj.Type
	secret.Data = obj.Data

	_, err = s.Update(ctx, secret, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, secret, metav1.CreateOptions{})
	}
	return err
}

type SecretKey struct {
	ID        string
	ProjectID string
	Key       string
	Value     []byte
	Revision  int64
}

var errSecretKeyUnavailable = fmt.Errorf("secret key scope or revision unavailable")

func secretRevisionAnnotation(key string) string {
	return "nortezh.io/secret-revision-" + strings.TrimPrefix(key, "secret-")
}

func (c *Client) UpsertSecretKey(ctx context.Context, obj SecretKey) error {
	projectID, err := strconv.ParseInt(obj.ProjectID, 10, 64)
	if err != nil || projectID <= 0 || obj.ProjectID != strconv.FormatInt(projectID, 10) || obj.ID != ResourceID(projectID, "secrets") || c.namespace == "" || obj.Revision < 0 {
		return errSecretKeyUnavailable
	}
	if obj.Revision > 0 {
		secretID, err := strconv.ParseInt(strings.TrimPrefix(obj.Key, "secret-"), 10, 64)
		if err != nil || secretID <= 0 || obj.Key != fmt.Sprintf("secret-%d", secretID) {
			return errSecretKeyUnavailable
		}
	}
	annotation := secretRevisionAnnotation(obj.Key)
	s := c.client.CoreV1().Secrets(c.namespace)
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return errors.IsConflict(err) || errors.IsAlreadyExists(err) || errors.IsNotFound(err)
	}, func() error {
		secret, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			annotations := map[string]string{}
			if obj.Revision > 0 {
				annotations[annotation] = strconv.FormatInt(obj.Revision, 10)
			}
			_, err = s.Create(ctx, &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: obj.ID, Namespace: c.namespace, Labels: map[string]string{"id": obj.ID, "projectId": obj.ProjectID}, Annotations: annotations},
				Type:       v1.SecretTypeOpaque,
				Data:       map[string][]byte{obj.Key: obj.Value},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if secret.Name != obj.ID || secret.Namespace != c.namespace || secret.Labels["id"] != obj.ID || secret.Labels["projectId"] != obj.ProjectID || secret.Type != v1.SecretTypeOpaque || secret.DeletionTimestamp != nil || len(secret.OwnerReferences) != 0 {
			return errSecretKeyUnavailable
		}
		if marker, ok := secret.Annotations[annotation]; ok {
			current, err := strconv.ParseInt(marker, 10, 64)
			if err != nil || current <= 0 || marker != strconv.FormatInt(current, 10) || obj.Revision < current || (obj.Revision == current && !bytes.Equal(secret.Data[obj.Key], obj.Value)) {
				return errSecretKeyUnavailable
			}
		}
		if obj.Revision > 0 {
			if secret.Annotations == nil {
				secret.Annotations = make(map[string]string)
			}
			secret.Annotations[annotation] = strconv.FormatInt(obj.Revision, 10)
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[obj.Key] = obj.Value
		_, err = s.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
}

// ReadSyncedSecretKey returns transient bytes only; callers must never log or serialize them.
// Revision evidence assumes compatible writers; it does not fence old binaries or authorize execution.
func (c *Client) ReadSyncedSecretKey(ctx context.Context, projectID, secretID, revision int64) ([]byte, error) {
	if c.namespace == "" || projectID <= 0 || secretID <= 0 || revision <= 0 {
		return nil, errSecretKeyUnavailable
	}
	id := ResourceID(projectID, "secrets")
	key := fmt.Sprintf("secret-%d", secretID)
	secret, err := c.client.CoreV1().Secrets(c.namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, errSecretKeyUnavailable
	}
	if secret.Name != id || secret.Namespace != c.namespace || secret.Labels["id"] != id || secret.Labels["projectId"] != strconv.FormatInt(projectID, 10) || secret.Type != v1.SecretTypeOpaque || secret.DeletionTimestamp != nil || len(secret.OwnerReferences) != 0 ||
		secret.Annotations[secretRevisionAnnotation(key)] != strconv.FormatInt(revision, 10) || len(secret.Data[key]) == 0 {
		return nil, errSecretKeyUnavailable
	}
	return bytes.Clone(secret.Data[key]), nil
}

func (c *Client) DeleteSecretKey(ctx context.Context, id, key string) error {
	s := c.client.CoreV1().Secrets(c.namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := s.Get(ctx, id, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, ok := secret.Data[key]; !ok {
			return nil
		}
		delete(secret.Data, key)
		delete(secret.Annotations, secretRevisionAnnotation(key))
		if len(secret.Data) == 0 {
			err = s.Delete(ctx, id, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &secret.ResourceVersion}})
		} else {
			_, err = s.Update(ctx, secret, metav1.UpdateOptions{})
		}
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	})
}

type SecretDockerConfigJSON struct {
	ID        string
	ProjectID string
	JSON      []byte
}

func (c *Client) CreateSecretDockerConfigJSON(ctx context.Context, obj SecretDockerConfigJSON) error {
	return c.CreateSecret(ctx, Secret{
		ID:        obj.ID,
		ProjectID: obj.ProjectID,
		Data: map[string][]byte{
			v1.DockerConfigJsonKey: obj.JSON,
		},
		Type: v1.SecretTypeDockerConfigJson,
	})
}

func (c *Client) DeleteSecret(ctx context.Context, id string) error {
	err := c.client.CoreV1().Secrets(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
