package servers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

func TestModelsGitCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.GetObject().GetMetadata().GetVersion() != 1 {
		t.Fatalf("Create version = %d, want 1", created.GetObject().GetMetadata().GetVersion())
	}
	if got := gitModelClusters(created.GetObject()); !slices.Equal(got, []string{"site-a"}) {
		t.Fatalf("Create placement clusters = %v", got)
	}
	if st := created.GetObject().GetStatus(); st.GetReason() != "SavedToGit" || st.GetPhase() != aigridv1.ModelPhase_MODEL_PHASE_UNSPECIFIED ||
		!slices.Equal(st.GetPlacement().GetSelectedClusters(), []string{"site-a"}) || st.GetPlacement().GetNumberOfSelectedClusters() != 1 {
		t.Fatalf("Create status = %v, want saved to Git on site-a", st)
	}
	for k := range created.GetObject().GetMetadata().GetAnnotations() {
		if strings.HasPrefix(k, "ai-grid/") {
			t.Fatalf("Create returned internal annotation %q", k)
		}
	}

	deployment := readGitYAMLDocs(t, writer, "catalog/models/sim/deployment/deployment.yaml")
	llm := deployment[0]
	if llm.GetKind() != llmInferenceKind || llm.GetNamespace() != "ai-grid-internal-models" || llm.GetAnnotations()[gitVersionAnnotation] != "1" {
		t.Fatalf("deployment[0] kind = %q, namespace = %q, annotations = %v", llm.GetKind(), llm.GetNamespace(), llm.GetAnnotations())
	}
	if _, found, _ := unstructured.NestedMap(llm.Object, "spec", "router", "scheduler"); !found {
		t.Fatal("LLMInferenceService has no spec.router.scheduler")
	}
	// The grid operator scrapes the scheduler's metrics only over HTTPS.
	containers, _, _ := unstructured.NestedSlice(llm.Object, "spec", "router", "scheduler", "template", "containers")
	if len(containers) != 1 {
		t.Fatalf("scheduler template containers = %v, want one main container", containers)
	}
	main, _ := containers[0].(map[string]interface{})
	args, _, _ := unstructured.NestedStringSlice(main, "args")
	if main["name"] != "main" || !slices.Equal(args, []string{"--metrics-cert-dir=/var/run/kserve/tls"}) {
		t.Fatalf("scheduler main container = %v, want args [--metrics-cert-dir=/var/run/kserve/tls]", main)
	}
	policy := deployment[1]
	if policy.GetKind() != "NetworkPolicy" || policy.GetName() != "sim-grid" || policy.GetNamespace() != "ai-grid-internal-models" {
		t.Fatalf("deployment[1] kind = %q, name = %q, namespace = %q", policy.GetKind(), policy.GetName(), policy.GetNamespace())
	}
	if got, _, _ := unstructured.NestedString(policy.Object, "spec", "podSelector", "matchLabels", "app.kubernetes.io/name"); got != "sim" {
		t.Fatalf("NetworkPolicy podSelector app.kubernetes.io/name = %q", got)
	}
	if got := kustomizationResources(t, writer, "catalog/models/sim/deployment/kustomization.yaml"); !slices.Equal(got, []string{"deployment.yaml"}) {
		t.Fatalf("deployment kustomization resources = %v", got)
	}

	ingress := readGitYAMLDocs(t, writer, "catalog/models/sim/catalog/model.yaml")
	external, modelRef := ingress[0], ingress[1]
	if external.GetKind() != "ExternalModel" || external.GetNamespace() != "models-as-a-service" {
		t.Fatalf("catalog[0] kind = %q, namespace = %q", external.GetKind(), external.GetNamespace())
	}
	refs, _, _ := unstructured.NestedSlice(external.Object, "spec", "externalProviderRefs")
	if len(refs) != 1 || refs[0].(map[string]interface{})["targetModel"] != "sim" {
		t.Fatalf("ExternalModel externalProviderRefs = %v", refs)
	}
	if kind, _, _ := unstructured.NestedString(modelRef.Object, "spec", "modelRef", "kind"); modelRef.GetKind() != maasModelRefKind || kind != "ExternalModel" || modelRef.GetNamespace() != "models-as-a-service" {
		t.Fatalf("catalog[1] kind = %q, modelRef.kind = %q, namespace = %q", modelRef.GetKind(), kind, modelRef.GetNamespace())
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-a/kustomization.yaml"); !slices.Equal(got, []string{"../../models/sim/deployment"}) {
		t.Fatalf("site-a resources = %v", got)
	}
	if got := kustomizationResources(t, writer, "catalog/ingress/kustomization.yaml"); !slices.Equal(got, []string{"../models/sim/catalog"}) {
		t.Fatalf("ingress resources = %v", got)
	}

	got, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "sim"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.GetObject().GetMetadata().GetVersion() != 1 || got.GetObject().GetSpec().GetModel().GetUri() != "hf://facebook/opt-125m" {
		t.Fatalf("Get = %v", got.GetObject())
	}
	list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if list.GetTotal() != 1 || list.GetItems()[0].GetId() != "sim" {
		t.Fatalf("List = %v", list)
	}

	move := gitSampleModel("sim", "site-b")
	move.Metadata.Version = 1
	updated, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     move,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
		Lock:       true,
	})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.GetObject().GetMetadata().GetVersion() != 2 || !slices.Equal(gitModelClusters(updated.GetObject()), []string{"site-b"}) {
		t.Fatalf("Update version = %d, clusters = %v", updated.GetObject().GetMetadata().GetVersion(), gitModelClusters(updated.GetObject()))
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-a/kustomization.yaml"); len(got) != 0 {
		t.Fatalf("site-a resources after the move = %v, want none (the file stays)", got)
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-b/kustomization.yaml"); !slices.Equal(got, []string{"../../models/sim/deployment"}) {
		t.Fatalf("site-b resources = %v", got)
	}

	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     move,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}},
		Lock:       true,
	})
	assertConnectCode(t, err, connect.CodeAborted)

	_, err = srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim", ResourceVersion: 1})
	assertConnectCode(t, err, connect.CodeAborted)
	if _, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim", ResourceVersion: 2}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err = srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "sim"})
	assertConnectCode(t, err, connect.CodeNotFound)
	head, _ := writer.Head(ctx)
	if names, _ := head.List("catalog/models/sim/deployment"); len(names) != 0 {
		t.Fatalf("model files after Delete = %v", names)
	}
	if got := kustomizationResources(t, writer, "catalog/ingress/kustomization.yaml"); len(got) != 0 {
		t.Fatalf("ingress resources after Delete = %v, want none (the file stays)", got)
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-b/kustomization.yaml"); len(got) != 0 {
		t.Fatalf("site-b resources after Delete = %v", got)
	}
}

