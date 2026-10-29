package servers

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// appResource is one entry of an Application's status.resources. An empty
// health leaves health out, as Argo CD does for kinds it has no check for.
func appResource(kind, name, sync, health, message string) map[string]interface{} {
	out := map[string]interface{}{"kind": kind, "name": name, "namespace": "ns", "status": sync}
	if health != "" {
		out["health"] = map[string]interface{}{"status": health, "message": message}
	}
	return out
}

// application is the Application ApplicationSet appSet generates for
// cluster, in the default namespace, listing resources.
func application(appSet, cluster string, resources ...map[string]interface{}) *unstructured.Unstructured {
	return applicationNamed(appSet+"-"+cluster, appSet, resources...)
}

// applicationNamed is an Application called name, owned by ApplicationSet
// owner (none when empty).
func applicationNamed(name, owner string, resources ...map[string]interface{}) *unstructured.Unstructured {
	list := make([]interface{}, 0, len(resources))
	for _, r := range resources {
		list = append(list, r)
	}
	metadata := map[string]interface{}{"name": name, "namespace": DefaultCatalogAppNamespace}
	if owner != "" {
		metadata["ownerReferences"] = []interface{}{map[string]interface{}{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "ApplicationSet", "name": owner, "uid": "u-" + owner,
		}}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   metadata,
		"status":     map[string]interface{}{"resources": list},
	}}
}

// withSyncFailure marks an Application's last sync Failed with message.
// The failed sync was to the revision the Application wants, r2.
func withSyncFailure(app *unstructured.Unstructured, message string) *unstructured.Unstructured {
	return withSyncFailureAt(app, message, "r2")
}

// withSyncFailureAt marks a failed sync to revision tried, while the
// Application wants r2.
func withSyncFailureAt(app *unstructured.Unstructured, message, tried string) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(app.Object, map[string]interface{}{
		"phase": "Failed", "message": message, "syncResult": map[string]interface{}{"revision": tried},
	}, "status", "operationState")
	_ = unstructured.SetNestedField(app.Object, "r2", "status", "sync", "revision")
	return app
}

// withRetry marks a sync to r2 that failed and that Argo CD is retrying.
func withRetry(app *unstructured.Unstructured, message string, retries int64) *unstructured.Unstructured {
	_ = unstructured.SetNestedMap(app.Object, map[string]interface{}{
		"phase": "Running", "message": message, "retryCount": retries,
		"syncResult": map[string]interface{}{"revision": "r2"},
	}, "status", "operationState")
	_ = unstructured.SetNestedField(app.Object, "r2", "status", "sync", "revision")
	return app
}

// refusal is what an AppProject that refuses a kind or namespace leaves in
// operationState.message (demo1, 2026-10-28).
const refusal = "one or more synchronization tasks are not valid (retried 2 times). resource serving.kserve.io:LLMInferenceService is not permitted in project acm-models-catalog-models"

// pruning marks a resource as one Argo CD is about to prune.
func pruning(res map[string]interface{}) map[string]interface{} {
	res["requiresPruning"] = true
	return res
}

func newAppClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{applicationsResource: "ApplicationList", managedClustersResource: "ManagedClusterList"},
		objects...,
	)
}

