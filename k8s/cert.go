package k8s

import (
	"context"

	v1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Certificate struct {
	ID        string
	ProjectID string
	Domain    string
}

func (c *Client) CreateCertificate(ctx context.Context, obj Certificate) error {
	s := c.certManagerClient.CertmanagerV1().Certificates(c.namespace)

	cert, err := s.Get(ctx, obj.ID, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		err = nil
	}
	if err != nil {
		return err
	}

	labels := map[string]string{
		"projectId": obj.ProjectID,
	}

	if cert == nil {
		cert = &v1.Certificate{}
	}

	cert.ObjectMeta.Name = obj.ID
	cert.ObjectMeta.Labels = labels
	cert.Spec = v1.CertificateSpec{
		CommonName: obj.Domain,
		DNSNames:   []string{obj.Domain},
		IssuerRef: cmmeta.ObjectReference{
			Name: "letsencrypt",
			Kind: v1.IssuerKind,
		},
		PrivateKey: &v1.CertificatePrivateKey{
			Algorithm: v1.ECDSAKeyAlgorithm,
			Size:      256,
		},
		SecretName: "tls-" + obj.ID,
	}

	_, err = s.Update(ctx, cert, metav1.UpdateOptions{})
	if errors.IsNotFound(err) {
		_, err = s.Create(ctx, cert, metav1.CreateOptions{})
	}
	return err
}

func (c *Client) DeleteCertificate(ctx context.Context, id string) error {
	err := c.certManagerClient.CertmanagerV1().Certificates(c.namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
