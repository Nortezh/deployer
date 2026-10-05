package cnpg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const allocationMap = "kdb-port-allocations"

type endpoint struct{ Node, Host, Port string }

func pinnedImage(image string) bool {
	i := strings.LastIndex(image, "@sha256:")
	if i <= 0 || len(image)-i != len("@sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(image[i+len("@sha256:"):])
	return err == nil
}

func portBounds(value string) (int, int, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return 0, 0, errors.New("CNPG requires one explicit port range")
	}
	lo, e1 := strconv.Atoi(parts[0])
	hi, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil || lo < 1 || hi < lo || hi > 65535 {
		return 0, 0, errors.New("CNPG invalid port range")
	}
	return lo, hi, nil
}

// Share KDB's existing map and resourceVersion conflict protocol. Never create
// a new map: a missing map means the platform allocator is not prepared.
func reserve(ctx context.Context, client kubernetes.Interface, namespace, ports, owner string) (result endpoint, err error) {
	lo, hi, err := portBounds(ports)
	if err != nil {
		return result, err
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "kdb/role=lb"})
	if err != nil {
		return result, errors.New("CNPG LB nodes unavailable")
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	candidates := map[string]endpoint{}
	for _, node := range nodes.Items {
		host := node.Annotations["kdb.io/host"]
		if host == "" {
			for _, address := range node.Status.Addresses {
				if address.Type == "InternalIP" {
					host = address.Address
					break
				}
			}
		}
		if host != "" {
			candidates[node.Name] = endpoint{Node: node.Name, Host: host}
		}
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		maps := client.CoreV1().ConfigMaps(namespace)
		cm, err := maps.Get(ctx, allocationMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		var existing string
		for key, value := range cm.Data {
			if value != owner {
				continue
			}
			if existing != "" {
				return errors.New("CNPG duplicate existing allocation")
			}
			existing = key
		}
		if existing != "" {
			i := strings.LastIndex(existing, "_")
			if i < 0 {
				return errors.New("CNPG invalid existing allocation")
			}
			ep, ok := candidates[existing[:i]]
			port, err := strconv.Atoi(existing[i+1:])
			if !ok || err != nil || port < lo || port > hi {
				return errors.New("CNPG existing allocation outside approved profile")
			}
			ep.Port = strconv.Itoa(port)
			result = ep
			return nil
		}
		for _, node := range nodes.Items {
			ep, ok := candidates[node.Name]
			if !ok {
				continue
			}
			for port := lo; port <= hi; port++ {
				key := fmt.Sprintf("%s_%d", ep.Node, port)
				if _, occupied := cm.Data[key]; occupied {
					continue
				}
				if cm.Data == nil {
					cm.Data = map[string]string{}
				}
				cm.Data[key] = owner
				if _, err := maps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
					return err
				}
				ep.Port = strconv.Itoa(port)
				result = ep
				return nil
			}
		}
		return errors.New("CNPG no available LB ports")
	})
	if err != nil {
		return endpoint{}, errors.New("CNPG port reservation unavailable")
	}
	return result, nil
}

func release(ctx context.Context, client kubernetes.Interface, namespace, owner string) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		maps := client.CoreV1().ConfigMaps(namespace)
		cm, err := maps.Get(ctx, allocationMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed := false
		for key, value := range cm.Data {
			if value == owner {
				delete(cm.Data, key)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		_, err = maps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return errors.New("CNPG port release unavailable")
	}
	return nil
}
