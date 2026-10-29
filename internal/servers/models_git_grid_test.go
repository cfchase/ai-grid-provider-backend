package servers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The grid routes a model by name: each inference cluster gets an HTTPRoute on
// its models Gateway, matched on the model name the consumer puts in
// X-Gateway-Model-Name, and every ingress cluster gets one InferenceProvider
// per placed cluster, so the consumer knows which site serves the model.
func TestModelsGitGridFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-b", "site-c")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if got := kustomizationResources(t, writer, "catalog/models/sim/deployment/kustomization.yaml"); !slices.Equal(got, []string{"deployment.yaml", "grid.yaml"}) {
		t.Fatalf("deployment kustomization resources = %v", got)
	}
	grid := readGitYAMLDocs(t, writer, "catalog/models/sim/deployment/grid.yaml")
	if len(grid) != 2 {
		t.Fatalf("deployment/grid.yaml has %d documents, want 2", len(grid))
	}
	route := grid[0]
	if route.GetKind() != "HTTPRoute" || route.GetName() != "sim-grid" || route.GetNamespace() != "ai-grid-internal-models" {
		t.Fatalf("grid[0] kind = %q, name = %q, namespace = %q", route.GetKind(), route.GetName(), route.GetNamespace())
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if len(parents) != 1 {
		t.Fatalf("HTTPRoute parentRefs = %v", parents)
	}
	parent := parents[0].(map[string]interface{})
	if parent["name"] != "ai-grid-models" || parent["namespace"] != "openshift-ingress" || parent["kind"] != "Gateway" {
		t.Fatalf("HTTPRoute parentRef = %v", parent)
	}
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("HTTPRoute rules = %v", rules)
	}
	rule := rules[0].(map[string]interface{})
	match := rule["matches"].([]interface{})[0].(map[string]interface{})
	if p := match["path"].(map[string]interface{}); p["type"] != "PathPrefix" || p["value"] != "/v1/" {
		t.Fatalf("HTTPRoute path match = %v", p)
	}
	header := match["headers"].([]interface{})[0].(map[string]interface{})
	if header["name"] != "X-Gateway-Model-Name" || header["type"] != "Exact" || header["value"] != "example/sim" {
		t.Fatalf("HTTPRoute header match = %v", header)
	}
	backend := rule["backendRefs"].([]interface{})[0].(map[string]interface{})
	if backend["kind"] != "InferencePool" || backend["group"] != "inference.networking.k8s.io" || backend["name"] != "sim-inference-pool" || fmt.Sprint(backend["port"]) != "8000" {
		t.Fatalf("HTTPRoute backendRef = %v", backend)
	}
	if timeouts := rule["timeouts"].(map[string]interface{}); timeouts["request"] != "0s" || timeouts["backendRequest"] != "0s" {
		t.Fatalf("HTTPRoute timeouts = %v", timeouts)
	}

	local := grid[1]
	if local.GetKind() != "InferenceProvider" || local.GetName() != "grid-sim-local" || local.GetNamespace() != "" {
		t.Fatalf("grid[1] kind = %q, name = %q, namespace = %q", local.GetKind(), local.GetName(), local.GetNamespace())
	}
	if got, _, _ := unstructured.NestedString(local.Object, "spec", "clusterName"); got != "local-models" {
		t.Fatalf("local InferenceProvider clusterName = %q", got)
	}
	if selector, found, _ := unstructured.NestedMap(local.Object, "spec", "siteSelector"); found && len(selector) > 0 {
		t.Fatalf("local InferenceProvider siteSelector = %v, want empty", selector)
	}
	assertProviderModels(t, local, "example/sim")

	if got := kustomizationResources(t, writer, "catalog/models/sim/catalog/kustomization.yaml"); !slices.Equal(got, []string{"grid.yaml", "model.yaml"}) {
		t.Fatalf("catalog kustomization resources = %v", got)
	}
	assertPeerProviders(t, writer, "sim", "site-b", "site-c")
}