func TestModelsGitSharedIndexes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	for _, id := range []string{"zeta", "alpha"} {
		if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel(id, "site-a", "site-c")}); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	want := []string{"../../models/alpha/deployment", "../../models/zeta/deployment"}
	for _, cluster := range []string{"site-a", "site-c"} {
		if got := kustomizationResources(t, writer, "catalog/clusters/"+cluster+"/kustomization.yaml"); !slices.Equal(got, want) {
			t.Fatalf("%s resources = %v, want %v", cluster, got, want)
		}
	}
	list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
	if err != nil || list.GetTotal() != 2 || list.GetItems()[0].GetId() != "alpha" {
		t.Fatalf("List = %v, %v", list, err)
	}

	if _, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "zeta", ResourceVersion: 1}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-a/kustomization.yaml"); !slices.Equal(got, want[:1]) {
		t.Fatalf("site-a resources after Delete = %v", got)
	}
	if got := kustomizationResources(t, writer, "catalog/ingress/kustomization.yaml"); !slices.Equal(got, []string{"../models/alpha/catalog"}) {
		t.Fatalf("ingress resources after Delete = %v", got)
	}

	// Narrowing the placement leaves the model only in the clusters it keeps.
	narrow := gitSampleModel("alpha", "site-c")
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: narrow, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}}}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-a/kustomization.yaml"); len(got) != 0 {
		t.Fatalf("site-a resources after narrowing = %v, want none", got)
	}
	if got := kustomizationResources(t, writer, "catalog/clusters/site-c/kustomization.yaml"); !slices.Equal(got, want[:1]) {
		t.Fatalf("site-c resources after narrowing = %v", got)
	}
}

func TestModelsGitValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitModelsServer(t, gitops.NewMemoryWriter(nil))

	clusterSets := gitSampleModel("a", "site-a")
	clusterSets.Spec.Placement.ClusterSets = []string{"global"}
	otherLabel := gitSampleModel("b", "site-a")
	otherLabel.Spec.Placement.LabelSelector.MatchExpressions[0].Key = "region"
	notIn := gitSampleModel("c", "site-a")
	notIn.Spec.Placement.LabelSelector.MatchExpressions[0].Operator = "NotIn"
	rollout := gitSampleModel("d", "site-a")
	rollout.Spec.Rollout = &aigridv1.ModelRollout{Strategy: aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_PROGRESSIVE}
	allWithFields := gitSampleModel("h", "site-a")
	allWithFields.Spec.Rollout = &aigridv1.ModelRollout{Strategy: aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_ALL, ProgressDeadline: "5m"}
	badCluster := gitSampleModel("e", "US East")
	noCluster := gitSampleModel("f")
	emptyPlacement := gitSampleModel("g", "site-a")
	emptyPlacement.Spec.Placement = &aigridv1.ModelPlacement{}
	// Without a placement the model would be published to the ingress and
	// deployed nowhere.
	noPlacement := gitSampleModel("i", "site-a")
	noPlacement.Spec.Placement = nil
	for name, object := range map[string]*aigridv1.Model{
		"cluster sets": clusterSets, "other label": otherLabel, "NotIn": notIn, "rollout": rollout, "bad cluster name": badCluster,
		"In with no values": noCluster, "empty placement": emptyPlacement, "All with a deadline": allWithFields,
		"no placement": noPlacement,
	} {
		_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: object})
		if err == nil {
			t.Fatalf("%s: Create() error = nil, want InvalidArgument", name)
		}
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	}

	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)
	// An update can't clear the placement either.
	unplace := gitSampleModel("sim")
	unplace.Spec.Placement = nil
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: unplace, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}}})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	// Strategy All alone is what git mode does anyway: accepted, not stored.
	all := gitSampleModel("all", "site-a")
	all.Spec.Rollout = &aigridv1.ModelRollout{Strategy: aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_ALL}
	if created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: all}); err != nil || created.GetObject().GetSpec().GetRollout() != nil {
		t.Fatalf("Create(rollout All) = %v, %v; want created with no rollout", created, err)
	}

	// match_labels name=<cluster> is the same placement as name In [<cluster>].
	byLabel := gitSampleModel("labelled", "site-a")
	byLabel.Spec.Placement.LabelSelector = &aigridv1.ModelLabelSelector{MatchLabels: map[string]string{"name": "site-c"}}
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: byLabel})
	if err != nil {
		t.Fatalf("Create(match_labels) error = %v", err)
	}
	if got := gitModelClusters(created.GetObject()); !slices.Equal(got, []string{"site-c"}) {
		t.Fatalf("match_labels clusters = %v", got)
	}

	// A label selector ANDs its requirements, so two name requirements would
	// select the clusters in both, not either: refused rather than guessed.
	for name, selector := range map[string]*aigridv1.ModelLabelSelector{
		"match_labels and In": {
			MatchLabels:      map[string]string{"name": "site-a"},
			MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: "name", Operator: "In", Values: []string{"site-b"}}},
		},
		"two In expressions": {
			MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{
				{Key: "name", Operator: "In", Values: []string{"site-a"}},
				{Key: "name", Operator: "In", Values: []string{"site-b"}},
			},
		},
	} {
		twice := gitSampleModel("twice")
		twice.Spec.Placement = &aigridv1.ModelPlacement{LabelSelector: selector}
		_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: twice})
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("Create(%s) error = %v, want InvalidArgument", name, err)
		}
	}
}

