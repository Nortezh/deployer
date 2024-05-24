package k8s

import (
	"context"
	"fmt"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

type PersistentVolumeClaim struct {
	ID           string
	ProjectID    string
	Size         int64
	StorageClass string
}

func (c *Client) CreatePersistentVolumeClaim(ctx context.Context, obj PersistentVolumeClaim) error {
	s := c.client.CoreV1().PersistentVolumeClaims(c.namespace)

	pvc, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	if pvc == nil {
		pvc = &v1.PersistentVolumeClaim{}
	}
	pvc.ObjectMeta.Name = obj.ID
	pvc.ObjectMeta.Labels = map[string]string{
		"id":        obj.ID,
		"projectId": obj.ProjectID,
	}
	pvc.Spec.AccessModes = []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
	pvc.Spec.Resources = v1.VolumeResourceRequirements{
		Requests: v1.ResourceList{
			"storage": resource.MustParse(strconv.FormatInt(obj.Size, 10) + "Gi"),
		},
	}
	if obj.StorageClass != "" {
		pvc.Spec.StorageClassName = pointer.String(obj.StorageClass)
	}

	_, err = s.Update(ctx, pvc, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, pvc, metav1.CreateOptions{})
	}
	return err
}

func (c *Client) DeletePersistentVolumeClaim(ctx context.Context, id string) error {
	err := c.client.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

type PersistentVolumeClaimForReplicaSet struct {
	ID           string
	ProjectID    string
	Revision     int64
	Size         int64
	StorageClass string
}

func (c *Client) CreatePersistentVolumeClaimForReplicaSet(ctx context.Context, obj PersistentVolumeClaimForReplicaSet) error {
	s := c.client.CoreV1().PersistentVolumeClaims(c.namespace)

	id := fmt.Sprintf("%s-%d", obj.ID, obj.Revision)

	pvc := &v1.PersistentVolumeClaim{}
	pvc.ObjectMeta.Name = id
	pvc.ObjectMeta.Labels = map[string]string{
		"id":        obj.ID,
		"revision":  strconv.FormatInt(obj.Revision, 10),
		"projectId": obj.ProjectID,
	}
	pvc.Spec.AccessModes = []v1.PersistentVolumeAccessMode{
		v1.ReadWriteOnce,
	}
	pvc.Spec.Resources = v1.VolumeResourceRequirements{
		Requests: v1.ResourceList{
			"storage": resource.MustParse(strconv.FormatInt(obj.Size, 10) + "Gi"),
		},
	}
	if obj.StorageClass != "" {
		pvc.Spec.StorageClassName = pointer.String(obj.StorageClass)
	}

	_, err := s.Create(ctx, pvc, metav1.CreateOptions{})
	return err
}