func TestCatalogStatusRead(t *testing.T) {
	t.Parallel()
	const models = DefaultCatalogModelsAppSet
	for name, tc := range map[string]struct {
		app  *unstructured.Unstructured
		want map[string]ObjectReport
	}{
		"synced and healthy is ready": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "Synced", "Healthy", "LLMInferenceService is ready")),
			want: map[string]ObjectReport{"m": {State: StateReady, Message: "LLMInferenceService is ready"}},
		},
		"degraded is failed": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "Synced", "Degraded", "image pull failed")),
			want: map[string]ObjectReport{"m": {State: StateFailed, Message: "image pull failed"}},
		},
		"progressing is pending": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "Synced", "Progressing", "starting")),
			want: map[string]ObjectReport{"m": {State: StatePending, Message: "starting"}},
		},
		// The live object is the previous revision.
		"out of sync and healthy is pending": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Healthy", "")),
			want: map[string]ObjectReport{"m": {State: StatePending}},
		},
		"out of sync and degraded stays failed": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Degraded", "crash loop")),
			want: map[string]ObjectReport{"m": {State: StateFailed, Message: "crash loop"}},
		},
		"missing is pending": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Missing", "")),
			want: map[string]ObjectReport{"m": {State: StatePending}},
		},
		"no health is pending": {
			app:  application(models, "site-a", appResource(kindLLMInferenceService, "m", "Synced", "", "")),
			want: map[string]ObjectReport{"m": {State: StatePending}},
		},
		"out of sync after a failed sync is failed with the sync's message": {
			app: withSyncFailure(
				application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Missing", "")),
				"one or more objects failed to apply"),
			want: map[string]ObjectReport{"m": {State: StateFailed, Message: "one or more objects failed to apply"}},
		},
		// The failure was a sync to an older revision; the object changed since.
		"an old failed sync doesn't fail an object edited since": {
			app: withSyncFailureAt(
				application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Healthy", "")),
				"an older failure", "r1"),
			want: map[string]ObjectReport{"m": {State: StatePending}},
		},
		// While the models namespace moves, the old copy is OutOfSync and
		// requiresPruning; the new one decides.
		"a copy being pruned is left out": {
			app: application(models, "site-a",
				pruning(appResource(kindLLMInferenceService, "m", "OutOfSync", "Healthy", "")),
				appResource(kindLLMInferenceService, "m", "Synced", "Healthy", "")),
			want: map[string]ObjectReport{"m": {State: StateReady}},
		},
		"a repeated name keeps the worse report, in either order": {
			app: application(models, "site-a",
				appResource(kindLLMInferenceService, "m", "Synced", "Degraded", "old"),
				appResource(kindLLMInferenceService, "m", "Synced", "Healthy", ""),
				appResource(kindLLMInferenceService, "n", "Synced", "Healthy", ""),
				appResource(kindLLMInferenceService, "n", "Synced", "Progressing", "")),
			want: map[string]ObjectReport{"m": {State: StateFailed, Message: "old"}, "n": {State: StatePending}},
		},
		// An AppProject refusal: the resource is OutOfSync and Missing, the
		// Application Healthy, and the reason only in operationState.
		"a sync Argo CD is retrying is pending with its message": {
			app: withRetry(
				application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Missing", "")),
				refusal, 2),
			want: map[string]ObjectReport{"m": {State: StatePending, Message: refusal, Retrying: true}},
		},
		// Fixed in r3 while the retry of r2 still runs: the old reason goes.
		"a retry of an older revision is plain pending": {
			app: func() *unstructured.Unstructured {
				app := withRetry(
					application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Missing", "")),
					refusal, 3)
				_ = unstructured.SetNestedField(app.Object, "r3", "status", "sync", "revision")
				return app
			}(),
			want: map[string]ObjectReport{"m": {State: StatePending}},
		},
		"a first sync attempt still running is plain pending": {
			app: withRetry(
				application(models, "site-a", appResource(kindLLMInferenceService, "m", "OutOfSync", "Missing", "")),
				"syncing", 0),
			want: map[string]ObjectReport{"m": {State: StatePending}},
		},
		"a retried sync leaves synced objects alone": {
			app: withRetry(
				application(models, "site-a", appResource(kindLLMInferenceService, "m", "Synced", "Healthy", "")),
				refusal, 2),
			want: map[string]ObjectReport{"m": {State: StateReady}},
		},
		"a failed sync leaves synced objects alone": {
			app: withSyncFailure(
				application(models, "site-a", appResource(kindLLMInferenceService, "m", "Synced", "Healthy", "")),
				"another object failed"),
			want: map[string]ObjectReport{"m": {State: StateReady}},
		},
		"other kinds are left out": {
			app: application(models, "site-a",
				appResource("NetworkPolicy", "m-grid", "Synced", "", ""),
				appResource(kindLLMInferenceService, "m", "Synced", "Healthy", "")),
			want: map[string]ObjectReport{"m": {State: StateReady}},
		},
		"an Application with no resources lists nothing": {
			app:  application(models, "site-a"),
			want: map[string]ObjectReport{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := NewCatalogStatusReader(newAppClient(tc.app), CatalogApps{}).Read(context.Background(), ModelsSet, kindLLMInferenceService)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if want := (ObjectReports{"site-a": tc.want}); !reflect.DeepEqual(got, want) {
				t.Fatalf("Read() = %v, want %v", got, want)
			}
		})
	}
}

