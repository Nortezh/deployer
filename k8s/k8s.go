package k8s

import (
	"fmt"

	certmanager "github.com/cert-manager/cert-manager/pkg/client/clientset/versioned"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Client struct {
	client            *kubernetes.Clientset
	certManagerClient *certmanager.Clientset
	dynamic           dynamic.Interface
	namespace         string
}

func NewClient(namespace string) (*Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	certManagerClient, err := certmanager.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &Client{
		client:            client,
		certManagerClient: certManagerClient,
		dynamic:           dyn,
		namespace:         namespace,
	}, nil
}

func NewLocalClient(namespace string) (*Client, error) {
	config := &rest.Config{
		Host:    "localhost:8001",
		APIPath: "/",
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	certManagerClient, err := certmanager.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &Client{
		client:            client,
		certManagerClient: certManagerClient,
		dynamic:           dyn,
		namespace:         namespace,
	}, nil
}

// Dynamic exposes the dynamic client for applying arbitrary CRs (e.g. kdb.io managed databases).
func (c *Client) Dynamic() dynamic.Interface { return c.dynamic }

// Namespace is the deployer's configured namespace (app workloads).
func (c *Client) Namespace() string { return c.namespace }

// DBNamespace is the dedicated namespace for managed-database CRs (<namespace>-database).
func (c *Client) DBNamespace() string { return c.namespace + "-database" }

// ResourceID is the canonical k8s resource name for a project-scoped resource: "<name>-<projectID>".
// Single source of truth — main's private resourceID delegates here.
func ResourceID(projectID int64, name string) string {
	return fmt.Sprintf("%s-%d", name, projectID)
}

// func getServicePort(namespace, serviceName, portName string) int {
// 	svc, err := client.CoreV1().Services(namespace).Get(serviceName, metav1.GetOptions{})
// 	if err != nil {
// 		slog.Error("can not get service", "namespace", namespace, "service", serviceName, "error", err)
// 		return 0
// 	}
//
// 	for _, port := range svc.Spec.Ports {
// 		if port.Name == portName {
// 			return int(port.Port)
// 		}
// 	}
// 	return 0
// }
//
// func getIngresses(namespace string) ([]v1beta1.Ingress, error) {
// 	list, err := client.ExtensionsV1beta1().Ingresses(namespace).List(metav1.ListOptions{})
// 	if err != nil {
// 		slog.Error("can not list ingresses", "error", err)
// 		return nil, err
// 	}
// 	return list.Items, nil
// }
//
// func getSecretTLS(namespace, name string) (cert []byte, key []byte, err error) {
// 	s, err := client.CoreV1().Secrets(namespace).Get(name, metav1.GetOptions{})
// 	if err != nil {
// 		return nil, nil, err
// 	}
//
// 	cert = s.Data["tls.crt"]
// 	key = s.Data["tls.key"]
// 	return
// }
