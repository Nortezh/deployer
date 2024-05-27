package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/acoshift/configfile"
	"github.com/deploys-app/api"
	"github.com/deploys-app/api/client"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/resource"
	knet "k8s.io/apimachinery/pkg/util/net"

	"github.com/deploys-app/deployer/k8s"
	// "github.com/deploys-app/deploys/logs"
)

func main() {
	cfg := configfile.NewEnvReader()

	locationID := cfg.MustString("location")
	projectID := cfg.String("project_id")
	namespace := cfg.String("namespace")
	apiEndpoint := cfg.String("api_endpoint")

	token := cfg.String("token")
	if token == "" {
		slog.Error("token required")
		os.Exit(1)
	}

	var k8sClient *k8s.Client
	if cfg.Bool("local") {
		var err error
		k8sClient, err = k8s.NewLocalClient(namespace)
		if err != nil {
			slog.Error("can not create k8s client", "error", err)
			os.Exit(1)
		}
	} else {
		var err error
		k8sClient, err = k8s.NewClient(namespace)
		if err != nil {
			slog.Error("can not create k8s client", "error", err)
			os.Exit(1)
		}
	}

	slog.Info("start deployer")
	slog.Info("config",
		"location", locationID,
		"project_id", projectID,
		"namespace", namespace,
		"api_endpoint", apiEndpoint,
	)

	ctx := context.Background()

	chEvent := make(chan struct{})

	eventTopic := cfg.String("event_topic")
	if eventTopic != "" {
		pubSubClient, err := pubsub.NewClient(ctx, projectID)
		if err != nil {
			slog.Error("can not create pubsub client", "error", err)
			os.Exit(1)
		}

		if pubSubClient != nil {
			defer pubSubClient.Close()

			subscription := locationID + "." + eventTopic

			_, err = pubSubClient.CreateSubscription(ctx, subscription, pubsub.SubscriptionConfig{
				Topic:             pubSubClient.Topic(eventTopic),
				AckDeadline:       10 * time.Second,
				RetentionDuration: time.Hour,
				ExpirationPolicy:  24 * time.Hour,
			})
			if err != nil {
				slog.Info("creating subscription error", "error", err)
			}

			go func() {
				err := pubSubClient.Subscription(subscription).Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
					slog.Info("received event", "data", string(msg.Data))

					msg.Ack()

					select {
					case chEvent <- struct{}{}:
					default:
					}
				})
				if err != nil {
					slog.Error("can't subscribe", "error", err)
					if !cfg.Bool("local") {
						os.Exit(1)
					}
				}
			}()
		}
	}

	deployer := (&client.Client{
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		Endpoint: apiEndpoint,
		Auth: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
		},
	}).Deployer()

	w := Worker{
		Deployer:     deployer,
		Client:       k8sClient,
		RuntimeClass: cfg.String("runtime_class"),
		H2CP:         cfg.Bool("h2cp"),
		Cert:         cfg.Bool("cert"),
		CPULimit:     cfg.StringDefault("cpu_limit", defaultLimitCPU),
		MemoryLimit:  cfg.StringDefault("memory_limit", defaultMemoryLimit),
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)

	for {
		w.Run()

		select {
		case <-stop:
			return
		case <-time.After(10 * time.Second):
		case <-chEvent:
		}
	}
}

const (
	defaultRequestCPU  = "0.01"
	defaultLimitCPU    = "2"
	defaultMemoryLimit = "2Gi"
)

type Worker struct {
	Deployer     api.Deployer
	Client       *k8s.Client
	RuntimeClass string
	H2CP         bool
	Cert         bool // manage cert using cert manager
	CPULimit     string
	MemoryLimit  string

	// state
	location *api.LocationItem
	results  []*api.DeployerSetResultItem
}

func (w *Worker) cpuLimit(limit string) string {
	if limit == "" || limit == "0" {
		return w.CPULimit
	}
	return limit
}

func (w *Worker) memoryLimit(memory string) string {
	if memory == "" || memory == "0" {
		return w.MemoryLimit
	}
	m, _ := resource.ParseQuantity(memory)
	if m.IsZero() {
		return "2Gi"
	}
	return m.String()
}

func (w *Worker) normalizeRequestCPU(request string) string {
	if request == "" {
		return defaultRequestCPU
	}
	if request == "0" {
		return defaultRequestCPU
	}
	_, err := resource.ParseQuantity(request)
	if err != nil {
		return defaultRequestCPU
	}
	return request
}

func (w *Worker) normalizeLimitCPU(limit string) string {
	// preserve old behavior when not setting limit, to support single-thread app
	if limit == "" || limit == "0" {
		return "1"
	}
	_, err := resource.ParseQuantity(w.cpuLimit(limit))
	if err != nil {
		return "1"
	}
	return limit
}

// target for 1 limit cpu (for single thread application)
func (w *Worker) targetCPUPercent(request, limit string) int {
	reqQuantity := resource.MustParse(w.normalizeRequestCPU(request))
	limQuantity := resource.MustParse(w.normalizeLimitCPU(limit))
	req := float64(reqQuantity.MilliValue()) / 1000
	lim := float64(limQuantity.MilliValue()) / 1000

	// 80 * limit / request
	return int(80 * lim / req)
}

