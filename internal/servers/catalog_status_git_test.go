package servers

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"testing"

	"buf.build/go/protovalidate"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// llmisvc, modelRef and externalModel are a catalog resource for model
// granite in the given sync and health.
func llmisvc(sync, health, message string) map[string]interface{} {
	return appResource(kindLLMInferenceService, "granite", sync, health, message)
}

func modelRef(sync, health string) map[string]interface{} {
	return appResource(kindMaaSModelRef, "granite", sync, health, "")
}

func externalModel(sync, health string) map[string]interface{} {
	return appResource(kindExternalModel, "granite", sync, health, "")
}

// modelsApp, ingressApp and subscriptionsApp are a cluster's catalog
// Applications.
func modelsApp(cluster string, resources ...map[string]interface{}) runtime.Object {
	return application(DefaultCatalogModelsAppSet, cluster, resources...)
}

func ingressApp(cluster string, resources ...map[string]interface{}) runtime.Object {
	return application(DefaultCatalogIngressAppSet, cluster, resources...)
}

func subscriptionsApp(cluster string, resources ...map[string]interface{}) runtime.Object {
	return application(DefaultCatalogSubscriptionsAppSet, cluster, resources...)
}

// readyIngress is an ingress Application that reports granite published.
func readyIngress(cluster string) runtime.Object {
	return ingressApp(cluster, modelRef("Synced", "Healthy"), externalModel("Synced", "Healthy"))
}

// forbidApplications makes every Application list Forbidden.
func forbidApplications(client *fake.FakeDynamicClient) {
	client.PrependReactor("list", "applications", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(applicationsResource.GroupResource(), "", errors.New("no access"))
	})
}

// clusterStatus is a ModelClusterStatus a test expects.
func clusterStatus(cluster string, phase aigridv1.ModelPhase, reason, message string) *aigridv1.ModelClusterStatus {
	return &aigridv1.ModelClusterStatus{Cluster: cluster, Phase: phase, Reason: reason, Message: message}
}

const (
	phaseReady   = aigridv1.ModelPhase_MODEL_PHASE_READY
	phasePending = aigridv1.ModelPhase_MODEL_PHASE_PENDING
	phaseFailed  = aigridv1.ModelPhase_MODEL_PHASE_FAILED
	phasePartial = aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE

	llmReady    = "LLMInferenceService ready"
	ingressOK   = "MaaSModelRef ready; ExternalModel ready"
	combinedOK  = llmReady + "; " + ingressOK
	llmNotReady = "LLMInferenceService not ready"
)