func TestCatalogStatusReadApplications(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	healthy := func(kind, name string) map[string]interface{} {
		return appResource(kind, name, "Synced", "Healthy", "")
	}
	t.Run("each set's own Applications, by owner", func(t *testing.T) {
		t.Parallel()
		client := newAppClient(
			application(DefaultCatalogModelsAppSet, "site-b", healthy(kindLLMInferenceService, "m")),
			application(DefaultCatalogIngressAppSet, "site-a", healthy(kindMaaSModelRef, "m")),
			// Named like site-x's models Application, but nobody's.
			applicationNamed(DefaultCatalogModelsAppSet+"-site-x", "", healthy(kindLLMInferenceService, "m")),
			// Owned by another ApplicationSet whose name extends the models set's.
			application(DefaultCatalogModelsAppSet+"-extra", "site-y", healthy(kindLLMInferenceService, "m")),
		)
		reader := NewCatalogStatusReader(client, CatalogApps{})
		got, err := reader.Read(ctx, ModelsSet, kindLLMInferenceService)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if want := (ObjectReports{"site-b": {"m": {State: StateReady}}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("models Read() = %v, want %v", got, want)
		}
		got, err = reader.Read(ctx, IngressSet, kindMaaSModelRef)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if want := (ObjectReports{"site-a": {"m": {State: StateReady}}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("ingress Read() = %v, want %v", got, want)
		}
	})
	t.Run("configured names and namespace", func(t *testing.T) {
		t.Parallel()
		app := application("my-subs", "edge", healthy(kindMaaSSubscription, "team-a"))
		app.SetNamespace("argocd")
		reader := NewCatalogStatusReader(newAppClient(app), CatalogApps{Namespace: "argocd", Subscriptions: "my-subs"})
		got, err := reader.Read(ctx, SubscriptionsSet, kindMaaSSubscription)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if want := (ObjectReports{"edge": {"team-a": {State: StateReady}}}); !reflect.DeepEqual(got, want) {
			t.Fatalf("Read() = %v, want %v", got, want)
		}
	})
	t.Run("a nil reader reports nothing", func(t *testing.T) {
		t.Parallel()
		var reader *CatalogStatusReader
		if got, err := reader.Read(ctx, ModelsSet, kindLLMInferenceService); got != nil || err != nil {
			t.Fatalf("Read() = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("a missing or forbidden list warns once", func(t *testing.T) {
		t.Parallel()
		client := newAppClient()
		client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(applicationsResource.GroupResource(), "", errors.New("no access"))
		})
		var logs strings.Builder
		reader := NewCatalogStatusReader(client, CatalogApps{}).SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
		for range 3 {
			_, _ = reader.Read(ctx, ModelsSet, kindLLMInferenceService)
		}
		if n := strings.Count(logs.String(), "can't list the catalog's Argo CD Applications"); n != 1 {
			t.Fatalf("warned %d times, want once:\n%s", n, logs.String())
		}
	})
	for name, apiErr := range map[string]error{
		"not found": apierrors.NewNotFound(applicationsResource.GroupResource(), ""),
		"forbidden": apierrors.NewForbidden(applicationsResource.GroupResource(), "", errors.New("no access")),
	} {
		t.Run(name+" reports nothing", func(t *testing.T) {
			t.Parallel()
			client := newAppClient()
			client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apiErr
			})
			if got, err := NewCatalogStatusReader(client, CatalogApps{}).Read(ctx, ModelsSet, kindLLMInferenceService); got != nil || err != nil {
				t.Fatalf("Read() = %v, %v; want nil, nil", got, err)
			}
		})
	}
	t.Run("other errors are returned", func(t *testing.T) {
		t.Parallel()
		client := newAppClient()
		client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewServiceUnavailable("down")
		})
		if _, err := NewCatalogStatusReader(client, CatalogApps{}).Read(ctx, ModelsSet, kindLLMInferenceService); err == nil {
			t.Fatal("Read() error = nil, want the API error")
		}
	})
}