func TestModelsGitGridPlacementChangeRewritesProviders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: gitSampleModel("sim", "site-b", "site-c")}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     gitSampleModel("sim", "site-c", "site-d"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
	}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	assertPeerProviders(t, writer, "sim", "site-c", "site-d")

	if _, err := srv.Delete(ctx, &aigridv1.ModelsDeleteRequest{Id: "sim"}); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	head, _ := writer.Head(ctx)
	for _, dir := range []string{"catalog/models/sim/deployment", "catalog/models/sim/catalog"} {
		if names, _ := head.List(dir); len(names) != 0 {
			t.Fatalf("%s after Delete = %v, want no files", dir, names)
		}
	}
}

// The served name is spec.model.name, or the id when the model has none, as
// the ExternalModel's targetModel.
func TestModelsGitGridServedNameDefaultsToID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	model := gitSampleModel("sim", "site-b")
	model.Spec.Model.Name = ""
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	grid := readGitYAMLDocs(t, writer, "catalog/models/sim/deployment/grid.yaml")
	rules, _, _ := unstructured.NestedSlice(grid[0].Object, "spec", "rules")
	header := rules[0].(map[string]interface{})["matches"].([]interface{})[0].(map[string]interface{})["headers"].([]interface{})[0].(map[string]interface{})
	if header["value"] != "sim" {
		t.Fatalf("HTTPRoute header value = %v, want the id", header["value"])
	}
	assertProviderModels(t, grid[1], "sim")
	peers := readGitYAMLDocs(t, writer, "catalog/models/sim/catalog/grid.yaml")
	assertProviderModels(t, peers[0], "sim")
}

// Two models with one served name on one cluster would put two HTTPRoutes with
// the same match on that cluster's models Gateway, so the second is refused.
// On different clusters the name is the same model served in two places, so
// it is accepted only from the same source (TestModelsGitGridServedNameSource).
func TestModelsGitGridServedNameConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitModelsServer(t, gitops.NewMemoryWriter(nil))

	shared := func(id string, clusters ...string) *aigridv1.Model {
		model := gitSampleModel(id, clusters...)
		model.Spec.Model.Name = "facebook/opt-125m"
		return model
	}
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: shared("first", "site-a", "site-b")}); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: shared("second", "site-b")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)

	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: shared("elsewhere", "site-c")}); err != nil {
		t.Fatalf("Create(elsewhere) error = %v, want a model on another cluster accepted", err)
	}
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     shared("elsewhere", "site-a"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
	})
	assertConnectCode(t, err, connect.CodeAlreadyExists)

	// A model doesn't conflict with itself.
	if _, err := srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     shared("first", "site-a"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.placement"}},
	}); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	other := gitSampleModel("other", "site-a")
	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: other}); err != nil {
		t.Fatalf("Create(other) error = %v, want another served name accepted", err)
	}
}

// Annotation ai-grid/api picks the OpenAI API the ExternalModel calls:
// completions for base models with no chat template, chat (the default)
// otherwise.
func TestModelsGitAPIAnnotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	writer := gitops.NewMemoryWriter(nil)
	srv := mustGitModelsServer(t, writer)

	for id, want := range map[string]string{"chat-default": "/v1/chat/completions", "chat": "/v1/chat/completions", "completions": "/v1/completions"} {
		model := gitSampleModel(id, "site-"+id)
		if id != "chat-default" {
			model.Metadata.Annotations["ai-grid/api"] = id
		}
		created, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: model})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
		external := readGitYAMLDocs(t, writer, "catalog/models/"+id+"/catalog/model.yaml")[0]
		refs, _, _ := unstructured.NestedSlice(external.Object, "spec", "externalProviderRefs")
		if got := refs[0].(map[string]interface{})["path"]; got != want {
			t.Fatalf("%s: ExternalModel path = %v, want %s", id, got, want)
		}
		if id != "chat-default" && created.GetObject().GetMetadata().GetAnnotations()["ai-grid/api"] != id {
			t.Fatalf("%s: Create annotations = %v, want ai-grid/api kept", id, created.GetObject().GetMetadata().GetAnnotations())
		}
	}

	bad := gitSampleModel("bad", "site-a")
	bad.Metadata.Annotations["ai-grid/api"] = "embeddings"
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: bad})
	assertConnectCode(t, err, connect.CodeInvalidArgument)

	// Subscriptions still don't accept the annotation.
	if err := validateGitMetadata(&aigridv1.Metadata{Annotations: map[string]string{"ai-grid/api": "chat"}}); err == nil {
		t.Fatal("validateGitMetadata accepted ai-grid/api, want it limited to models")
	}
}