// TestModelsGitStatus checks the phase, reason and message of a model placed
// on site-a and site-b, and its status on each of them (status.clusters),
// from the catalog Applications' reports. Every ModelClusterStatus must pass
// the buf.validate rules declared on it.
func TestModelsGitStatus(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		objects      []runtime.Object
		forbid       bool
		wantPhase    aigridv1.ModelPhase
		wantReason   string
		wantMessage  string
		wantClusters []*aigridv1.ModelClusterStatus
	}{
		"ready": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				readyIngress("hub"),
			},
			wantPhase: phaseReady, wantReason: "Ready", wantMessage: "Ready on hub, site-a, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", llmReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		// Only ingress clusters have an ingress Application (demo1: site-a);
		// an inference-only cluster isn't missing one.
		"ready on a combined cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				readyIngress("site-a"),
			},
			wantPhase: phaseReady, wantReason: "Ready", wantMessage: "Ready on site-a, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", combinedOK),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"not ready on one cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Progressing", "starting")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
			},
			// No ingress cluster: nothing shows the model published.
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Not ready on site-a; ready on site-b; no ingress cluster has reported the MaaSModelRef",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", llmNotReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"not ready on one cluster, ready on the ingress": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Progressing", "starting")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				readyIngress("hub"),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Not ready on site-a; ready on hub, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", llmNotReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"not ready on one cluster and on the ingress": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Progressing", "starting")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("hub", modelRef("Synced", "Progressing"), externalModel("Synced", "Healthy")),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Not ready on hub, site-a; ready on site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", llmNotReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"out of sync is not ready yet": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("OutOfSync", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				readyIngress("hub"),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Not ready on site-a; ready on hub, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", llmNotReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"failed on one cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Degraded", "image pull failed")),
				modelsApp("site-b", llmisvc("Synced", "Degraded", "image pull failed")),
				readyIngress("hub"),
			},
			wantPhase: phaseFailed, wantReason: "Failed",
			wantMessage: "Failed on site-a, site-b; ready on hub. LLMInferenceService on site-a: image pull failed; LLMInferenceService on site-b: image pull failed",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseFailed, "Failed", "LLMInferenceService failed"),
				clusterStatus("site-b", phaseFailed, "Failed", "LLMInferenceService failed"),
			},
		},
		// Ready somewhere outranks failed elsewhere (the console's order).
		"failed on one cluster, ready on the other": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Degraded", "image pull failed")),
				readyIngress("hub"),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable",
			wantMessage: "Failed on site-b; ready on hub, site-a. LLMInferenceService on site-b: image pull failed",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", llmReady),
				clusterStatus("site-b", phaseFailed, "Failed", "LLMInferenceService failed"),
			},
		},
		// An AppProject refuses the model on site-b: pending with the reason
		// while Argo CD retries, failed once the retries run out.
		"refused by the AppProject, retrying": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				withRetry(application(DefaultCatalogModelsAppSet, "site-b", llmisvc("OutOfSync", "Missing", "")), refusal, 2),
				readyIngress("site-a"),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable",
			wantMessage: "Not ready on site-b; ready on site-a. LLMInferenceService on site-b: " + refusal,
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", combinedOK),
				clusterStatus("site-b", phasePending, "NotReady", llmNotReady),
			},
		},
		"refused by the AppProject, retries exhausted": {
			objects: []runtime.Object{
				withSyncFailure(application(DefaultCatalogModelsAppSet, "site-a", llmisvc("OutOfSync", "Missing", "")), refusal),
				withSyncFailure(application(DefaultCatalogModelsAppSet, "site-b", llmisvc("OutOfSync", "Missing", "")), refusal),
			},
			wantPhase: phaseFailed, wantReason: "Failed",
			wantMessage: "Failed on site-a, site-b; no ingress cluster has reported the MaaSModelRef. LLMInferenceService on site-a: " + refusal + "; LLMInferenceService on site-b: " + refusal,
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseFailed, "Failed", "LLMInferenceService failed"),
				clusterStatus("site-b", phaseFailed, "Failed", "LLMInferenceService failed"),
			},
		},
		"MaaSModelRef not ready on a combined cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("site-a", modelRef("Synced", "Progressing"), externalModel("Synced", "Healthy")),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Not ready on site-a; ready on site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NotReady", llmReady+"; MaaSModelRef not ready; ExternalModel ready"),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"a placed cluster has no Application yet": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Ready on site-a; no report from site-b; no ingress cluster has reported the MaaSModelRef",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", llmReady),
				clusterStatus("site-b", phasePending, "NoReport", "No LLMInferenceService report"),
			},
		},
		// Just written: the Applications haven't picked it up.
		"no Application lists the model yet": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b"),
				readyIngress("hub"),
			},
			wantPhase: phasePartial, wantReason: "PartiallyAvailable", wantMessage: "Ready on hub, site-a; no report from site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", llmReady),
				clusterStatus("site-b", phasePending, "NoReport", "No LLMInferenceService report"),
			},
		},
		"MaaSModelRef not listed on a combined cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("site-a"),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Ready on site-b; no report from site-a",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phasePending, "NoReport", llmReady+"; no MaaSModelRef report; no ExternalModel report"),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"the ExternalModel is missing on an ingress cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("hub", modelRef("Synced", "Healthy")),
			},
			wantPhase: phasePending, wantReason: "NotReady", wantMessage: "Ready on site-a, site-b; no report from hub",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", llmReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		// A cluster the model no longer is placed on, whose copy Argo CD is
		// still pruning, doesn't count.
		"a cluster the model left": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-c", llmisvc("OutOfSync", "Degraded", "being deleted")),
				readyIngress("hub"),
			},
			wantPhase: phaseReady, wantReason: "Ready", wantMessage: "Ready on hub, site-a, site-b",
			wantClusters: []*aigridv1.ModelClusterStatus{
				clusterStatus("site-a", phaseReady, "Ready", llmReady),
				clusterStatus("site-b", phaseReady, "Ready", llmReady),
			},
		},
		"no reports": {
			objects:    nil,
			wantReason: "SavedToGit", wantMessage: "Saved to Git; no cluster has reported the model's status",
		},
		"forbidden": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
			},
			forbid:     true,
			wantReason: "SavedToGit", wantMessage: "Saved to Git; no cluster has reported the model's status",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			client := newAppClient(tc.objects...)
			if tc.forbid {
				forbidApplications(client)
			}
			srv, err := NewModelsServer().SetWriter(gitops.NewMemoryWriter(nil)).
				SetCatalogStatus(NewCatalogStatusReader(client, CatalogApps{})).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("granite", "site-a", "site-b")})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "granite"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			for op, model := range map[string]*aigridv1.Model{"Create": created.GetObject(), "Get": got.GetObject(), "List": list.GetItems()[0]} {
				st := model.GetStatus()
				if st.GetPhase() != tc.wantPhase || st.GetReason() != tc.wantReason || st.GetMessage() != tc.wantMessage {
					t.Errorf("%s status = %v, %q, %q; want %v, %q, %q", op, st.GetPhase(), st.GetReason(), st.GetMessage(), tc.wantPhase, tc.wantReason, tc.wantMessage)
				}
				if st.GetPlacement().GetNumberOfSelectedClusters() != 2 {
					t.Errorf("%s placement = %v, want site-a and site-b", op, st.GetPlacement())
				}
				// The deployment fields are TestModelsGitTwoStatuses's.
				if !slices.EqualFunc(st.GetClusters(), tc.wantClusters, func(a, b *aigridv1.ModelClusterStatus) bool {
					return proto.Equal(clusterStatus(a.GetCluster(), a.GetPhase(), a.GetReason(), a.GetMessage()), b)
				}) {
					t.Errorf("%s status.clusters = %v; want %v", op, st.GetClusters(), tc.wantClusters)
				}
				for _, cluster := range st.GetClusters() {
					if err := protovalidate.Validate(cluster); err != nil {
						t.Errorf("%s status.clusters[%s] fails its buf.validate rules: %v", op, cluster.GetCluster(), err)
					}
				}
			}
		})
	}
}

