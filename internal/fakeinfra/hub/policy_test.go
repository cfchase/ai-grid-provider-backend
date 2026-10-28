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

var testPoliciesGVR = schema.GroupVersionResource{Group: "policy.open-cluster-management.io", Version: "v1", Resource: "policies"}

// A fixture Policy is served as is by namespace and name.
func TestFixturePolicies(t *testing.T) {
	t.Parallel()
	fx := loadShipped(t, "default")
	fx.Access.ServiceAccount.Token = testToken
	fixture := unstructured.Unstructured{}
	fixture.SetAPIVersion("policy.open-cluster-management.io/v1")
	fixture.SetKind("Policy")
	fixture.SetNamespace("site-paris-1")
	fixture.SetName("ai-grid-policies.example")
	fx.Policies = []unstructured.Unstructured{fixture}
	th := startHub(t, fx)
	client, err := dynamic.NewForConfig(&rest.Config{Host: th.srv.URL, BearerToken: testToken})
	if err != nil {
		t.Fatalf("dynamic.NewForConfig: %v", err)
	}
	got, err := client.Resource(testPoliciesGVR).Namespace("site-paris-1").Get(context.Background(), "ai-grid-policies.example", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetResourceVersion() == "" {
		t.Fatal("Get() has no resourceVersion")
	}
	if _, err := client.Resource(testPoliciesGVR).Namespace("site-oslo-1").Get(context.Background(), "ai-grid-policies.example", metav1.GetOptions{}); err == nil {
		t.Fatal("Get(a Policy the fixtures don't have) succeeded")
	}
}

func TestPolicyFixturesRejectTheReadyPolicyAndDuplicates(t *testing.T) {
	t.Parallel()
	policy := func(namespace, name string) unstructured.Unstructured {
		p := unstructured.Unstructured{}
		p.SetNamespace(namespace)
		p.SetName(name)
		return p
	}
	for name, policies := range map[string][]unstructured.Unstructured{
		"ready":     {policy(ReadyPolicyNamespace, ReadyPolicyName)},
		"duplicate": {policy("a", "p"), policy("a", "p")},
		"no name":   {policy("a", "")},
	} {
		if _, err := New(&Fixtures{Policies: policies}); err == nil {
			t.Errorf("%s: New() succeeded", name)
		}
	}
}
