package k8s

import (
	"context"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

type HorizontalPodAutoscaler struct {
	ID            string
	ProjectID     string
	MinReplicas   int
	MaxReplicas   int
	TargetPercent int
}

func (c *Client) CreateHorizontalPodAutoscaler(ctx context.Context, obj HorizontalPodAutoscaler) error {
	s := c.client.AutoscalingV1().HorizontalPodAutoscalers(c.namespace)

	hpa, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	if hpa == nil {
		hpa = &autoscalingv1.HorizontalPodAutoscaler{}
	}
	hpa.ObjectMeta.Name = obj.ID
	hpa.ObjectMeta.Labels = map[string]string{
		"id":        obj.ID,
		"projectId": obj.ProjectID,
	}
	hpa.Spec = autoscalingv1.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       obj.ID,
		},
		MinReplicas:                    pointer.Int32(int32(obj.MinReplicas)),
		MaxReplicas:                    int32(obj.MaxReplicas),
		TargetCPUUtilizationPercentage: pointer.Int32(int32(obj.TargetPercent)),
	}

	_, err = s.Update(ctx, hpa, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, hpa, metav1.CreateOptions{})
	}
	return err
}

func (c *Client) DeleteHorizontalPodAutoscaler(ctx context.Context, id string) error {
	err := c.client.AutoscalingV1().HorizontalPodAutoscalers(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