// TestModelClusterStatusValidationRules checks the buf.validate rules on
// ModelClusterStatus refuse what the server never writes: a cluster that is
// not a DNS label, an UNSPECIFIED, PARTIALLY_AVAILABLE or undefined phase,
// and an empty reason.
func TestModelClusterStatusValidationRules(t *testing.T) {
	t.Parallel()
	valid := clusterStatus("site-a", phaseReady, "Ready", llmReady)
	if err := protovalidate.Validate(valid); err != nil {
		t.Fatalf("protovalidate(%v) error = %v, want none", valid, err)
	}
	for name, mutate := range map[string]func(*aigridv1.ModelClusterStatus){
		"empty cluster":       func(c *aigridv1.ModelClusterStatus) { c.Cluster = "" },
		"cluster not a label": func(c *aigridv1.ModelClusterStatus) { c.Cluster = "Site_A" },
		"unspecified phase":   func(c *aigridv1.ModelClusterStatus) { c.Phase = aigridv1.ModelPhase_MODEL_PHASE_UNSPECIFIED },
		"partial phase":       func(c *aigridv1.ModelClusterStatus) { c.Phase = phasePartial },
		"undefined phase":     func(c *aigridv1.ModelClusterStatus) { c.Phase = 99 },
		"empty reason":        func(c *aigridv1.ModelClusterStatus) { c.Reason = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := proto.Clone(valid).(*aigridv1.ModelClusterStatus)
			mutate(c)
			var verr *protovalidate.ValidationError
			if err := protovalidate.Validate(c); !errors.As(err, &verr) {
				t.Fatalf("protovalidate(%v) error = %v, want a ValidationError", c, err)
			}
		})
	}
}

