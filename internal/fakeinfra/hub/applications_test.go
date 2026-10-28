package hub

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var testApplicationsGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// The shipped catalog Applications load, and the hub lists and gets them by
// namespace.
func TestShippedApplications(t *testing.T) {
	t.Parallel()
	fx := loadShipped(t, "default")
	fx.Access.ServiceAccount.Token = testToken
	th := startHub(t, fx)
	client, err := dynamic.NewForConfig(&rest.Config{Host: th.srv.URL, BearerToken: testToken})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	apps := client.Resource(testApplicationsGVR)
	list, err := apps.Namespace("openshift-gitops").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, app := range list.Items {
		names = append(names, app.GetName())
		if owners := app.GetOwnerReferences(); len(owners) != 1 || owners[0].Kind != "ApplicationSet" {
			t.Errorf("%s ownerReferences = %v, want its ApplicationSet", app.GetName(), owners)
		}
	}
	want := []string{
		"acm-models-catalog-ingress-site-ashburn-1",
		"acm-models-catalog-models-site-madrid-1",
		"acm-models-catalog-models-site-paris-1",
		"acm-models-catalog-subscriptions-site-ashburn-1",
	}
	if len(names) != len(want) {
		t.Fatalf("List() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("List() = %v, want %v", names, want)
		}
	}
	got, err := apps.Namespace("openshift-gitops").Get(context.Background(), want[2], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if resources, _, _ := unstructured.NestedSlice(got.Object, "status", "resources"); len(resources) != 2 {
		t.Fatalf("Get() status.resources = %v, want the NetworkPolicy and the LLMInferenceService", resources)
	}
	if empty, err := apps.Namespace("argocd").List(context.Background(), metav1.ListOptions{}); err != nil || len(empty.Items) != 0 {
		t.Fatalf("List(another namespace) = %v, %v; want empty", empty, err)
	}
	if _, err := apps.Namespace("openshift-gitops").Get(context.Background(), "nope", metav1.GetOptions{}); err == nil {
		t.Fatal("Get(an Application the fixtures don't have) succeeded")
	}
}

func TestApplicationFixturesRejectDuplicatesAndMissingNames(t *testing.T) {
	t.Parallel()
	app := func(namespace, name string) unstructured.Unstructured {
		a := unstructured.Unstructured{}
		a.SetNamespace(namespace)
		a.SetName(name)
		return a
	}
	for name, apps := range map[string][]unstructured.Unstructured{
		"duplicate":    {app("a", "x"), app("a", "x")},
		"no name":      {app("a", "")},
		"no namespace": {app("", "x")},
	} {
		if _, err := New(&Fixtures{Applications: apps}); err == nil {
			t.Errorf("%s: New() succeeded", name)
		}
	}
}
