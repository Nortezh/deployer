package k8s

import (
	"context"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
)

type Service struct {
	ID         string
	ProjectID  string
	Port       int
	Protocol   string
	ExposeNode bool
	H2CP       bool
}

func (c *Client) CreateService(ctx context.Context, obj Service) error {
	s := c.client.CoreV1().Services(c.namespace)

	svc, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	labels := map[string]string{
		"id":        obj.ID,
		"projectId": obj.ProjectID,
	}

	if svc == nil {
		svc = &v1.Service{}
	}

	if !obj.ExposeNode {
		// self-healing
		if ip := svc.Spec.ClusterIP; ip != "" && ip != "None" {
			s.Delete(ctx, obj.ID, metav1.DeleteOptions{})
			svc = &v1.Service{}
		}
	}

	svc.ObjectMeta.Name = obj.ID
	svc.ObjectMeta.Labels = labels
	svc.Spec.Selector = labels

	if !obj.ExposeNode {
		svc.Spec.Type = v1.ServiceTypeClusterIP
		svc.Spec.ClusterIP = "None"
		svc.Spec.Ports = []v1.ServicePort{
			{
				Name:       "http",
				Protocol:   v1.ProtocolTCP,
				Port:       int32(obj.Port),
				TargetPort: intstr.FromInt(obj.Port),
			},
		}

		if obj.Protocol != "" {
			svc.Spec.Ports[0].AppProtocol = pointer.String(obj.Protocol)
		}

		if obj.H2CP {
			svc.Spec.Ports[0].Port = 1
			svc.Spec.Ports[0].TargetPort = intstr.FromInt(1)
			svc.Spec.Ports[0].AppProtocol = pointer.String("h2c")
		}
	} else {
		svc.Spec.Type = v1.ServiceTypeNodePort
		if len(svc.Spec.Ports) == 0 {
			svc.Spec.Ports = append(svc.Spec.Ports, v1.ServicePort{})
		}
		svc.Spec.Ports[0].Protocol = v1.ProtocolTCP
		svc.Spec.Ports[0].Port = int32(obj.Port)
		svc.Spec.Ports[0].TargetPort = intstr.FromInt(obj.Port)
	}

	_, err = s.Update(ctx, svc, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, svc, metav1.CreateOptions{})
	}
	return err
}

func (c *Client) DeleteService(ctx context.Context, id string) error {
	err := c.client.CoreV1().Services(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) GetNodePort(ctx context.Context, id string) (int, error) {
	s := c.client.CoreV1().Services(c.namespace)

	svc, err := s.Get(ctx, id, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	if len(svc.Spec.Ports) == 0 {
		return 0, nil
	}
	return int(svc.Spec.Ports[0].NodePort), nil
}

type ServiceForReplicaSet struct {
	ID         string
	Revision   int64
	ProjectID  string
	Port       int
	ExposeNode bool
}

func (c *Client) CreateServiceForReplicaSet(ctx context.Context, obj ServiceForReplicaSet) error {
	s := c.client.CoreV1().Services(c.namespace)

	svc, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	labels := map[string]string{
		"id":        obj.ID,
		"revision":  strconv.FormatInt(obj.Revision, 10),
		"projectId": obj.ProjectID,
	}

	if svc == nil {
		svc = &v1.Service{}
	}

	if !obj.ExposeNode {
		// self-healing
		if ip := svc.Spec.ClusterIP; ip != "" && ip != "None" {
			s.Delete(ctx, obj.ID, metav1.DeleteOptions{})
			svc = &v1.Service{}
		}
	}

	svc.ObjectMeta.Name = obj.ID
	svc.ObjectMeta.Labels = labels
	svc.Spec.Selector = labels

	if !obj.ExposeNode {
		svc.Spec.Type = v1.ServiceTypeClusterIP
		svc.Spec.Ports = []v1.ServicePort{
			{
				Name:       "app",
				Protocol:   v1.ProtocolTCP,
				Port:       int32(obj.Port),
				TargetPort: intstr.FromInt(obj.Port),
			},
		}
	} else {
		svc.Spec.Type = v1.ServiceTypeNodePort
		if len(svc.Spec.Ports) == 0 {
			svc.Spec.Ports = append(svc.Spec.Ports, v1.ServicePort{})
		}
		svc.Spec.Ports[0].Protocol = v1.ProtocolTCP
		svc.Spec.Ports[0].Port = int32(obj.Port)
		svc.Spec.Ports[0].TargetPort = intstr.FromInt(obj.Port)
	}

	_, err = s.Update(ctx, svc, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, svc, metav1.CreateOptions{})
	}
	return err
}