func assertProviderModels(t *testing.T, provider *unstructured.Unstructured, want ...string) {
	t.Helper()
	models, _, _ := unstructured.NestedSlice(provider.Object, "spec", "models")
	var names []string
	for _, m := range models {
		names = append(names, m.(map[string]interface{})["name"].(string))
	}
	if !slices.Equal(names, want) {
		t.Fatalf("%s models = %v, want %v", provider.GetName(), names, want)
	}
	if got, _, _ := unstructured.NestedString(provider.Object, "spec", "gridNetworkRef"); got != "grid" {
		t.Fatalf("%s gridNetworkRef = %q", provider.GetName(), got)
	}
}

// assertPeerProviders checks that catalog/<id>/grid.yaml holds one
// InferenceProvider per cluster, for that cluster's peer backend and site.
func assertPeerProviders(t *testing.T, writer gitops.Writer, id string, clusters ...string) {
	t.Helper()
	docs := readGitYAMLDocs(t, writer, "catalog/models/"+id+"/catalog/grid.yaml")
	if len(docs) != len(clusters) {
		t.Fatalf("catalog/grid.yaml has %d documents, want %d", len(docs), len(clusters))
	}
	for i, cluster := range clusters {
		p := docs[i]
		if p.GetKind() != "InferenceProvider" || p.GetName() != "grid-"+id+"-at-"+cluster || p.GetNamespace() != "" {
			t.Fatalf("provider %d kind = %q, name = %q, namespace = %q", i, p.GetKind(), p.GetName(), p.GetNamespace())
		}
		if got, _, _ := unstructured.NestedString(p.Object, "spec", "clusterName"); got != "peer-"+cluster {
			t.Fatalf("%s clusterName = %q", p.GetName(), got)
		}
		if got, _, _ := unstructured.NestedStringMap(p.Object, "spec", "siteSelector", "matchLabels"); len(got) != 1 || got["ai-grid/site"] != cluster {
			t.Fatalf("%s siteSelector = %v", p.GetName(), got)
		}
		assertProviderModels(t, p, "example/"+id)
	}
}

// An ingress cluster balances one served name across every site that serves
// it, so two models may share the name only when they serve the same thing:
// the same model URI and the same image. Otherwise tenants asking for the name
// would get either model.
func TestModelsGitGridServedNameSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv := mustGitModelsServer(t, gitops.NewMemoryWriter(nil))
	shared := func(id, uri string, clusters ...string) *aigridv1.Model {
		model := gitSampleModel(id, clusters...)
		model.Spec.Model = &aigridv1.ModelSource{Uri: uri, Name: "shared/model"}
		return model
	}

	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: shared("first", "hf://org/one", "site-a")}); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	_, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: shared("other-uri", "hf://org/two", "site-b")})
	assertConnectCode(t, err, connect.CodeAlreadyExists)
	if err == nil || !strings.Contains(err.Error(), `"first"`) {
		t.Fatalf("error = %v, want it to name model first", err)
	}

	otherImage := shared("other-image", "hf://org/one", "site-b")
	otherImage.Spec.Main.Image = "quay.io/example/other:latest"
	_, err = srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: otherImage})
	assertConnectCode(t, err, connect.CodeAlreadyExists)

	if _, err := srv.Create(ctx, &aigridv1.ModelsCreateRequest{Object: shared("same", "hf://org/one", "site-b")}); err != nil {
		t.Fatalf("Create(same) error = %v, want the same source on another cluster accepted", err)
	}

	// An update that moves a sharing model to another source collides too.
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     shared("same", "hf://org/two", "site-b"),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.model"}},
	})
	assertConnectCode(t, err, connect.CodeAlreadyExists)
	image := shared("same", "hf://org/one", "site-b")
	image.Spec.Main.Image = "quay.io/example/other:latest"
	_, err = srv.Update(ctx, &aigridv1.ModelsUpdateRequest{
		Object:     image,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec.main.image"}},
	})
	assertConnectCode(t, err, connect.CodeAlreadyExists)
}