func TestModelsGitDeleteReferenced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	models := mustGitModelsServer(t, writer)
	subscriptions := mustGitSubscriptionsServer(t, writer)

	if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim-model", "site-a")}); err != nil {
		t.Fatalf("Create model error = %v", err)
	}
	// gitSampleSubscription refers to sim-model with no namespace, which git
	// mode resolves to models-as-a-service, where the model's MaaSModelRef is.
	if _, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
		t.Fatalf("Create subscription error = %v", err)
	}
	_, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim-model", ResourceVersion: 1})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}

// The reference guard holds against a concurrent write: a subscription to
// the model committed between the delete's read and its commit makes the
// delete Aborted, and a model deleted between a subscribe's read and its
// commit makes the subscribe Aborted.
func TestModelsGitDeleteRacesSubscribe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := &racingWriter{MemoryWriter: gitops.NewMemoryWriter(nil)}
	models := mustGitModelsServer(t, writer)
	subscriptions := mustGitSubscriptionsServer(t, writer.MemoryWriter)
	if _, err := models.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim-model", "site-a")}); err != nil {
		t.Fatalf("Create model error = %v", err)
	}

	writer.race = func() {
		if _, err := subscriptions.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-a")}); err != nil {
			t.Errorf("racing subscription Create() error = %v", err)
		}
	}
	_, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim-model"})
	assertConnectCode(t, err, connect.CodeAborted)

	if _, err := subscriptions.Delete(ctx, &aigridv1.SubscriptionsDeleteRequest{Id: "team-a"}); err != nil {
		t.Fatalf("Delete subscription error = %v", err)
	}
	racingSubs := mustGitSubscriptionsServer(t, writer)
	writer.race = func() {
		if _, err := models.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim-model"}); err != nil {
			t.Errorf("racing model Delete() error = %v", err)
		}
	}
	_, err = racingSubs.Create(ctx, &aigridv1.SubscriptionsCreateRequest{Object: gitSampleSubscription("team-b")})
	assertConnectCode(t, err, connect.CodeAborted)
}

func mustGitModelsServer(t *testing.T, writer gitops.Writer) *ModelsServer {
	t.Helper()
	// No dynamic client: git mode must never call the cluster.
	srv, err := NewModelsServer().SetWriter(writer).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return srv
}

// gitSampleModel is a model with the scheduler, placed on the named clusters.
func gitSampleModel(id string, clusters ...string) *aigridv1.Model {
	replicas := int32(1)
	model := &aigridv1.Model{
		Id:       id,
		Metadata: &aigridv1.Metadata{Name: id, Annotations: map[string]string{"openshift.io/display-name": id}},
		Spec: &aigridv1.ModelSpec{
			Model:    &aigridv1.ModelSource{Uri: "hf://facebook/opt-125m"},
			Replicas: &replicas,
			Main:     &aigridv1.ModelContainer{Image: "quay.io/example/vllm:latest"},
		},
	}
	model.Spec.Router = &aigridv1.ModelRouter{Scheduler: true}
	model.Spec.Placement = &aigridv1.ModelPlacement{LabelSelector: &aigridv1.ModelLabelSelector{
		MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: "name", Operator: "In", Values: clusters}},
	}}
	return model
}

func gitModelClusters(model *aigridv1.Model) []string {
	for _, req := range model.GetSpec().GetPlacement().GetLabelSelector().GetMatchExpressions() {
		if req.GetKey() == "name" {
			return req.GetValues()
		}
	}
	return nil
}

func kustomizationResources(t *testing.T, writer gitops.Writer, p string) []string {
	t.Helper()
	docs := readGitYAMLDocs(t, writer, p)
	resources, _, err := unstructured.NestedStringSlice(docs[0].Object, "resources")
	if err != nil {
		t.Fatalf("%s resources: %v", p, err)
	}
	return resources
}