func TestSubscriptionsGitStatus(t *testing.T) {
	t.Parallel()
	sub := func(sync, health string) map[string]interface{} {
		return appResource(kindMaaSSubscription, "team-a", sync, health, "")
	}
	policy := func(sync, health string) map[string]interface{} {
		return appResource(kindMaaSAuthPolicy, "team-a-policy", sync, health, "")
	}
	ref := func(sync, health string) map[string]interface{} {
		return appResource(kindMaaSModelRef, "sim-model", sync, health, "")
	}
	for name, tc := range map[string]struct {
		objects           []runtime.Object
		forbid            bool
		wantPhase         string
		wantPolicyPhase   string
		wantPolicyMessage string
		wantModelPhase    string
		wantReason        string
	}{
		"active": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				subscriptionsApp("site-b", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				ingressApp("site-a", ref("Synced", "Healthy")),
				ingressApp("site-b", ref("Synced", "Healthy")),
			},
			wantPhase: "Active", wantPolicyPhase: "Active", wantPolicyMessage: "Ready on site-a, site-b",
			wantModelPhase: "Ready", wantReason: "Ready",
		},
		"pending on one cluster": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Progressing"), policy("OutOfSync", "Healthy")),
				subscriptionsApp("site-b", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				ingressApp("site-a", ref("Synced", "Progressing")),
				ingressApp("site-b", ref("Synced", "Healthy")),
			},
			wantPhase: "Pending", wantPolicyPhase: "Pending", wantPolicyMessage: "Not ready on site-a; ready on site-b",
			wantModelPhase: "Pending", wantReason: "NotReady",
		},
		"degraded on one cluster": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Degraded"), policy("Synced", "Degraded")),
				subscriptionsApp("site-b", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				ingressApp("site-a", ref("Synced", "Degraded")),
				ingressApp("site-b", ref("Synced", "Healthy")),
			},
			wantPhase: "Degraded", wantPolicyPhase: "Degraded", wantPolicyMessage: "Failed on site-a; ready on site-b",
			wantModelPhase: "Failed", wantReason: "Failed",
		},
		"refused by the AppProject": {
			objects: []runtime.Object{
				withRetry(application(DefaultCatalogSubscriptionsAppSet, "site-a", sub("OutOfSync", "Missing"), policy("OutOfSync", "Missing")), refusal, 1),
				ingressApp("site-a", ref("Synced", "Healthy")),
			},
			wantPhase: "Pending", wantPolicyPhase: "Pending", wantPolicyMessage: "Not ready on site-a",
			wantModelPhase: "Ready", wantReason: "Ready",
		},
		"refused by the AppProject, retries exhausted": {
			objects: []runtime.Object{
				withSyncFailure(application(DefaultCatalogSubscriptionsAppSet, "site-a", sub("OutOfSync", "Missing"), policy("OutOfSync", "Missing")), refusal),
				ingressApp("site-a", ref("Synced", "Healthy")),
			},
			wantPhase: "Degraded", wantPolicyPhase: "Degraded", wantPolicyMessage: "Failed on site-a",
			wantModelPhase: "Ready", wantReason: "Ready",
		},
		// MaaS hasn't paired the model with this subscription yet: pending,
		// not failed.
		"model awaiting its pairing": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				ingressApp("site-a", appResource(kindMaaSModelRef, "sim-model", "Synced", "Degraded", "Awaiting governance pairing")),
			},
			wantPhase: "Active", wantPolicyPhase: "Active", wantPolicyMessage: "Ready on site-a",
			wantModelPhase: "Pending", wantReason: "NotReady",
		},
		// A cluster that doesn't list the objects yet keeps them from Active
		// and Ready.
		"missing on one cluster": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				subscriptionsApp("site-b"),
				ingressApp("site-a", ref("Synced", "Healthy")),
				ingressApp("site-b"),
			},
			wantPhase: "Pending", wantPolicyPhase: "Pending", wantPolicyMessage: "Ready on site-a; no report from site-b",
			wantModelPhase: "Pending", wantReason: "NotReady",
		},
		// Subscriptions go to the clusters with a subscriptions Application;
		// an inference-only cluster has no say in their status.
		"inference-only cluster": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
				ingressApp("site-a", ref("Synced", "Healthy")),
				modelsApp("site-b"),
			},
			wantPhase: "Active", wantPolicyPhase: "Active", wantPolicyMessage: "Ready on site-a",
			wantModelPhase: "Ready", wantReason: "Ready",
		},
		"no reports": {
			objects:    nil,
			wantReason: "SavedToGit",
		},
		"forbidden": {
			objects: []runtime.Object{
				subscriptionsApp("site-a", sub("Synced", "Healthy"), policy("Synced", "Healthy")),
			},
			forbid:     true,
			wantReason: "SavedToGit",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			client := newAppClient(tc.objects...)
			if tc.forbid {
				forbidApplications(client)
			}
			srv, err := NewSubscriptionsServer().SetWriter(gitops.NewMemoryWriter(gitModelSeed("sim-model"))).
				SetCatalogStatus(NewCatalogStatusReader(client, CatalogApps{})).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			created, err := srv.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := srv.Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			list, err := srv.List(ctx, &aigridv1.SubscriptionsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			for op, sub := range map[string]*aigridv1.Subscription{"Create": created.GetObject(), "Get": got.GetObject(), "List": list.GetItems()[0]} {
				if phase := sub.GetStatus().GetPhase(); phase != tc.wantPhase {
					t.Errorf("%s status.phase = %q, want %q", op, phase, tc.wantPhase)
				}
			}
			if len(got.GetAuthPolicies()) != 1 {
				t.Fatalf("Get auth_policies = %v, want team-a-policy", got.GetAuthPolicies())
			}
			for op, policy := range map[string]*aigridv1.AuthPolicy{"Create": created.GetAuthPolicy(), "Get": got.GetAuthPolicies()[0]} {
				checkAuthPolicyStatus(t, op, policy, "team-a-policy", tc.wantPolicyPhase, tc.wantPolicyMessage)
			}
			regs := got.GetModelRegistrations()
			if len(regs) != 1 || regs[0].GetName() != "sim-model" || regs[0].GetPhase() != tc.wantModelPhase || regs[0].GetConditions()[0].GetReason() != tc.wantReason {
				t.Fatalf("Get model_registrations = %v, want phase %q, reason %q", regs, tc.wantModelPhase, tc.wantReason)
			}
		})
	}
}