func (w *Worker) Run() {
	ctx := context.Background()

	if w.location == nil {
		var err error
		w.location, err = w.Deployer.GetLocation(ctx, &api.Empty{})
		if err != nil {
			slog.Error("can not get location from api", "error", err)
			return
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	w.results = nil

	commands, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil {
		slog.Error("can not get commands from api", "error", err)
		return
	}

	if l := len(*commands); l > 0 {
		slog.Info("got commands", "count", l)
	}

	for _, x := range *commands {
		forceFlush := false

		switch {
		case x.PullSecretCreate != nil:
			x := x.PullSecretCreate
			w.pullSecretCreate(ctx, x)
		case x.PullSecretDelete != nil:
			x := x.PullSecretDelete
			w.pullSecretDelete(ctx, x)
		case x.WorkloadIdentityCreate != nil:
			x := x.WorkloadIdentityCreate
			w.workloadIdentityCreate(ctx, x)
		case x.WorkloadIdentityDelete != nil:
			x := x.WorkloadIdentityDelete
			w.workloadIdentityDelete(ctx, x)
		case x.DiskCreate != nil:
			x := x.DiskCreate
			w.diskCreate(ctx, x)
		case x.DiskDelete != nil:
			x := x.DiskDelete
			w.diskDelete(ctx, x)
		case x.DeploymentDeploy != nil:
			x := x.DeploymentDeploy
			w.deploymentDeploy(ctx, x)
		case x.DeploymentDelete != nil:
			x := x.DeploymentDelete
			w.deploymentDelete(ctx, x)
		case x.DeploymentPause != nil:
			x := x.DeploymentPause
			w.deploymentPause(ctx, x)
		case x.DeploymentCleanup != nil:
			x := x.DeploymentCleanup
			w.deploymentCleanupResource(ctx, x)
		case x.RouteCreate != nil:
			x := x.RouteCreate
			w.routeCreate(ctx, x)
			forceFlush = true
		case x.RouteDelete != nil:
			x := x.RouteDelete
			w.routeDelete(ctx, x)
			forceFlush = true
		}

		if forceFlush || len(w.results) > 3 {
			w.flushResults()
		}
	}

	w.flushResults()
}

func (w *Worker) flushResults() {
	if len(w.results) == 0 {
		return
	}
	slog.Info("flushing results", "count", len(w.results))

	results := api.DeployerSetResult(w.results)
	_, err := w.Deployer.SetResults(context.Background(), &results)
	if err != nil {
		slog.Error("can not set results", "error", err)
		return
	}
	w.results = nil
}

func (w *Worker) pullSecretCreate(ctx context.Context, it *api.DeployerCommandPullSecretCreate) {
	slog.Info("pullsecret: creating", "id", it.ID)

	id := pullSecretResourceID(it.ProjectID, it.Name)
	projectID := idString(it.ProjectID)

	jsonData, _ := base64.StdEncoding.DecodeString(it.Value)

	err := w.Client.CreateSecretDockerConfigJSON(ctx, k8s.SecretDockerConfigJSON{
		ID:        id,
		ProjectID: projectID,
		JSON:      jsonData,
	})
	if err != nil {
		slog.Error("pullsecret: creating error", "id", it.ID, "error", err)
		return
	}

	slog.Info("pullsecret: created", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		PullSecretCreate: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) pullSecretDelete(ctx context.Context, it *api.DeployerCommandMetadata) {
	slog.Info("pullsecret: deleting", "id", it.ID)

	err := w.Client.DeleteSecret(ctx, pullSecretResourceID(it.ProjectID, it.Name))
	if err != nil {
		slog.Error("pullsecret: deleting error", "id", it.ID, "error", err)
		return
	}

	slog.Info("pullsecret: deleted", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		PullSecretDelete: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) deploymentDeploy(ctx context.Context, it *api.DeployerCommandDeploymentDeploy) {
	slog.Info("deployment: deploying", "id", it.ID)

	id := resourceID(it.ProjectID, it.Name)
	projectID := idString(it.ProjectID)

	var result api.DeployerSetResultItemDeploy

	f := func() error {
		// reset each retry
		result = api.DeployerSetResultItemDeploy{
			ID:       it.ID,
			Revision: it.Revision,
		}

		sidecarConfigs := lo.Map(it.Spec.Sidecars, func(x *api.Sidecar, _ int) *api.SidecarConfig {
			return x.Config()
		})

		configMapData, bindData := prepareMountData(it.Spec.MountData, sidecarMountData(sidecarConfigs))
		cm := k8s.ConfigMap{
			ID:        id,
			ProjectID: projectID,
			Data:      configMapData,
		}

		err := w.Client.CreateConfigMap(ctx, cm)
		if err != nil {
			slog.Error("deployment: creating config map error", "id", it.ID, "error", err)
			return err
		}

		switch it.Type {
		case api.DeploymentTypeWebService:
			h2cp := w.H2CP && (it.Spec.Protocol == "http" || it.Spec.Protocol == "https")

			deploy := k8s.Deployment{
				ID:            id,
				ProjectID:     projectID,
				Name:          it.Name,
				Revision:      it.Revision,
				Image:         it.Spec.Image,
				Env:           it.Spec.Env,
				Command:       it.Spec.Command,
				Args:          it.Spec.Args,
				Replicas:      it.Spec.MinReplicas,
				ExposePort:    it.Spec.Port,
				Annotations:   it.Spec.Annotations,
				RequestCPU:    w.normalizeRequestCPU(it.Spec.CPU),
				RequestMemory: it.Spec.Memory,
				LimitCPU:      w.cpuLimit(it.Spec.CPULimit),
				LimitMemory:   w.memoryLimit(it.Spec.Memory),
				RuntimeClass:  w.RuntimeClass,
				Pool: k8s.PoolConfig{
					Name:  it.BillingConfig.Pool,
					Share: it.BillingConfig.SharePool,
				},
				SA:         resourceID(it.ProjectID, it.Spec.WorkloadIdentityName),
				PullSecret: pullSecretResourceID(it.ProjectID, it.Spec.PullSecretName),
				Disk: k8s.Disk{
					Name:      resourceID(it.ProjectID, it.Spec.DiskName),
					MountPath: it.Spec.DiskMountPath,
					SubPath:   it.Spec.DiskSubPath,
				},
				BindConfigMap: bindData,
				H2CP:          h2cp,
				Protocol:      string(it.Spec.Protocol),
				Sidecars:      sidecarConfigs,
			}

			err = w.Client.CreateDeployment(ctx, deploy)
			if err != nil {
				slog.Error("deployment: creating deployment error", "id", it.ID, "error", err)
				return err
			}

			err = w.Client.CreateService(ctx, k8s.Service{
				ID:        id,
				ProjectID: projectID,
				Port:      it.Spec.Port,
				Protocol:  string(it.Spec.Protocol),
				H2CP:      h2cp,
			})
			if err != nil {
				slog.Error("deployment: creating service error", "id", it.ID, "error", err)
				return err
			}

			// internal ingress
			err = w.Client.CreateIngress(ctx, k8s.Ingress{
				ID:        id + "-internal",
				Service:   id,
				ProjectID: projectID,
				Domain:    fmt.Sprintf("%s.internal%s", id, w.location.DomainSuffix),
				Path:      "/",
				Internal:  true,
			})
			if err != nil {
				slog.Error("deployment: creating internal ingress error", "id", it.ID, "error", err)
				return err
			}

			if it.Spec.Internal {
				// delete external ingress
				err = w.Client.DeleteIngress(ctx, id)
				if err != nil {
					slog.Error("deployment: deleting external ingress error", "id", it.ID, "error", err)
					return err
				}
			} else {
				// external ingress
				err = w.Client.CreateIngress(ctx, k8s.Ingress{
					ID:        id,
					Service:   id,
					ProjectID: projectID,
					Domain:    fmt.Sprintf("%s%s", id, w.location.DomainSuffix),
					Path:      "/",
				})
				if err != nil {
					slog.Error("deployment: creating external ingress error", "id", it.ID, "error", err)
					return err
				}
			}

			if it.Spec.MinReplicas != it.Spec.MaxReplicas {
				err = w.Client.CreateHorizontalPodAutoscaler(ctx, k8s.HorizontalPodAutoscaler{
					ID:            id,
					ProjectID:     projectID,
					MinReplicas:   it.Spec.MinReplicas,
					MaxReplicas:   it.Spec.MaxReplicas,
					TargetPercent: w.targetCPUPercent(it.Spec.CPU, it.Spec.CPULimit),
				})
				if err != nil {
					slog.Error("deployment: creating hpa error", "id", it.ID, "error", err)
					return err
				}
			} else {
				err = w.Client.DeleteHorizontalPodAutoscaler(ctx, id)
				if err != nil {
					slog.Error("deployment: deleting hpa error", "id", it.ID, "error", err)
					return err
				}
			}
		case api.DeploymentTypeWorker:
			deploy := k8s.Deployment{
				ID:            id,
				ProjectID:     projectID,
				Name:          it.Name,
				Revision:      it.Revision,
				Image:         it.Spec.Image,
				Env:           it.Spec.Env,
				Command:       it.Spec.Command,
				Args:          it.Spec.Args,
				Replicas:      it.Spec.MinReplicas,
				Annotations:   it.Spec.Annotations,
				RequestCPU:    w.normalizeRequestCPU(it.Spec.CPU),
				RequestMemory: it.Spec.Memory,
				LimitCPU:      w.cpuLimit(it.Spec.CPULimit),
				LimitMemory:   w.memoryLimit(it.Spec.Memory),
				RuntimeClass:  w.RuntimeClass,
				Pool: k8s.PoolConfig{
					Name:  it.BillingConfig.Pool,
					Share: it.BillingConfig.SharePool,
				},
				SA:         resourceID(it.ProjectID, it.Spec.WorkloadIdentityName),
				PullSecret: pullSecretResourceID(it.ProjectID, it.Spec.PullSecretName),
				Disk: k8s.Disk{
					Name:      resourceID(it.ProjectID, it.Spec.DiskName),
					MountPath: it.Spec.DiskMountPath,
					SubPath:   it.Spec.DiskSubPath,
				},
				BindConfigMap: bindData,
				Sidecars:      sidecarConfigs,
			}

			err = w.Client.CreateDeployment(ctx, deploy)
			if err != nil {
				slog.Error("deployment: creating deployment error", "id", it.ID, "error", err)
				return err
			}

			if it.Spec.MinReplicas != it.Spec.MaxReplicas {
				err = w.Client.CreateHorizontalPodAutoscaler(ctx, k8s.HorizontalPodAutoscaler{
					ID:            id,
					ProjectID:     projectID,
					MinReplicas:   it.Spec.MinReplicas,
					MaxReplicas:   it.Spec.MaxReplicas,
					TargetPercent: w.targetCPUPercent(it.Spec.CPU, it.Spec.CPULimit),
				})
				if err != nil {
					slog.Error("deployment: creating hpa error", "id", it.ID, "error", err)
					return err
				}
			} else {
				err = w.Client.DeleteHorizontalPodAutoscaler(ctx, id)
				if err != nil {
					slog.Error("deployment: deleting hpa error", "id", it.ID, "error", err)
					return err
				}
			}
		case api.DeploymentTypeCronJob:
			cj := k8s.CronJob{
				ID:            id,
				ProjectID:     projectID,
				Name:          it.Name,
				Revision:      it.Revision,
				Image:         it.Spec.Image,
				Env:           it.Spec.Env,
				Command:       it.Spec.Command,
				Args:          it.Spec.Args,
				Schedule:      it.Spec.Schedule,
				RequestCPU:    w.normalizeRequestCPU(it.Spec.CPU),
				RequestMemory: it.Spec.Memory,
				LimitCPU:      w.cpuLimit(it.Spec.CPULimit),
				LimitMemory:   w.memoryLimit(it.Spec.Memory),
				RuntimeClass:  w.RuntimeClass,
				Pool: k8s.PoolConfig{
					Name:  it.BillingConfig.Pool,
					Share: it.BillingConfig.SharePool,
				},
				SA:         resourceID(it.ProjectID, it.Spec.WorkloadIdentityName),
				PullSecret: pullSecretResourceID(it.ProjectID, it.Spec.PullSecretName),
				Disk: k8s.Disk{
					Name:      resourceID(it.ProjectID, it.Spec.DiskName),
					MountPath: it.Spec.DiskMountPath,
					SubPath:   it.Spec.DiskSubPath,
				},
				BindConfigMap: bindData,
				Sidecars:      sidecarConfigs,
			}

			err = w.Client.CreateCronJob(ctx, cj)
			if err != nil {
				slog.Error("deployment: creating cronjob error", "id", it.ID, "error", err)
				return err
			}
		case api.DeploymentTypeTCPService:
			deploy := k8s.Deployment{
				ID:            id,
				ProjectID:     projectID,
				Name:          it.Name,
				Revision:      it.Revision,
				Image:         it.Spec.Image,
				Env:           it.Spec.Env,
				Command:       it.Spec.Command,
				Args:          it.Spec.Args,
				Replicas:      1,
				ExposePort:    it.Spec.Port,
				Annotations:   it.Spec.Annotations,
				RequestCPU:    w.normalizeRequestCPU(it.Spec.CPU),
				RequestMemory: it.Spec.Memory,
				LimitCPU:      w.cpuLimit(it.Spec.CPULimit),
				LimitMemory:   w.memoryLimit(it.Spec.Memory),
				RuntimeClass:  w.RuntimeClass,
				Pool: k8s.PoolConfig{
					Name:  it.BillingConfig.Pool,
					Share: it.BillingConfig.SharePool,
				},
				SA:         resourceID(it.ProjectID, it.Spec.WorkloadIdentityName),
				PullSecret: pullSecretResourceID(it.ProjectID, it.Spec.PullSecretName),
				Disk: k8s.Disk{
					Name:      resourceID(it.ProjectID, it.Spec.DiskName),
					MountPath: it.Spec.DiskMountPath,
					SubPath:   it.Spec.DiskSubPath,
				},
				BindConfigMap: bindData,
				Sidecars:      sidecarConfigs,
			}

			err = w.Client.CreateDeployment(ctx, deploy)
			if err != nil {
				slog.Error("deployment: creating deployment error", "id", it.ID, "error", err)
				return err
			}

			err = w.Client.CreateService(ctx, k8s.Service{
				ID:         id,
				ProjectID:  projectID,
				Port:       it.Spec.Port,
				Protocol:   string(it.Spec.Protocol),
				ExposeNode: true,
			})
			if err != nil {
				slog.Error("deployment: creating service error", "id", it.ID, "error", err)
				return err
			}

			time.Sleep(time.Second)

			nodePort, err := w.Client.GetNodePort(ctx, id)
			if err != nil {
				slog.Error("deployment: getting service node port error", "id", it.ID, "error", err)
				return err
			}

			result.NodePort = &nodePort
		case api.DeploymentTypeInternalTCPService:
			deploy := k8s.Deployment{
				ID:            id,
				ProjectID:     projectID,
				Name:          it.Name,
				Revision:      it.Revision,
				Image:         it.Spec.Image,
				Env:           it.Spec.Env,
				Command:       it.Spec.Command,
				Args:          it.Spec.Args,
				Replicas:      it.Spec.MinReplicas,
				ExposePort:    it.Spec.Port,
				Annotations:   it.Spec.Annotations,
				RequestCPU:    w.normalizeRequestCPU(it.Spec.CPU),
				RequestMemory: it.Spec.Memory,
				LimitCPU:      w.cpuLimit(it.Spec.CPULimit),
				LimitMemory:   w.memoryLimit(it.Spec.Memory),
				RuntimeClass:  w.RuntimeClass,
				Pool: k8s.PoolConfig{
					Name:  it.BillingConfig.Pool,
					Share: it.BillingConfig.SharePool,
				},
				SA:         resourceID(it.ProjectID, it.Spec.WorkloadIdentityName),
				PullSecret: pullSecretResourceID(it.ProjectID, it.Spec.PullSecretName),
				Disk: k8s.Disk{
					Name:      resourceID(it.ProjectID, it.Spec.DiskName),
					MountPath: it.Spec.DiskMountPath,
					SubPath:   it.Spec.DiskSubPath,
				},
				BindConfigMap: bindData,
				Sidecars:      sidecarConfigs,
			}

			err = w.Client.CreateDeployment(ctx, deploy)
			if err != nil {
				slog.Error("deployment: creating deployment error", "id", it.ID, "error", err)
				return err
			}

			err = w.Client.CreateService(ctx, k8s.Service{
				ID:        id,
				ProjectID: projectID,
				Port:      it.Spec.Port,
				Protocol:  string(it.Spec.Protocol),
			})
			if err != nil {
				slog.Error("deployment: creating service error", "id", it.ID, "error", err)
				return err
			}

			if it.Spec.MinReplicas != it.Spec.MaxReplicas {
				err = w.Client.CreateHorizontalPodAutoscaler(ctx, k8s.HorizontalPodAutoscaler{
					ID:            id,
					ProjectID:     projectID,
					MinReplicas:   it.Spec.MinReplicas,
					MaxReplicas:   it.Spec.MaxReplicas,
					TargetPercent: w.targetCPUPercent(it.Spec.CPU, it.Spec.CPULimit),
				})
				if err != nil {
					slog.Error("deployment: creating hpa error", "id", it.ID, "error", err)
					return err
				}
			} else {
				err = w.Client.DeleteHorizontalPodAutoscaler(ctx, id)
				if err != nil {
					slog.Error("deployment: deleting hpa error", "id", it.ID, "error", err)
					return err
				}
			}
		default:
			return fmt.Errorf("unknown type")
		}

		result.Success = true

		return nil
	}

	err := f()
	if isRetryable(err) {
		slog.Error("deployment: got retryable error", "id", it.ID, "error", err)
		return
	}
	if err != nil {
		slog.Error("deployment: error", "id", it.ID, "error", err)

		result.Success = false
		result.NodePort = nil
		w.results = append(w.results, &api.DeployerSetResultItem{
			DeploymentDeploy: &result,
		})
		return
	}

	slog.Info("deployment: deployed", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		DeploymentDeploy: &result,
	})
}

func (w *Worker) deploymentDelete(ctx context.Context, it *api.DeployerCommandDeploymentMetadata) {
	slog.Info("deployment: deleting", "id", it.ID)

	err := w.deploymentRemoveK8SResource(ctx, it)
	if err != nil {
		slog.Error("deployment: k8s remove resource error", "id", it.ID, "error", err)
		return
	}

	slog.Info("deployment: deleted", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		DeploymentDelete: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) deploymentPause(ctx context.Context, it *api.DeployerCommandDeploymentMetadata) {
	slog.Info("deployment: pausing", "id", it.ID)

	err := w.deploymentRemoveK8SResource(ctx, it)
	if err != nil {
		slog.Error("deployment: k8s remove resource error", "id", it.ID, "error", err)
		return
	}

	slog.Info("deployment: paused", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		DeploymentPause: &api.DeployerSetResultItemDeployment{
			ID:       it.ID,
			Revision: it.Revision,
		},
	})
}

func (w *Worker) deploymentCleanupResource(ctx context.Context, it *api.DeployerCommandDeploymentMetadata) {
	slog.Info("deployment: cleanup resource", "id", it.ID)

	err := w.deploymentRemoveK8SResource(ctx, it)
	if err != nil {
		slog.Error("deployment: k8s remove resource error", "id", it.ID, "error", err)
		return
	}

	slog.Info("deployment: cleanup resource", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		DeploymentCleanup: &api.DeployerSetResultItemDeployment{
			ID:       it.ID,
			Revision: it.Revision,
		},
	})
}

func (w *Worker) deploymentRemoveK8SResource(ctx context.Context, it *api.DeployerCommandDeploymentMetadata) error {
	slog.Info("deployment: removing k8s resource", "id", it.ID)

	id := resourceID(it.ProjectID, it.Name)

	var err error
	switch it.Type {
	case api.DeploymentTypeWebService:
		err = w.Client.DeleteDeployment(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting deployment error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteHorizontalPodAutoscaler(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting hpa error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteIngress(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting ingress error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteIngress(ctx, id+"-internal")
		if err != nil {
			slog.Error("deployment: deleting internal ingress error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteService(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting service error", "id", it.ID, "error", err)
			return err
		}
	case api.DeploymentTypeWorker:
		err = w.Client.DeleteDeployment(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting deployment error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteHorizontalPodAutoscaler(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting hpa error", "id", it.ID, "error", err)
			return err
		}
	case api.DeploymentTypeCronJob:
		err = w.Client.DeleteCronJob(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting cronjob error", "id", it.ID, "error", err)
			return err
		}
	case api.DeploymentTypeTCPService:
		err = w.Client.DeleteDeployment(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting deployment error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteService(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting service error", "id", it.ID, "error", err)
			return err
		}
	case api.DeploymentTypeInternalTCPService:
		err = w.Client.DeleteDeployment(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting deployment error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteHorizontalPodAutoscaler(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting hpa error", "id", it.ID, "error", err)
			return err
		}

		err = w.Client.DeleteService(ctx, id)
		if err != nil {
			slog.Error("deployment: deleting service error", "id", it.ID, "error", err)
			return err
		}
	default:
		return fmt.Errorf("unknown type")
	}

	err = w.Client.DeleteConfigMap(ctx, id)
	if err != nil {
		slog.Error("deployment: deleting config map error", "id", it.ID, "error", err)
		return err
	}

	return nil
}

func (w *Worker) diskCreate(ctx context.Context, it *api.DeployerCommandDiskCreate) {
	slog.Info("disk: creating", "id", it.ID)

	id := resourceID(it.ProjectID, it.Name)
	projectID := idString(it.ProjectID)

	err := w.Client.CreatePersistentVolumeClaim(ctx, k8s.PersistentVolumeClaim{
		ID:        id,
		ProjectID: projectID,
		Size:      it.Size,
		// StorageClass: "ssd",
	})
	if err != nil {
		slog.Error("disk: creating error", "id", it.ID, "error", err)
		return
	}

	slog.Info("disk: created", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		DiskCreate: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) diskDelete(ctx context.Context, it *api.DeployerCommandMetadata) {
	slog.Info("disk: deleting", "id", it.ID)

	id := resourceID(it.ProjectID, it.Name)

	err := w.Client.DeletePersistentVolumeClaim(ctx, id)
	if err != nil {
		slog.Error("disk: deleting error", "id", it.ID, "error", err)
		return
	}

	slog.Info("disk: deleted", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		DiskDelete: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) routeCreate(ctx context.Context, it *api.DeployerCommandRouteCreate) {
	slog.Info("route: creating", "id", it.ID)

	ingID := fmt.Sprintf("domain-%d", it.ID)
	domainID := normalizeDomain(it.Domain)

	projectID := idString(it.ProjectID)
	var secret string
	if w.Cert {
		secret = "tls-" + domainID
	}

	switch {
	default:
		ing := k8s.Ingress{
			ID:        ingID,
			Service:   resourceID(it.ProjectID, it.Target), // TODO: unsupport when remove non-prefix target
			ProjectID: projectID,
			Domain:    it.Domain,
			Path:      it.Path,
			Secret:    secret,
			Config:    it.Config,
		}
		switch {
		case strings.HasPrefix(it.Target, "deployment://"):
			ing.Service = resourceID(it.ProjectID, strings.TrimPrefix(it.Target, "deployment://"))
		case strings.HasPrefix(it.Target, "ipfs://"):
			ing.Service = "ipfs-gateway"
			ing.UpstreamHost = "ipfs-gateway"
			ing.UpstreamPath = "/ipfs/" + strings.TrimPrefix(it.Target, "ipfs://")
		case strings.HasPrefix(it.Target, "ipns://"):
			ing.Service = "ipfs-gateway"
			ing.UpstreamHost = "ipfs-gateway"
			ing.UpstreamPath = "/ipns/" + strings.TrimPrefix(it.Target, "ipns://")
		case strings.HasPrefix(it.Target, "dnslink://"):
			ing.Service = "ipfs-gateway"
		}

		err := w.Client.CreateIngress(ctx, ing)
		if err != nil {
			slog.Error("route: creating ingress error", "id", it.ID, "error", err)
			return
		}
	case strings.HasPrefix(it.Target, "redirect://"):
		target := strings.TrimPrefix(it.Target, "redirect://")

		err := w.Client.CreateRedirectIngress(ctx, k8s.RedirectIngress{
			ID:        ingID,
			ProjectID: projectID,
			Domain:    it.Domain,
			Path:      it.Path,
			Target:    target,
			Secret:    secret,
			Config:    it.Config,
		})
		if err != nil {
			slog.Error("route: creating redirect ingress error", "id", it.ID, "error", err)
			return
		}
	}

	if w.Cert {
		slog.Info("route: creating cert", "id", it.ID, "domain", it.Domain)
		err := w.Client.CreateCertificate(ctx, k8s.Certificate{
			ID:        domainID,
			ProjectID: projectID,
			Domain:    it.Domain,
		})
		if err != nil {
			slog.Error("route: creating certificate error", "id", it.ID, "error", err)
			return
		}
	} else {
		slog.Info("route: skip creating cert (disabled)", "id", it.ID, "domain", it.Domain)
	}

	slog.Info("route: created", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		RouteCreate: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) routeDelete(ctx context.Context, it *api.DeployerCommandRouteDelete) {
	slog.Info("route: deleting", "id", it.ID)

	ingID := fmt.Sprintf("domain-%d", it.ID)
	domainID := normalizeDomain(it.Domain)

	err := w.Client.DeleteIngress(ctx, ingID)
	if err != nil {
		slog.Error("route: deleting ingress error", "id", it.ID, "error", err)
		return
	}

	if w.Cert {
		active, err := w.Deployer.IsDomainActive(ctx, &api.DeployerIsDomainActive{Domain: it.Domain})
		if err != nil {
			slog.Error("route: check domain active error", "id", it.ID, "error", err)
			return
		}

		if !active {
			slog.Info("route: no more active domain, delete certificate", "id", it.ID, "domain", it.Domain)
			err = w.Client.DeleteCertificate(ctx, domainID)
			if err != nil {
				slog.Error("route: deleting certificate error", "id", it.ID, "error", err)
				return
			}
		} else {
			slog.Info("route: domain is still active, skip delete certificate", "domain", it.Domain)
		}
	}

	slog.Info("route: deleted", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		RouteDelete: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) workloadIdentityCreate(ctx context.Context, it *api.DeployerCommandWorkloadIdentityCreate) {
	slog.Info("workloadidentity: creating", "id", it.ID)

	id := resourceID(it.ProjectID, it.Name)
	projectID := idString(it.ProjectID)

	err := w.Client.CreateServiceAccount(ctx, k8s.ServiceAccount{
		ID:        id,
		ProjectID: projectID,
		GSA:       it.GSA,
	})
	if err != nil {
		slog.Error("workloadidentity: creating error", "id", it.ID, "error", err)
		return
	}

	slog.Info("workloadidentity: created", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		WorkloadIdentityCreate: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

func (w *Worker) workloadIdentityDelete(ctx context.Context, it *api.DeployerCommandMetadata) {
	slog.Info("workloadidentity: deleting", "id", it.ID)

	id := resourceID(it.ProjectID, it.Name)

	err := w.Client.DeleteServiceAccount(ctx, id)
	if err != nil {
		slog.Error("workloadidentity: deleting error", "id", it.ID, "error", err)
		return
	}

	slog.Info("workloadidentity: deleted", "id", it.ID)

	w.results = append(w.results, &api.DeployerSetResultItem{
		WorkloadIdentityDelete: &api.DeployerSetResultItemGeneral{
			ID: it.ID,
		},
	})
}

// func (w *Worker) database(ctx context.Context) {
// 	list, err := server.ListDatabases(ctx,
// 		server.Status(api.Pending),
// 		server.InLocation(w.Location.ID),
// 		server.OrderByCreatedAtAsc(),
// 	)
// 	if err != nil {
// 		logs.Errorf("database: can not list; %v", err)
// 		logs.Report(err)
// 		return
// 	}
//
// 	for _, it := range list {
// 		// processName := "database/" + it.ResourceID()
// 		// _, ok := w.processList.Load(processName)
// 		// if ok {
// 		// 	continue
// 		// }
// 		//
// 		// w.incrProcess()
// 		// w.processList.Store(processName, struct{}{})
//
// 		// it := it
//
// 		// go func() {
// 		// defer func() {
// 		// 	w.processList.Delete(processName)
// 		// 	w.decrProcess()
// 		// }()
//
// 		switch it.Action {
// 		case api.Create:
// 			w.databaseCreate(ctx, it)
// 		case api.Delete:
// 			w.databaseDelete(ctx, it)
// 		}
// 		// }()
// 	}
// }

// func (w *Worker) databaseCreate(ctx context.Context, it *server.Database) {
// 	logs.Infof("database: %d, creating...", it.ID)
//
// 	id := it.ResourceID()
// 	projectID := idString(it.ProjectID)
//
// 	f := func() error {
// 		status, err := server.GetDatabaseStatus(ctx, it.ID)
// 		if err != nil {
// 			return err
// 		}
// 		if status != api.Pending {
// 			return nil
// 		}
//
// 		switch it.DBName {
// 		case "redis", "redislabs/redisearch":
// 			d := redis.Deployer{Client: w.Client}
// 			err = d.Deploy(ctx, redis.Config{
// 				ID:         id,
// 				Name:       it.Name,
// 				ProjectID:  projectID,
// 				Image:      it.DBName + ":" + latestIfEmpty(it.DBVersion),
// 				Args:       it.Args,
// 				DBSize:     parseInt64(it.Config["db_size"]),
// 				Databases:  parseInt64(it.Config["databases"]),
// 				RequestCPU: normalizeRequestCPU(it.CPU),
// 				LimitCPU:   defaultLimitCPU,
// 				Password:   it.Config["password"],
// 			})
// 			if err != nil {
// 				return err
// 			}
// 		default:
// 			return fmt.Errorf("invalid db name")
// 		}
//
// 		err = server.SetDatabaseStatus(ctx, it.ID, api.Success)
// 		if err != nil {
// 			return err
// 		}
// 		err = server.StampDatabaseSuccess(ctx, it.ID)
// 		if err != nil {
// 			return err
// 		}
//
// 		return nil
// 	}
//
// 	err := f()
// 	if err != nil {
// 		if err := server.SetDatabaseStatus(ctx, it.ID, api.Error); err != nil {
// 			logs.Errorf("database: can not set error status for %d; %v", it.ID, err)
// 		}
// 		return
// 	}
//
// 	logs.Infof("database: %d, created...", it.ID)
// }
//
// func (w *Worker) databaseDelete(ctx context.Context, it *server.Database) {
// 	logs.Infof("database: %d, deleting...", it.ID)
//
// 	id := it.ResourceID()
//
// 	err := pgctx.RunInTx(ctx, func(ctx context.Context) error {
// 		status, err := server.GetDatabaseStatus(ctx, it.ID)
// 		if err != nil {
// 			return err
// 		}
// 		if status != api.Pending {
// 			return nil
// 		}
//
// 		err = server.RemoveDatabase(ctx, it.ID)
// 		if err != nil {
// 			return err
// 		}
//
// 		switch it.DBName {
// 		case "redis":
// 			d := redis.Deployer{Client: w.Client}
// 			err = d.Delete(ctx, id)
// 			if err != nil {
// 				return err
// 			}
// 		default:
// 			return fmt.Errorf("invalid db name")
// 		}
// 		return nil
// 	})
// 	if err != nil {
// 		if err := server.SetDatabaseStatus(ctx, it.ID, api.Error); err != nil {
// 			logs.Errorf("database: can not set error status for %d; %v", it.ID, err)
// 		}
// 		return
// 	}
//
// 	logs.Infof("database: %d, deleted", it.ID)
// }

func idString(id int64) string {
	return strconv.FormatInt(id, 10)
}

func parseFloat64(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseInt64(s string) int64 {
	i, _ := strconv.ParseInt(s, 10, 64)
	return i
}

// func targetCPUPercent(limit float64) int {
// 	request, _ := strconv.ParseFloat(requestCPU, 64)
// 	if request <= 0 {
// 		request = 0.01
// 	}
//
// 	// floor limit in-case of limit can be fraction
// 	limit = math.Floor(limit)
// 	if limit <= 0 {
// 		limit = 1
// 	}
//
// 	// return 80% of limit but relative to request
// 	return int(80 * limit / request)
// }

func latestIfEmpty(version string) string {
	if version == "" {
		return "latest"
	}
	return version
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	if knet.IsConnectionRefused(err) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if strings.Contains(err.Error(), "would exceed context deadline") {
		return true
	}

	return false
}

func normalizeDomain(domain string) string {
	domain = strings.ReplaceAll(domain, "-", "--")
	domain = strings.ReplaceAll(domain, ".", "-")
	domain = strings.ToLower(domain)
	return domain
}

func resourceID(projectID int64, name string) string {
	if projectID <= 0 || name == "" {
		return ""
	}
	return fmt.Sprintf("%s-%d", name, projectID)
}

func pullSecretResourceID(projectID int64, name string) string {
	if projectID <= 0 || name == "" {
		return ""
	}
	return fmt.Sprintf("pull-%s-%d", name, projectID)
}

func prepareMountData(mountData map[string]string, sidecarMountData []map[string]string) (configMapData map[string]string, bindData map[string]string) {
	type item struct {
		key  string
		path string
		data string
	}

	var list []item
	for path, data := range mountData {
		list = append(list, item{path: path, data: data})
	}
	for _, d := range sidecarMountData {
		for path, data := range d {
			list = append(list, item{path: path, data: data})
		}
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].path < list[j].path
	})

	for i := range list {
		list[i].key = fmt.Sprintf("file-%d", i)
	}

	configMapData = make(map[string]string)
	bindData = make(map[string]string)
	for _, t := range list {
		configMapData[t.key] = t.data
		bindData[t.key] = t.path
	}
	return
}

func sidecarMountData(sidecar []*api.SidecarConfig) []map[string]string {
	var rs []map[string]string
	for _, x := range sidecar {
		if len(x.MountData) == 0 {
			continue
		}
		rs = append(rs, x.MountData)
	}
	return rs
}