// Git mode reads the ManagedClusters only for status.placement.locations, from
// each placed cluster's location label, for ListPlacementClusters and
// ListLocations, and to check the clusters a placement adds.
func TestModelsGitLocations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newManagedClusterClient(
		managedCluster("site-a", map[string]string{"location": "us-east"}, true),
		managedCluster("site-b", map[string]string{"location": "us-west"}, true),
		managedCluster("site-c", map[string]string{"location": "us-east"}, true),
	)
	srv, err := NewModelsServer().SetWriter(gitops.NewMemoryWriter(nil)).SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a", "site-c", "site-b")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	want := []string{"us-east", "us-west"}
	if got := created.GetObject().GetStatus().GetPlacement().GetLocations(); !slices.Equal(got, want) {
		t.Fatalf("Create locations = %v, want %v", got, want)
	}
	list, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got := list.GetItems()[0].GetStatus().GetPlacement().GetLocations(); !slices.Equal(got, want) {
		t.Fatalf("List locations = %v, want %v", got, want)
	}

	// Without a client there are no locations or placement clusters, and no
	// error.
	noClient := mustGitModelsServer(t, gitops.NewMemoryWriter(nil))
	locations, err := noClient.ListLocations(ctx, &aigridv1.ModelsListLocationsRequest{})
	if err != nil || len(locations.GetLocations()) != 0 {
		t.Fatalf("ListLocations() without a client = %v, %v", locations, err)
	}
	clusters, err := noClient.ListPlacementClusters(ctx, &aigridv1.ModelsListPlacementClustersRequest{})
	if err != nil || len(clusters.GetClusters()) != 0 {
		t.Fatalf("ListPlacementClusters() without a client = %v, %v", clusters, err)
	}
}

// ListPlacementClusters answers the names of the clusters a model can be
// placed on, since git mode places by cluster name: AI Grid members
// (ai-grid/enabled "true", role inference or combined, Compliant with the
// ready Policy) that are Available, with or without a location label, unique
// and sorted. The deprecated ListLocations answers the same list.
func TestModelsListPlacementClustersAnswersClusterNames(t *testing.T) {
	t.Parallel()
	available := []map[string]interface{}{{"type": "ManagedClusterConditionAvailable", "status": "True"}}
	unavailable := []map[string]interface{}{{"type": "ManagedClusterConditionAvailable", "status": "False"}}
	member := func(role string) map[string]string {
		return map[string]string{"ai-grid/enabled": "true", "ai-grid/role": role}
	}
	ready := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "policy.open-cluster-management.io/v1",
		"kind":       "Policy",
		"metadata":   map[string]interface{}{"name": "ai-grid-ready", "namespace": "ai-grid-policies"},
		"status": map[string]interface{}{"status": []interface{}{
			map[string]interface{}{"clustername": "site-a", "compliant": "Compliant"},
			map[string]interface{}{"clustername": "site-b", "compliant": "Compliant"},
			map[string]interface{}{"clustername": "site-c", "compliant": "Compliant"},
			map[string]interface{}{"clustername": "site-d", "compliant": "NonCompliant"},
			map[string]interface{}{"clustername": "site-e", "compliant": "Compliant"},
			map[string]interface{}{"clustername": "site-f", "compliant": "Compliant"},
		}},
	}}
	client := newManagedClusterClient(
		// Ready inference members, with or without a location label; no ai-grid/provisioned label (D3).
		managedClusterUnstructured("site-b", map[string]string{"ai-grid/enabled": "true", "ai-grid/role": "inference", "location": "us-west"}, nil, true, available),
		managedClusterUnstructured("site-a", member("combined"), nil, true, available),
		managedClusterUnstructured("site-c", member("ingress"), nil, true, available),   // serves no models
		managedClusterUnstructured("site-d", member("inference"), nil, true, available), // not provisioned yet
		managedClusterUnstructured("site-e", member("inference"), nil, true, unavailable),
		managedClusterUnstructured("site-f", map[string]string{"ai-grid/role": "inference"}, nil, true, available), // not enabled
		managedClusterUnstructured("site-g", member("inference"), nil, true, available),                            // not placed by the Policy
		ready,
	)
	srv, err := NewModelsServer().SetWriter(gitops.NewMemoryWriter(nil)).SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := []string{"site-a", "site-b"}
	clusters, err := srv.ListPlacementClusters(context.Background(), &aigridv1.ModelsListPlacementClustersRequest{})
	if err != nil {
		t.Fatalf("ListPlacementClusters() error = %v", err)
	}
	if !slices.Equal(clusters.GetClusters(), want) {
		t.Fatalf("ListPlacementClusters() = %v, want %v", clusters.GetClusters(), want)
	}
	got, err := srv.ListLocations(context.Background(), &aigridv1.ModelsListLocationsRequest{})
	if err != nil {
		t.Fatalf("ListLocations() error = %v", err)
	}
	if !slices.Equal(got.GetLocations(), want) {
		t.Fatalf("ListLocations() = %v, want %v", got.GetLocations(), want)
	}
}

