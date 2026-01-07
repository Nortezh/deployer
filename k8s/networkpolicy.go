package k8s

import (
	"context"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type NetworkPolicy struct {
	ID                      string
	ProjectID               string
	AllowIncomingProjectIDs []string
}

func (c *Client) ApplyNetworkPolicy(ctx context.Context, obj NetworkPolicy) error {
	s := c.client.NetworkingV1().NetworkPolicies(c.namespace)

	policy, err := s.Get(ctx, obj.ID, metav1.GetOptions{})

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

	// Build ingress rules - allow from same project and AllowFromProjectIDs
	var ingressPeers []networkingv1.NetworkPolicyPeer

	// Allow from same project
	ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"projectId": obj.ProjectID,
			},
		},
	})

	// Allow from additional projects
	for _, allowedProjectID := range obj.AllowIncomingProjectIDs {
		ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
			PodSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"projectId": allowedProjectID,
				},
			},
		})
	}

	if policy == nil {
		policy = &networkingv1.NetworkPolicy{}
	}

	policy.ObjectMeta.Name = obj.ID
	policy.ObjectMeta.Labels = labels
	policy.Spec = networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{
				"projectId": obj.ProjectID,
			},
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
		},
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{From: ingressPeers},
		},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{}, // empty rule allows all egress
		},
	}

	_, err = s.Update(ctx, policy, metav1.UpdateOptions{})

	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, policy, metav1.CreateOptions{})
	}

	if err != nil {
		return err
	}

	return nil
}

func (c *Client) DeleteNetworkPolicy(ctx context.Context, id string) error {
	err := c.client.
		NetworkingV1().
		NetworkPolicies(c.namespace).
		Delete(ctx, id, metav1.DeleteOptions{})

	if errors.IsNotFound(err) {
		return nil
	}

	return err
}