// checkAuthPolicyStatus checks an AuthPolicy's phase and its Ready
// condition: none without a phase, True with Active, False with reason
// Failed with Degraded, and False with reason NotReady with Pending.
func checkAuthPolicyStatus(t *testing.T, op string, policy *aigridv1.AuthPolicy, wantID, wantPhase, wantMessage string) {
	t.Helper()
	if policy.GetId() != wantID || policy.GetPhase() != wantPhase {
		t.Errorf("%s auth policy %q phase = %q, want %q phase %q", op, policy.GetId(), policy.GetPhase(), wantID, wantPhase)
	}
	conditions := policy.GetConditions()
	if wantPhase == "" {
		if len(conditions) != 0 {
			t.Errorf("%s auth policy conditions = %v, want none", op, conditions)
		}
		return
	}
	wantStatus, wantReason := aigridv1.ConditionStatus_CONDITION_STATUS_TRUE, "Ready"
	switch wantPhase {
	case "Degraded":
		wantStatus, wantReason = aigridv1.ConditionStatus_CONDITION_STATUS_FALSE, "Failed"
	case "Pending":
		wantStatus, wantReason = aigridv1.ConditionStatus_CONDITION_STATUS_FALSE, "NotReady"
	}
	if len(conditions) != 1 || conditions[0].GetType() != "Ready" || conditions[0].GetStatus() != wantStatus ||
		conditions[0].GetReason() != wantReason || conditions[0].GetMessage() != wantMessage {
		t.Errorf("%s auth policy conditions = %v, want Ready %v, reason %q, message %q", op, conditions, wantStatus, wantReason, wantMessage)
	}
}