// A placement names clusters, and status.placement.locations holds their
// location labels. A create or update that adds a name no ManagedCluster has
// (such as a location label copied from status.placement.locations) is
// InvalidArgument naming it, instead of a model deployed nowhere. A cluster the
// model is already on is not checked again, so a model on a cluster that has
// since left the hub can still be changed or moved off it.
func TestModelsGitPlacementNamesKnownClusters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := newManagedClusterClient(
		managedCluster("site-a", map[string]string{"location": "us-east"}, true),
		managedCluster("site-b", map[string]string{"location": "us-east"}, true),
	)
	srv, err := NewModelsServer().SetWriter(gitops.NewMemoryWriter(nil)).SetDynamicClient(client).Build()
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a", "site-b")})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	locations := created.GetObject().GetStatus().GetPlacement().GetLocations()
	if !slices.Equal(locations, []string{"us-east"}) {
		t.Fatalf("Create locations = %v, want [us-east]", locations)
	}

	// The location labels are not cluster names.
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("copy", locations...)})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if got := unknownPlacementClusters(t, err); !slices.Equal(got, []string{"us-east"}) {
		t.Fatalf("Create(us-east) unknown clusters = %v, want [us-east] (%v)", got, err)
	}
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("mixed", "site-a", "madrid", "paris")})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if got := unknownPlacementClusters(t, err); !slices.Equal(got, []string{"madrid", "paris"}) {
		t.Fatalf("Create(site-a, madrid, paris) unknown clusters = %v, want [madrid paris] (%v)", got, err)
	}
	if _, err := srv.Get(ctx, &aigridv1.ModelsGetRequest{Id: "copy"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("Get(copy) error = %v, want NotFound: a refused create commits nothing", err)
	}
	// An existing id is AlreadyExists before its clusters are checked.
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "madrid")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)

	placement := &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}}
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: gitSampleModel("sim", "site-a", "us-east"), UpdateMask: placement})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if got := unknownPlacementClusters(t, err); !slices.Equal(got, []string{"us-east"}) {
		t.Fatalf("Update(us-east) unknown clusters = %v, want [us-east] (%v)", got, err)
	}

	// site-b leaves the hub: the model can still be changed, and moved off it.
	if err := client.Resource(managedClustersResource).Delete(ctx, "site-b", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete ManagedCluster site-b: %v", err)
	}
	replicas := gitSampleModel("sim")
	two := int32(2)
	replicas.Spec.Replicas = &two
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: replicas, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.replicas"}}}); err != nil {
		t.Fatalf("Update(spec.replicas) on a model placed on a removed cluster error = %v", err)
	}
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: gitSampleModel("sim", "site-a", "site-b"), UpdateMask: placement}); err != nil {
		t.Fatalf("Update(placement kept) error = %v", err)
	}
	moved, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: gitSampleModel("sim", "site-a"), UpdateMask: placement})
	if err != nil {
		t.Fatalf("Update(placement site-a) error = %v", err)
	}
	if got := moved.GetObject().GetStatus().GetPlacement().GetSelectedClusters(); !slices.Equal(got, []string{"site-a"}) {
		t.Fatalf("moved selected_clusters = %v, want [site-a]", got)
	}
	// Moving back onto the removed cluster adds it again, so it is checked.
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: gitSampleModel("sim", "site-a", "site-b"), UpdateMask: placement})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if got := unknownPlacementClusters(t, err); !slices.Equal(got, []string{"site-b"}) {
		t.Fatalf("Update(site-a, site-b) unknown clusters = %v, want [site-b] (%v)", got, err)
	}
}

