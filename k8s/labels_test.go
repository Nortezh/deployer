package k8s

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodLabels(t *testing.T) {
	base := map[string]string{"id": "web-1", "projectId": "1"}

	if got := podLabels(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("no extra: got %v", got)
	}

	got := podLabels(base, map[string]string{"id": "other", "seer.io/app_id": "web"})
	want := map[string]string{"id": "web-1", "projectId": "1", "seer.io/app_id": "web"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(base) != 2 {
		t.Fatalf("base was modified: %v", base)
	}
}

func TestDeploymentLabelsFollowPodLabels(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	client := &Client{client: clientset, namespace: "app"}

	base := map[string]string{"id": "web-1", "projectId": "1"}
	extra := map[string]string{"seer.io/app_id": "web", "seer.io/retention": "7d"}
	withExtra := podLabels(base, extra)

	apply := func(podLabels map[string]string) {
		t.Helper()
		err := client.CreateDeployment(ctx, Deployment{
			ID:            "web-1",
			ProjectID:     "1",
			Name:          "web",
			Image:         "nginx",
			Replicas:      1,
			RequestCPU:    "0.1",
			RequestMemory: "64Mi",
			PodLabels:     podLabels,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(want map[string]string) {
		t.Helper()
		d, err := clientset.AppsV1().Deployments("app").Get(ctx, "web-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d.Labels, want) {
			t.Fatalf("deployment labels: got %v, want %v", d.Labels, want)
		}
		if !reflect.DeepEqual(d.Spec.Template.Labels, want) {
			t.Fatalf("pod template labels: got %v, want %v", d.Spec.Template.Labels, want)
		}
		if !reflect.DeepEqual(d.Spec.Selector.MatchLabels, base) {
			t.Fatalf("selector changed: %v", d.Spec.Selector.MatchLabels)
		}
	}

	apply(extra)
	check(withExtra)
	apply(nil) // log history turned off
	check(base)
	apply(extra) // and on again
	check(withExtra)
}

func TestCronJobLabelsFollowPodLabels(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	client := &Client{client: clientset, namespace: "app"}

	base := map[string]string{"id": "job-1", "projectId": "1"}
	extra := map[string]string{"seer.io/app_id": "job", "seer.io/retention": "7d"}
	withExtra := podLabels(base, extra)

	apply := func(podLabels map[string]string) {
		t.Helper()
		err := client.CreateCronJob(ctx, CronJob{
			ID:            "job-1",
			ProjectID:     "1",
			Name:          "job",
			Image:         "busybox",
			Schedule:      "* * * * *",
			RequestCPU:    "0.1",
			RequestMemory: "64Mi",
			PodLabels:     podLabels,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(want map[string]string) {
		t.Helper()
		cj, err := clientset.BatchV1().CronJobs("app").Get(ctx, "job-1", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cj.Labels, want) {
			t.Fatalf("cronjob labels: got %v, want %v", cj.Labels, want)
		}
		if !reflect.DeepEqual(cj.Spec.JobTemplate.Labels, want) {
			t.Fatalf("job template labels: got %v, want %v", cj.Spec.JobTemplate.Labels, want)
		}
		if !reflect.DeepEqual(cj.Spec.JobTemplate.Spec.Template.Labels, want) {
			t.Fatalf("pod template labels: got %v, want %v", cj.Spec.JobTemplate.Spec.Template.Labels, want)
		}
	}

	apply(extra)
	check(withExtra)
	apply(nil) // log history turned off
	check(base)
	apply(extra) // and on again
	check(withExtra)
}