func TestClusterSummary(t *testing.T) {
	t.Parallel()
	got := clusterSummary([]string{"site-c"}, []string{"site-a"}, []string{"site-b"}, []string{"edge"})
	if want := "Failed on site-c; not ready on site-a; ready on site-b; no report from edge"; got != want {
		t.Fatalf("clusterSummary() = %q, want %q", got, want)
	}
	if got := clusterSummary(nil, nil, nil, nil); got != "" {
		t.Fatalf("clusterSummary() = %q, want empty", got)
	}
}

// TestCatalogStatusDegradedUnderWay checks the Degraded health messages that
// are steps still under way, not failures. The catalog's health checks map
// every Ready=False to Degraded, and KServe and MaaS set Ready=False while a
// model starts or waits for a subscription.
func TestCatalogStatusDegradedUnderWay(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind, message string
		want          ObjectReport
	}{
		// KServe copies a not-yet-available Deployment's condition.
		"model starting": {
			kindLLMInferenceService, "Deployment does not have minimum availability.",
			ObjectReport{State: StatePending, Message: "Deployment does not have minimum availability."},
		},
		"model's HTTPRoute not observed yet": {
			kindLLMInferenceService, "The managed HTTPRoute has not been observed yet",
			ObjectReport{State: StatePending, Message: "The managed HTTPRoute has not been observed yet"},
		},
		"model's Gateways not ready": {
			kindLLMInferenceService, "The following Gateways are not ready: [gw]",
			ObjectReport{State: StatePending, Message: "The following Gateways are not ready: [gw]"},
		},
		"model's inference pool not ready": {
			kindLLMInferenceService, "The inference pool ns/pool is not ready",
			ObjectReport{State: StatePending, Message: "The inference pool ns/pool is not ready"},
		},
		"model's rollout progressing": {
			kindLLMInferenceService, "LWS is progressing",
			ObjectReport{State: StatePending, Message: "LWS is progressing"},
		},
		"model waiting for its Gateway": {
			kindLLMInferenceService, "Waiting for Gateway gw to be programmed",
			ObjectReport{State: StatePending, Message: "Waiting for Gateway gw to be programmed"},
		},
		"model's rollout in progress": {
			kindLLMInferenceService, "Deployment rollout in progress",
			ObjectReport{State: StatePending, Message: "Deployment rollout in progress"},
		},
		"model's routes not accepted yet": {
			kindLLMInferenceService, "The following HTTPRoutes exist but no Gateway controller has accepted them yet: [ns/r]",
			ObjectReport{State: StatePending, Message: "The following HTTPRoutes exist but no Gateway controller has accepted them yet: [ns/r]"},
		},
		"model's ScaledObject not ready yet": {
			kindLLMInferenceService, "ScaledObject is not yet ready",
			ObjectReport{State: StatePending, Message: "ScaledObject is not yet ready"},
		},
		// KServe router.go: a rejected route dumps its condition.
		"model's route rejected": {
			kindLLMInferenceService, `The following HTTPRoutes are not ready: [ns/r: Accepted=False (reason "NotAllowedByListeners", message "x")]`,
			ObjectReport{State: StateFailed, Message: `The following HTTPRoutes are not ready: [ns/r: Accepted=False (reason "NotAllowedByListeners", message "x")]`},
		},
		"model's route rejected while another waits": {
			kindLLMInferenceService, `The following HTTPRoutes are not ready: [ns/r: Accepted=False (reason "NotAllowedByListeners", message "x")]; still waiting for a Gateway to accept: [ns/q]`,
			ObjectReport{State: StateFailed, Message: `The following HTTPRoutes are not ready: [ns/r: Accepted=False (reason "NotAllowedByListeners", message "x")]; still waiting for a Gateway to accept: [ns/q]`},
		},
		"model's inference pool rejected": {
			kindLLMInferenceService, `ns/p: Accepted="False" (reason "NotSupportedByGateway", message "y")`,
			ObjectReport{State: StateFailed, Message: `ns/p: Accepted="False" (reason "NotSupportedByGateway", message "y")`},
		},
		"model with a bad config": {
			kindLLMInferenceService, "config llm-template not found",
			ObjectReport{State: StateFailed, Message: "config llm-template not found"},
		},
		"model stopped": {
			kindLLMInferenceService, "Service is stopped",
			ObjectReport{State: StateFailed, Message: "Service is stopped"},
		},
		// maas-controller deriveModelPhase: runtime ready, no pairing.
		"model ref awaiting a subscription": {
			kindMaaSModelRef, "Awaiting governance pairing",
			ObjectReport{State: StatePending, Message: "Awaiting governance pairing", NoSubscription: true},
		},
		"model ref awaiting a subscription and its backend": {
			kindMaaSModelRef, "Awaiting governance pairing and backend readiness",
			ObjectReport{State: StatePending, Message: "Awaiting governance pairing and backend readiness"},
		},
		"model ref governed, backend not ready": {
			kindMaaSModelRef, "Governed but backend is not ready",
			ObjectReport{State: StatePending, Message: "Governed but backend is not ready"},
		},
		"model ref failed": {
			kindMaaSModelRef, "Failed to update model status: boom",
			ObjectReport{State: StateFailed, Message: "Failed to update model status: boom"},
		},
		// Startup messages count only for the kind that writes them.
		"another kind's startup message": {
			kindMaaSModelRef, "Deployment does not have minimum availability.",
			ObjectReport{State: StateFailed, Message: "Deployment does not have minimum availability."},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			set := ModelsSet
			if tc.kind != kindLLMInferenceService {
				set = IngressSet
			}
			app := application(CatalogApps{}.withDefaults().name(set), "site-a", appResource(tc.kind, "m", "Synced", "Degraded", tc.message))
			got, err := NewCatalogStatusReader(newAppClient(app), CatalogApps{}).Read(context.Background(), set, tc.kind)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if want := (ObjectReports{"site-a": {"m": tc.want}}); !reflect.DeepEqual(got, want) {
				t.Fatalf("Read() = %v, want %v", got, want)
			}
		})
	}
}

// An out-of-sync MaaSModelRef that says it awaits a subscription is the
// previous revision: pending, not "published, no subscription".
func TestCatalogStatusNoSubscriptionNeedsSync(t *testing.T) {
	t.Parallel()
	app := application(DefaultCatalogIngressAppSet, "site-a", appResource(kindMaaSModelRef, "m", "OutOfSync", "Degraded", "Awaiting governance pairing"))
	got, err := NewCatalogStatusReader(newAppClient(app), CatalogApps{}).Read(context.Background(), IngressSet, kindMaaSModelRef)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	want := ObjectReports{"site-a": {"m": {State: StatePending, Message: "Awaiting governance pairing"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read() = %v, want %v", got, want)
	}
}