// unknownPlacementClusters returns the names in the google.rpc.ResourceInfo
// details of err that name an ai_grid.v1.ManagedCluster, in order.
func unknownPlacementClusters(t *testing.T, err error) []string {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("error = %v (%T), want *connect.Error", err, err)
	}
	var names []string
	for _, detail := range connectErr.Details() {
		msg, err := connectproto.UnmarshalErrorDetail(detail)
		if err != nil {
			t.Fatalf("UnmarshalErrorDetail(%s) error = %v", detail.Type, err)
		}
		if info, ok := msg.(*errdetails.ResourceInfo); ok && info.GetResourceType() == "type.googleapis.com/ai_grid.v1.ManagedCluster" {
			names = append(names, info.GetResourceName())
		}
	}
	return names
}

// Without the ManagedCluster API no cluster exists, so every name is unknown;
// a failed read of the clusters fails the call rather than skipping the check.
// The backend lists ManagedClusters with its own credentials, so any other
// list error, Forbidden included, is the service's own: Internal, as for
// Clusters/List, with the Kubernetes error kept as the cause.
func TestModelsGitPlacementClusterReadErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	managedClusters := schema.GroupResource{Group: "cluster.open-cluster-management.io", Resource: "managedclusters"}
	for name, tc := range map[string]struct {
		err  error
		want connect.Code
	}{
		"no ManagedCluster API": {
			err:  apierrors.NewNotFound(managedClusters, ""),
			want: connect.CodeInvalidArgument,
		},
		"list fails": {err: errors.New("api unavailable"), want: connect.CodeInternal},
		"list forbidden": {
			err:  apierrors.NewForbidden(managedClusters, "", errors.New("no RBAC")),
			want: connect.CodeInternal,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client := newManagedClusterClient(managedCluster("site-a", nil, true))
			client.PrependReactor("list", "managedclusters", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})
			srv, err := NewModelsServer().SetWriter(gitops.NewMemoryWriter(nil)).SetDynamicClient(client).Build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")})
			assertConnectCode(t, err, tc.want)
			if tc.want == connect.CodeInternal && !errors.Is(err, tc.err) {
				t.Fatalf("Create() error = %v, want the list error as its cause", err)
			}
			// ListPlacementClusters answers an empty list without the API.
			_, err = srv.ListPlacementClusters(ctx, &aigridv1.ModelsListPlacementClustersRequest{})
			if tc.want != connect.CodeInternal {
				if err != nil {
					t.Fatalf("ListPlacementClusters() error = %v, want nil", err)
				}
				return
			}
			assertConnectCode(t, err, connect.CodeInternal)
			if !errors.Is(err, tc.err) {
				t.Fatalf("ListPlacementClusters() error = %v, want the list error as its cause", err)
			}
		})
	}
}

// A zero resource_version skips the version check; Update needs a mask.
func TestModelsGitDeleteWithoutVersionAndUpdateNeedsMask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitModelsServer(t, gitops.NewMemoryWriter(nil))
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-a")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: gitSampleModel("sim", "site-b")})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
	if _, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err = srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim"})
	assertConnectCode(t, err, connect.CodeNotFound)
}

