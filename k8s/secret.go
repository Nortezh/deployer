package k8s

import (
	"context"

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
}

func (c *Client) UpsertSecretKey(ctx context.Context, obj SecretKey) error {
	s := c.client.CoreV1().Secrets(c.namespace)
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return errors.IsConflict(err) || errors.IsAlreadyExists(err) || errors.IsNotFound(err)
	}, func() error {
		secret, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			_, err = s.Create(ctx, &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: obj.ID, Labels: map[string]string{"id": obj.ID, "projectId": obj.ProjectID}},
				Type:       v1.SecretTypeOpaque,
				Data:       map[string][]byte{obj.Key: obj.Value},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		secret.Labels = map[string]string{"id": obj.ID, "projectId": obj.ProjectID}
		secret.Type = v1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = make(map[string][]byte)
		}
		secret.Data[obj.Key] = obj.Value
		_, err = s.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
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