// TestModelsGitStatusIngressClusters checks that READY and
// PARTIALLY_AVAILABLE need ingress evidence. With no ingress report and no
// cluster labelled as ingress, a model ready on every placed cluster is
// PENDING, not READY or PARTIALLY_AVAILABLE. A cluster labelled
// ai-grid/enabled "true" and ai-grid/role ingress or combined must report
// the MaaSModelRef even when it has no ingress Application (Argo CD hasn't
// reached it yet, or the Applications can't be read).
func TestModelsGitStatusIngressClusters(t *testing.T) {
	t.Parallel()
	ingress := map[string]string{"ai-grid/enabled": "true", "ai-grid/role": "ingress"}
	modelsReady := []runtime.Object{
		modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
		modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
	}
	partial := []runtime.Object{
		modelsApp("site-a", llmisvc("Synced", "Progressing", "starting")),
		modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
	}
	for name, tc := range map[string]struct {
		objects     []runtime.Object
		listFails   bool
		wantPhase   aigridv1.ModelPhase
		wantMessage string
		wantSiteA   string
	}{
		"no ingress at all": {
			objects:   modelsReady,
			wantPhase: phasePending, wantMessage: "Ready on site-a, site-b; no ingress cluster has reported the MaaSModelRef",
		},
		"labelled ingress, not reported": {
			objects:   append(slices.Clone(modelsReady), managedCluster("hub", ingress, true)),
			wantPhase: phasePending, wantMessage: "Ready on site-a, site-b; no report from hub",
		},
		"labelled ingress, reported ready": {
			objects:   append(slices.Clone(modelsReady), readyIngress("hub"), managedCluster("hub", ingress, true)),
			wantPhase: phaseReady, wantMessage: "Ready on hub, site-a, site-b",
		},
		"partial, labelled ingress reported ready": {
			objects:   append(slices.Clone(partial), readyIngress("hub"), managedCluster("hub", ingress, true)),
			wantPhase: phasePartial, wantMessage: "Not ready on site-a; ready on hub, site-b",
			wantSiteA: llmNotReady,
		},
		"partial, labelled ingress not reported": {
			objects:   append(slices.Clone(partial), managedCluster("hub", ingress, true)),
			wantPhase: phasePending, wantMessage: "Not ready on site-a; ready on site-b; no report from hub",
			wantSiteA: llmNotReady,
		},
		"labelled combined cluster, not reported": {
			objects: append(slices.Clone(modelsReady), readyIngress("hub"),
				managedCluster("hub", ingress, true),
				managedCluster("site-a", map[string]string{"ai-grid/enabled": "true", "ai-grid/role": "combined"}, true)),
			wantPhase: phasePending, wantMessage: "Ready on hub, site-b; no report from site-a",
			wantSiteA: llmReady + "; no MaaSModelRef report; no ExternalModel report",
		},
		"ingress label on a cluster not in the grid": {
			objects: append(slices.Clone(modelsReady),
				managedCluster("hub", map[string]string{"ai-grid/role": "ingress"}, true)),
			wantPhase: phasePending, wantMessage: "Ready on site-a, site-b; no ingress cluster has reported the MaaSModelRef",
		},
		"cluster list fails": {
			objects:   append(slices.Clone(modelsReady), readyIngress("hub"), managedCluster("edge", ingress, true)),
			listFails: true,
			wantPhase: phaseReady, wantMessage: "Ready on hub, site-a, site-b",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			// The placed clusters must exist (checkPlacementClusters); a case
			// may give site-a its own labels.
			objects := slices.Clone(tc.objects)
			for _, name := range []string{"site-a", "site-b"} {
				if !slices.ContainsFunc(objects, func(o runtime.Object) bool {
					u, ok := o.(*unstructured.Unstructured)
					return ok && u.GetKind() == "ManagedCluster" && u.GetName() == name
				}) {
					objects = append(objects, managedCluster(name, nil, true))
				}
			}
			client := newAppClient(objects...)
			srv, err := NewModelsServer().SetLogger(slog.New(slog.DiscardHandler)).SetWriter(gitops.NewMemoryWriter(nil)).
				SetCatalogStatus(NewCatalogStatusReader(client, CatalogApps{})).SetDynamicClient(client).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("granite", "site-a", "site-b")}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if tc.listFails {
				client.PrependReactor("list", "managedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("hub unavailable")
				})
			}
			got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "granite"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			st := got.GetObject().GetStatus()
			if st.GetPhase() != tc.wantPhase || st.GetMessage() != tc.wantMessage {
				t.Errorf("status = %v, %q; want %v, %q", st.GetPhase(), st.GetMessage(), tc.wantPhase, tc.wantMessage)
			}
			wantSiteA := tc.wantSiteA
			if wantSiteA == "" {
				wantSiteA = llmReady
			}
			if len(st.GetClusters()) != 2 || st.GetClusters()[0].GetMessage() != wantSiteA {
				t.Errorf("status.clusters = %v; want site-a %q", st.GetClusters(), wantSiteA)
			}
		})
	}
}