// Labels and annotations reach the files Argo CD applies: only keys without
// a prefix and the console's two openshift.io annotations are allowed, and
// the backend's own labels don't show, so a read-modify-write of the
// metadata round-trips.
func TestModelsGitMetadataKeyAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)
	allowed := gitSampleModel("sim", "site-a")
	allowed.Metadata.Labels = map[string]string{"team": "a"}
	allowed.Metadata.Annotations["openshift.io/description"] = "d"
	created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: allowed})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := created.GetObject().GetMetadata().GetLabels(); len(got) != 1 || got["team"] != "a" {
		t.Fatalf("Create labels = %v, want only the caller's", got)
	}
	llm := readGitYAMLDocs(t, writer, "catalog/models/sim/deployment/deployment.yaml")[0]
	if llm.GetLabels()[modelLabelKey] != "sim" || llm.GetLabels()[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("file labels = %v, want the backend's labels too", llm.GetLabels())
	}
	roundTrip := created.GetObject()
	roundTrip.Metadata.Labels["tier"] = "gold"
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: roundTrip, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.labels", "metadata.annotations"}}}); err != nil {
		t.Fatalf("Update(read metadata back) error = %v", err)
	}

	for _, key := range []string{"helm.sh/resource-policy", "argocd.argoproj.io/sync-options", "app.kubernetes.io/managed-by", "ai-grid/clusters"} {
		object := gitSampleModel("other", "site-a")
		object.Metadata.Labels = map[string]string{key: "x"}
		_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: object})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
		update := gitSampleModel("sim", "site-a")
		update.Metadata.Annotations[key] = "x"
		_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{Object: update, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata.annotations"}}})
		assertConnectCode(t, err, connect.CodeInvalidArgument)
	}
	if n := len(writer.Messages()); n != 2 {
		t.Fatalf("commits = %d, want the create and the metadata update", n)
	}
}

// List applies the CEL filter, the order and the window to the repository's
// models, and rejects a negative limit.
func TestModelsGitListFilterOrderPaging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitModelsServer(t, gitops.NewMemoryWriter(nil))
	for _, id := range []string{"bravo", "alpha", "charlie"} {
		if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel(id, "site-a")}); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}
	order := "-id"
	limit, offset := int32(1), int32(1)
	list, err := srv.List(ctx, &aigridv1.ModelsListRequest{Order: &order, Limit: &limit, Offset: &offset})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if got := idsOf(list.GetItems()); list.GetTotal() != 3 || !slices.Equal(got, []string{"bravo"}) {
		t.Fatalf("List(-id, offset 1, limit 1) = %v, total %d", got, list.GetTotal())
	}
	filter := `this.id.startsWith("a") || this.id == "charlie"`
	list, err = srv.List(ctx, &aigridv1.ModelsListRequest{Filter: &filter})
	if err != nil {
		t.Fatalf("List(filter) error = %v", err)
	}
	if got := idsOf(list.GetItems()); !slices.Equal(got, []string{"alpha", "charlie"}) {
		t.Fatalf("List(filter) = %v", got)
	}
	negative := int32(-1)
	_, err = srv.List(ctx, &aigridv1.ModelsListRequest{Limit: &negative})
	assertConnectCode(t, err, connect.CodeInvalidArgument)
}

// Until the repository is cloned, Models and Subscriptions answer
// Unavailable, so a client can retry.
func TestGitNotClonedIsUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.StartBackgroundWriter(ctx, func(context.Context) (gitops.Writer, error) {
		return nil, errors.New("clone: host unreachable")
	}, time.Hour)
	_, err := mustGitModelsServer(t, writer).List(ctx, &aigridv1.ModelsListRequest{})
	assertConnectCode(t, err, connect.CodeUnavailable)
	_, err = mustGitSubscriptionsServer(t, writer).Get(ctx, &aigridv1.SubscriptionsGetRequest{Id: "team-a"})
	assertConnectCode(t, err, connect.CodeUnavailable)
}

// A backend started without AI_GRID_GIT_URL answers FailedPrecondition for
// every Models call that reads or writes the catalog, naming the setting,
// rather than not starting at all. ListLocations reads the cluster instead.
func TestModelsGitUnconfigured(t *testing.T) {
	ctx := context.Background()
	srv := mustGitModelsServer(t, gitops.Unconfigured{})
	_, err := srv.List(ctx, &aigridv1.ModelsListRequest{})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), "AI_GRID_GIT_URL") {
		t.Errorf("List() error = %v, want it to name AI_GRID_GIT_URL", err)
	}
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("m1", "site-a")})
	assertConnectCode(t, err, connect.CodeFailedPrecondition)
}
