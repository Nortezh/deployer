package k8s

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ServiceAccount struct {
	ID        string
	ProjectID string
	GSA       string
}

func (c *Client) CreateServiceAccount(ctx context.Context, obj ServiceAccount) error {
	s := c.client.CoreV1().ServiceAccounts(c.namespace)

	sa, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	if sa == nil {
		sa = &v1.ServiceAccount{}
	}
	sa.ObjectMeta.Name = obj.ID
	sa.ObjectMeta.Labels = map[string]string{
		"id":        obj.ID,
		"projectId": obj.ProjectID,
	}
	sa.ObjectMeta.Annotations = map[string]string{}
	if obj.GSA != "" {
		sa.Annotations["iam.gke.io/gcp-service-account"] = obj.GSA
	}

	_, err = s.Update(ctx, sa, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, sa, metav1.CreateOptions{})
	}
	return err
}

func (c *Client) DeleteServiceAccount(ctx context.Context, id string) error {
	err := c.client.CoreV1().ServiceAccounts(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