// TestModelsGitTwoStatuses checks a model's deployment status on each placed
// cluster and its MaaS status on each ingress cluster, kept apart: a model
// that runs but has no subscription is READY with MaaS NO_SUBSCRIPTION, not
// PENDING (ai-grid-1 dry run, 2026-10-29).
func TestModelsGitTwoStatuses(t *testing.T) {
	t.Parallel()
	const (
		running  = aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_RUNNING
		starting = aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_STARTING
		depFail  = aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_FAILED
		depNone  = aigridv1.ModelDeploymentState_MODEL_DEPLOYMENT_STATE_UNSPECIFIED

		published = aigridv1.ModelMaasState_MODEL_MAAS_STATE_PUBLISHED
		noSub     = aigridv1.ModelMaasState_MODEL_MAAS_STATE_NO_SUBSCRIPTION
		notReady  = aigridv1.ModelMaasState_MODEL_MAAS_STATE_NOT_READY
		maasFail  = aigridv1.ModelMaasState_MODEL_MAAS_STATE_FAILED
		maasNone  = aigridv1.ModelMaasState_MODEL_MAAS_STATE_UNSPECIFIED

		awaiting = "Awaiting governance pairing"
		minAvail = "Deployment does not have minimum availability."
	)
	ref := func(health, message string) map[string]interface{} {
		return appResource(kindMaaSModelRef, "granite", "Synced", health, message)
	}
	type dep struct {
		state   aigridv1.ModelDeploymentState
		message string
	}
	ingress := func(cluster string, state aigridv1.ModelMaasState, message string) *aigridv1.ModelIngressStatus {
		return &aigridv1.ModelIngressStatus{Cluster: cluster, State: state, Message: message}
	}
	for name, tc := range map[string]struct {
		objects        []runtime.Object
		labelled       []string
		wantPhase      aigridv1.ModelPhase
		wantDeployment aigridv1.ModelDeploymentState
		wantMaaS       aigridv1.ModelMaasState
		wantPlaced     map[string]dep
		wantIngress    []*aigridv1.ModelIngressStatus
	}{
		"running, no subscription": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "LLMInferenceService is ready")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "LLMInferenceService is ready")),
				ingressApp("hub", ref("Degraded", awaiting), externalModel("Synced", "Healthy")),
			},
			wantPhase: phaseReady, wantDeployment: running, wantMaaS: noSub,
			wantPlaced:  map[string]dep{"site-a": {running, ""}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("hub", noSub, awaiting)},
		},
		"running and published": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("hub", ref("Healthy", "Governed and runtime-healthy"), externalModel("Synced", "Healthy")),
			},
			wantPhase: phaseReady, wantDeployment: running, wantMaaS: published,
			wantPlaced:  map[string]dep{"site-a": {running, ""}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("hub", published, "")},
		},
		"starting on one cluster": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Degraded", minAvail)),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("hub", ref("Degraded", "Awaiting governance pairing and backend readiness"), externalModel("Synced", "Healthy")),
			},
			wantPhase: phasePending, wantDeployment: starting, wantMaaS: notReady,
			wantPlaced:  map[string]dep{"site-a": {starting, minAvail}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("hub", notReady, "Awaiting governance pairing and backend readiness")},
		},
		"failed on one cluster, no subscription": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Degraded", "config llm-template not found")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("hub", ref("Degraded", awaiting), externalModel("Synced", "Healthy")),
			},
			wantPhase: phasePartial, wantDeployment: depFail, wantMaaS: noSub,
			wantPlaced:  map[string]dep{"site-a": {depFail, "config llm-template not found"}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("hub", noSub, awaiting)},
		},
		"the ExternalModel failed": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("hub", ref("Degraded", awaiting), appResource(kindExternalModel, "granite", "Synced", "Degraded", "provider grid-provider not found")),
			},
			wantPhase: phaseFailed, wantDeployment: running, wantMaaS: maasFail,
			wantPlaced:  map[string]dep{"site-a": {running, ""}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("hub", maasFail, "provider grid-provider not found")},
		},
		"a combined cluster has both": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				ingressApp("site-a", ref("Degraded", awaiting), externalModel("Synced", "Healthy")),
			},
			wantPhase: phaseReady, wantDeployment: running, wantMaaS: noSub,
			wantPlaced:  map[string]dep{"site-a": {running, ""}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("site-a", noSub, awaiting)},
		},
		"a placed cluster hasn't reported": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				readyIngress("hub"),
			},
			wantPhase: phasePartial, wantDeployment: starting, wantMaaS: published,
			wantPlaced:  map[string]dep{"site-a": {running, ""}, "site-b": {depNone, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("hub", published, "")},
		},
		"a labelled ingress cluster hasn't reported": {
			objects: []runtime.Object{
				modelsApp("site-a", llmisvc("Synced", "Healthy", "")),
				modelsApp("site-b", llmisvc("Synced", "Healthy", "")),
				readyIngress("hub"),
			},
			labelled:  []string{"hub", "edge"},
			wantPhase: phasePending, wantDeployment: running, wantMaaS: notReady,
			wantPlaced:  map[string]dep{"site-a": {running, ""}, "site-b": {running, ""}},
			wantIngress: []*aigridv1.ModelIngressStatus{ingress("edge", maasNone, ""), ingress("hub", published, "")},
		},
		"no reports": {
			wantDeployment: depNone, wantMaaS: maasNone,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			objects := slices.Clone(tc.objects)
			objects = append(objects, managedCluster("site-a", nil, true), managedCluster("site-b", nil, true))
			for _, cluster := range tc.labelled {
				objects = append(objects, managedCluster(cluster, map[string]string{"ai-grid/enabled": "true", "ai-grid/role": "ingress"}, true))
			}
			client := newAppClient(objects...)
			srv, err := NewModelsServer().SetLogger(slog.New(slog.DiscardHandler)).SetWriter(gitops.NewMemoryWriter(nil)).
				SetCatalogStatus(NewCatalogStatusReader(client, CatalogApps{})).SetDynamicClient(client).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("granite", "site-a", "site-b")}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "granite"})
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			for op, model := range map[string]*aigridv1.Model{"Get": got.GetObject(), "List": list.GetItems()[0]} {
				st := model.GetStatus()
				if st.GetPhase() != tc.wantPhase || st.GetDeployment() != tc.wantDeployment || st.GetMaas() != tc.wantMaaS {
					t.Errorf("%s phase, deployment, maas = %v, %v, %v; want %v, %v, %v (%q)", op,
						st.GetPhase(), st.GetDeployment(), st.GetMaas(), tc.wantPhase, tc.wantDeployment, tc.wantMaaS, st.GetMessage())
				}
				placed := map[string]dep{}
				for _, c := range st.GetClusters() {
					placed[c.GetCluster()] = dep{c.GetDeployment(), c.GetDeploymentMessage()}
					if err := protovalidate.Validate(c); err != nil {
						t.Errorf("%s clusters[%s] fails its rules: %v", op, c.GetCluster(), err)
					}
				}
				if len(tc.wantPlaced) > 0 || len(placed) > 0 {
					if !maps.Equal(placed, tc.wantPlaced) {
						t.Errorf("%s clusters deployment = %v; want %v", op, placed, tc.wantPlaced)
					}
				}
				if !slices.EqualFunc(st.GetIngress(), tc.wantIngress, func(a, b *aigridv1.ModelIngressStatus) bool { return proto.Equal(a, b) }) {
					t.Errorf("%s ingress = %v; want %v", op, st.GetIngress(), tc.wantIngress)
				}
				for _, in := range st.GetIngress() {
					if err := protovalidate.Validate(in); err != nil {
						t.Errorf("%s ingress[%s] fails its rules: %v", op, in.GetCluster(), err)
					}
				}
			}
		})
	}
}
