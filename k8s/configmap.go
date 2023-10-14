package k8s

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ConfigMap struct {
	ID        string
	ProjectID string
	Data      map[string]string // key => data
}

func (c *Client) CreateConfigMap(ctx context.Context, obj ConfigMap) error {
	s := c.client.CoreV1().ConfigMaps(c.namespace)

	configMap, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	if configMap == nil {
		configMap = &v1.ConfigMap{}
	}
	configMap.ObjectMeta.Name = obj.ID
	configMap.ObjectMeta.Labels = map[string]string{
		"id":        obj.ID,
		"projectId": obj.ProjectID,
	}
	configMap.ObjectMeta.Annotations = map[string]string{}
	configMap.Data = obj.Data

	_, err = s.Update(ctx, configMap, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, configMap, metav1.CreateOptions{})
	}
	return err
}

func (c *Client) DeleteConfigMap(ctx context.Context, id string) error {
	err := c.client.CoreV1().ConfigMaps(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
