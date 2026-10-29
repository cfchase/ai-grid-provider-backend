package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"github.com/osac-project/ai-grid-provider-backend/internal/gitops"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// In git mode each model is two directories under catalog/models/<id>/, and
// Argo CD delivers them through two kinds of index:
//
//   - deployment/: the LLMInferenceService and its NetworkPolicy, in the
//     models namespace. catalog/clusters/<name>/kustomization.yaml lists
//     the deployments placed on the ManagedCluster <name>.
//   - catalog/: the ExternalModel and MaaSModelRef that publish the model
//     through the grid, in the subscription namespace.
//     catalog/ingress/kustomization.yaml lists every model's, for the ingress
//     spokes.
//
// Index files are never deleted, so a cluster or the ingress keeps its path
// when its last model goes, and Argo CD prunes instead of failing.
const (
	modelsGitDir    = "catalog/models"
	clustersGitDir  = "catalog/clusters"
	ingressGitIndex = "catalog/ingress/kustomization.yaml"

	// gitClustersAnnotation records the model's clusters on its
	// LLMInferenceService, so a read returns the placement without scanning
	// every cluster index.
	gitClustersAnnotation = "ai-grid/clusters"
	// legacyModelNameAnnotation held the caller's spec.model.name in the
	// first builds that served a model as its id. The backend no longer
	// writes it: a read drops it, so the next update removes it from Git.
	legacyModelNameAnnotation = "ai-grid/model-name"
	// gridProviderName is the ExternalProvider that publishes the grid
	// gateway to MaaS on each ingress spoke.
	gridProviderName = "grid-provider"
	// clusterNameLabelKey is the label ACM sets on every ManagedCluster to
	// its name.
	clusterNameLabelKey = "name"
)

func modelDeploymentGitDir(id string) string { return path.Join(modelsGitDir, id, "deployment") }
func modelCatalogGitDir(id string) string    { return path.Join(modelsGitDir, id, "catalog") }
func clusterGitIndex(cluster string) string {
	return path.Join(clustersGitDir, cluster, "kustomization.yaml")
}

// gitModel is a model read from its files, with its clusters.
type gitModel struct {
	model    *aigridv1.Model
	clusters []string
}

func (s *ModelsServer) gitList(ctx context.Context) ([]*aigridv1.Model, error) {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("list", "", err)
	}
	resources, err := readKustomizationResources(head, ingressGitIndex)
	if err != nil {
		return nil, err
	}
	items := make([]*aigridv1.Model, 0, len(resources))
	reports := s.readModelReports(ctx)
	for _, resource := range resources {
		// ../models/<id>/catalog
		id := path.Base(path.Dir(resource))
		current, err := s.readGitModel(head, id)
		if err != nil {
			return nil, err
		}
		reports.apply(current)
		items = append(items, current.model)
	}
	return items, nil
}

func (s *ModelsServer) gitGet(ctx context.Context, id string) (*aigridv1.Model, error) {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("get", id, err)
	}
	current, err := s.readGitModel(head, id)
	if err != nil {
		return nil, err
	}
	s.readModelReports(ctx).apply(current)
	return current.model, nil
}

func (s *ModelsServer) gitCreate(ctx context.Context, object *aigridv1.Model) (*aigridv1.Model, error) {
	id := object.GetId()
	if err := validateGitMetadata(object.GetMetadata()); err != nil {
		return nil, err
	}
	clusters, err := gitPlacementClusters(object.GetSpec())
	if err != nil {
		return nil, err
	}
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("create", id, err)
	}
	if _, err := head.Read(path.Join(modelDeploymentGitDir(id), "deployment.yaml")); err == nil {
		return nil, connect.Errorf(connect.CodeAlreadyExists, "Model %q already exists", id)
	} else if !errors.Is(err, gitops.ErrNotFound) {
		return nil, modelGitError("create", id, err)
	}
	if err := s.checkPlacementClusters(ctx, clusters); err != nil {
		return nil, err
	}
	return s.gitCommitModel(ctx, head, "create", object, 1, nil, clusters)
}

func (s *ModelsServer) gitUpdate(ctx context.Context, req *aigridv1.ModelsUpdateRequest) (*aigridv1.Model, error) {
	object := req.GetObject()
	id := modelID(object)
	version := object.GetMetadata().GetVersion()
	if req.GetLock() && version == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "lock requires metadata.version")
	}
	paths, err := parseModelUpdateMask(req.GetUpdateMask())
	if err != nil {
		return nil, err
	}
	head, err := s.writer.Head(ctx)
	if err != nil {
		return nil, modelGitError("update", id, err)
	}
	current, err := s.readGitModel(head, id)
	if err != nil {
		return nil, err
	}
	if version != 0 && version != current.model.GetMetadata().GetVersion() {
		return nil, staleModelError("update", id)
	}

	merged, err := mergeModelUpdate(current.model, object, paths)
	if err != nil {
		return nil, err
	}
	merged.Status = nil
	if err := validateGitMetadata(merged.GetMetadata()); err != nil {
		return nil, err
	}
	normalizeModelResources(merged.GetSpec().GetMain())
	if err := validateModelSpec(merged.GetSpec()); err != nil {
		return nil, err
	}
	servedNameOnUpdate(current.model, merged)
	if err := normalizeServedName(id, merged.GetSpec()); err != nil {
		return nil, err
	}
	clusters, err := gitPlacementClusters(merged.GetSpec())
	if err != nil {
		return nil, err
	}
	var added []string
	for _, cluster := range clusters {
		if !slices.Contains(current.clusters, cluster) {
			added = append(added, cluster)
		}
	}
	if err := s.checkPlacementClusters(ctx, added); err != nil {
		return nil, err
	}
	return s.gitCommitModel(ctx, head, "update", merged, current.model.GetMetadata().GetVersion()+1, current.clusters, clusters)
}

// gitDelete removes the model's files and its index entries. A nonzero
// version must equal the model's, or the delete is Aborted. It is refused while
// a subscription in the repository names the model, and the subscriptions
// directory is in the commit's read set, so a subscription written since the
// check makes the delete Aborted.
func (s *ModelsServer) gitDelete(ctx context.Context, id string, version int32) error {
	head, err := s.writer.Head(ctx)
	if err != nil {
		return modelGitError("delete", id, err)
	}
	current, err := s.readGitModel(head, id)
	if err != nil {
		return err
	}
	if version != 0 && version != current.model.GetMetadata().GetVersion() {
		return staleModelError("delete", id)
	}
	if err := s.gitEnsureNotReferenced(head, id); err != nil {
		return err
	}

	change := gitops.Change{Put: map[string][]byte{}, Read: []string{subscriptionsGitDir}, Message: "models: delete " + id}
	for _, dir := range []string{modelDeploymentGitDir(id), modelCatalogGitDir(id)} {
		names, err := head.List(dir)
		if err != nil {
			return modelGitError("delete", id, err)
		}
		for _, name := range names {
			change.Delete = append(change.Delete, path.Join(dir, name))
		}
	}
	if err := putIndexChanges(head, change.Put, id, current.clusters, nil, false); err != nil {
		return err
	}
	if _, err := s.writer.Commit(ctx, head.Revision(), change); err != nil {
		return modelGitError("delete", id, err)
	}
	return nil
}

// gitCommitModel writes the model's files at version, moves it from the
// cluster indexes in from to those in to, and returns the model as written.
func (s *ModelsServer) gitCommitModel(ctx context.Context, head gitops.Snapshot, op string, object *aigridv1.Model, version int32, from, to []string) (*aigridv1.Model, error) {
	id := object.GetId()
	change := gitops.Change{Put: map[string][]byte{}, Message: "models: " + op + " " + id}

	deployment, err := s.renderModelDeployment(object, version, to)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "render model %q: %v", id, err).WithCause(err)
	}
	catalog, err := s.renderModelCatalog(object)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "render model %q: %v", id, err).WithCause(err)
	}
	change.Put[path.Join(modelDeploymentGitDir(id), "deployment.yaml")] = deployment
	change.Put[path.Join(modelDeploymentGitDir(id), "kustomization.yaml")] = renderKustomization([]string{"deployment.yaml"})
	change.Put[path.Join(modelCatalogGitDir(id), "model.yaml")] = catalog
	change.Put[path.Join(modelCatalogGitDir(id), "kustomization.yaml")] = renderKustomization([]string{"model.yaml"})
	if err := putIndexChanges(head, change.Put, id, from, to, true); err != nil {
		return nil, err
	}

	if _, err := s.writer.Commit(ctx, head.Revision(), change); err != nil {
		return nil, modelGitError(op, id, err)
	}
	written, err := parseGitModel(deployment)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read back model %q: %v", id, err).WithCause(err)
	}
	s.readModelReports(ctx).apply(written)
	return written.model, nil
}

// modelReports is what the status Policies report about models: inference
// holds each cluster's LLMInferenceServices (models-ready), ingress each
// cluster's MaaSModelRefs (model-refs-ready). Both are named by model id.
//
// ingressClusters are the clusters the hub labels as ingress: ai-grid/enabled
// "true" and ai-grid/role ingress or combined, the same labels
// ListPlacementClusters reads for inference and combined. Each should report
// the MaaSModelRef whether or not the catalog Policy lists it, so an ingress
// cluster the Policy hasn't reached yet, or a Policy that can't be read,
// doesn't count as ready.
type modelReports struct {
	inference, ingress ObjectReports
	ingressClusters    []string
}

func (s *ModelsServer) readModelReports(ctx context.Context) modelReports {
	if s.status == nil {
		return modelReports{}
	}
	return modelReports{
		inference:       readReports(ctx, s.logger, s.status, modelsStatusPolicy, modelsReadyCheck),
		ingress:         readReports(ctx, s.logger, s.status, catalogStatusPolicy, modelRefsReadyCheck),
		ingressClusters: s.ingressClusters(ctx),
	}
}

// ingressClusters lists the clusters labelled ai-grid/enabled "true" and
// ai-grid/role ingress or combined. Without a dynamic client, or when the
// list fails (logged), it knows none, and only the catalog Policy's clusters
// count as ingress.
func (s *ModelsServer) ingressClusters(ctx context.Context) []string {
	if s.client == nil {
		return nil
	}
	items, err := s.listManagedClusters(ctx)
	if err != nil {
		s.logger.Warn("list ingress clusters for model status", "error", err)
		return nil
	}
	var names []string
	for i := range items {
		labels := items[i].GetLabels()
		if role := labels["ai-grid/role"]; labels["ai-grid/enabled"] == "true" && (role == "ingress" || role == "combined") {
			names = append(names, items[i].GetName())
		}
	}
	return names
}

// apply sets the model's status from the reports. Each cluster the file
// places the model on should report its LLMInferenceService, and each ingress
// cluster (the catalog Policy's, and r.ingressClusters) should report its
// MaaSModelRef; a combined cluster reports both. Each cluster is ready when every report it
// should send says ready, not ready when one says not ready (or not found),
// and otherwise not reporting. status.message summarises every cluster, and
// status.clusters holds each placed one (ModelClusterStatus). The model is
// READY when every cluster is ready; PARTIALLY_AVAILABLE when every ingress
// cluster reports the MaaSModelRef ready and some, not all, placed clusters
// are ready; otherwise PENDING (reason NotReady). Both need at least one
// ingress cluster: with none, nothing shows the model published, so it is
// PENDING and the message says so. That is the console's
// precedence (ModelPhase in model_type.proto). With no report at all it stays
// saved to Git, with no clusters. It is never FAILED: the Policies can't tell
// a failed model from one that is still starting.
func (r modelReports) apply(current *gitModel) {
	id := current.model.GetId()
	placed := current.clusters
	clusters := slices.Clone(placed)
	for cluster := range r.ingress {
		clusters = append(clusters, cluster)
	}
	clusters = append(clusters, r.ingressClusters...)
	slices.Sort(clusters)
	clusters = slices.Compact(clusters)

	var ready, notReady, noReport []string
	var placedStatus []*aigridv1.ModelClusterStatus
	placedReady, ingressReady, anyIngress := 0, true, false
	reported := false
	for _, cluster := range clusters {
		isPlaced := slices.Contains(placed, cluster)
		inference, hasInference := r.inference[cluster][id]
		ingress, hasIngress := r.ingress[cluster][id]
		_, isIngress := r.ingress[cluster]
		isIngress = isIngress || slices.Contains(r.ingressClusters, cluster)
		anyIngress = anyIngress || isIngress
		hasInference = hasInference && isPlaced
		reported = reported || hasInference || hasIngress
		var state string
		switch {
		case (hasInference && !inference) || (hasIngress && !ingress):
			notReady = append(notReady, cluster)
			state = "NotReady"
		case (isPlaced && !hasInference) || (isIngress && !hasIngress):
			noReport = append(noReport, cluster)
			state = "NoReport"
		case hasInference || hasIngress:
			ready = append(ready, cluster)
			state = "Ready"
		}
		if isIngress && (!hasIngress || !ingress) {
			ingressReady = false
		}
		if !isPlaced {
			continue
		}
		entry := &aigridv1.ModelClusterStatus{
			Cluster: cluster,
			Phase:   aigridv1.ModelPhase_MODEL_PHASE_PENDING,
			Reason:  state,
			Message: clusterReports(hasInference, inference, isIngress, hasIngress, ingress),
		}
		if state == "Ready" {
			entry.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
			placedReady++
		}
		placedStatus = append(placedStatus, entry)
	}
	if !reported {
		return
	}
	status := current.model.Status
	status.Message = clusterSummary(notReady, ready, noReport)
	if !anyIngress {
		status.Message += "; no ingress cluster has reported the MaaSModelRef"
	}
	status.Clusters = placedStatus
	switch {
	case !anyIngress:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		status.Reason = "NotReady"
	case len(notReady) == 0 && len(noReport) == 0:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
		status.Reason = "Ready"
	case ingressReady && placedReady > 0 && placedReady < len(placedStatus):
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PARTIALLY_AVAILABLE
		status.Reason = "PartiallyAvailable"
	default:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		status.Reason = "NotReady"
	}
}

// clusterReports says what one placed cluster reported about the model, for
// example "LLMInferenceService ready; MaaSModelRef not ready" on a combined
// cluster. "Not ready" covers the Policies' "not found" as well (ObjectReports).
func clusterReports(hasInference, inference, isIngress, hasIngress, ingress bool) string {
	report := func(kind string, has, ready bool) string {
		switch {
		case !has:
			return "no " + kind + " report"
		case ready:
			return kind + " ready"
		default:
			return kind + " not ready"
		}
	}
	message := report("LLMInferenceService", hasInference, inference)
	if isIngress {
		message += "; " + report("MaaSModelRef", hasIngress, ingress)
	}
	return strings.ToUpper(message[:1]) + message[1:]
}

// renderModelDeployment renders the inference side: the LLMInferenceService,
// with the version and clusters annotations, and a NetworkPolicy that lets
// the grid reach the model's workload and scheduler metrics.
func (s *ModelsServer) renderModelDeployment(object *aigridv1.Model, version int32, clusters []string) ([]byte, error) {
	id := object.GetId()
	llm := llmInferenceServiceFromModel(object, s.namespace)
	annotations := llm.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[gitVersionAnnotation] = strconv.FormatInt(int64(version), 10)
	annotations[gitClustersAnnotation] = strings.Join(clusters, ",")
	delete(annotations, legacyModelNameAnnotation)
	llm.SetAnnotations(annotations)
	if err := unstructured.SetNestedField(llm.Object, servedModelName(object), "spec", "model", "name"); err != nil {
		return nil, err
	}

	policy := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]interface{}{
			"name":      id + "-grid",
			"namespace": s.namespace,
			"labels":    stringMapToInterfaceMap(map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue}),
		},
		"spec": map[string]interface{}{
			"podSelector": map[string]interface{}{
				"matchLabels": map[string]interface{}{"app.kubernetes.io/name": id},
			},
			"policyTypes": []interface{}{"Ingress"},
			"ingress": []interface{}{map[string]interface{}{
				// 8000: the grid gateway to the workload; 9090: the grid
				// operator scraping the scheduler's metrics, which it serves
				// over HTTPS on the same port (schedulerMetricsCertDirArg).
				"ports": []interface{}{
					map[string]interface{}{"port": int64(8000), "protocol": "TCP"},
					map[string]interface{}{"port": int64(9090), "protocol": "TCP"},
				},
				"from": []interface{}{map[string]interface{}{
					"namespaceSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": s.gridNamespace},
					},
				}},
			}},
		},
	}}
	return marshalYAMLDocs(llm, policy)
}

// renderModelCatalog renders the ingress side: an ExternalModel routed to the
// grid provider, and the MaaSModelRef that lists it in MaaS.
func (s *ModelsServer) renderModelCatalog(object *aigridv1.Model) ([]byte, error) {
	id := object.GetId()
	target := servedModelName(object)
	external := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "inference.opendatahub.io/v1alpha1",
		"kind":       "ExternalModel",
		"metadata": map[string]interface{}{
			"name":      id,
			"namespace": s.subscriptionNamespace,
			"labels":    stringMapToInterfaceMap(map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue}),
		},
		"spec": map[string]interface{}{
			"externalProviderRefs": []interface{}{map[string]interface{}{
				"ref":         map[string]interface{}{"name": gridProviderName},
				"apiFormat":   "openai-chat",
				"path":        "/v1/chat/completions",
				"targetModel": target,
				"weight":      int64(1),
			}},
		},
	}}
	modelRef := maasModelRefFromModel(object, s.subscriptionNamespace)
	if err := unstructured.SetNestedField(modelRef.Object, "ExternalModel", "spec", "modelRef", "kind"); err != nil {
		return nil, err
	}
	return marshalYAMLDocs(external, modelRef)
}

func (s *ModelsServer) readGitModel(head gitops.Snapshot, id string) (*gitModel, error) {
	p := path.Join(modelDeploymentGitDir(id), "deployment.yaml")
	data, err := head.Read(p)
	if err != nil {
		return nil, modelGitError("get", id, err)
	}
	current, err := parseGitModel(data)
	if err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read %s: %v", p, err).WithCause(err)
	}
	return current, nil
}

// parseGitModel reads the model from its deployment file: the annotations
// become metadata.version and spec.placement. The status says the model is
// saved to Git (no phase, reason SavedToGit) and lists the clusters the file
// places it on; modelReports.apply replaces the phase when the clusters report.
func parseGitModel(data []byte) (*gitModel, error) {
	docs := bytes.SplitN(data, []byte("\n---\n"), 2)
	llm := map[string]interface{}{}
	if err := yaml.Unmarshal(docs[0], &llm); err != nil {
		return nil, err
	}
	item := unstructured.Unstructured{Object: llm}
	if item.GetKind() != llmInferenceKind {
		return nil, fmt.Errorf("first document is %q, want %s", item.GetKind(), llmInferenceKind)
	}
	model := modelFromLLMInferenceService(item)
	annotations := model.GetMetadata().GetAnnotations()
	version, err := strconv.ParseInt(annotations[gitVersionAnnotation], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("annotation %s: %w", gitVersionAnnotation, err)
	}
	var clusters []string
	if s := annotations[gitClustersAnnotation]; s != "" {
		clusters = strings.Split(s, ",")
	}
	// spec.model.name is the name the deployment serves. A model written
	// before the backend served models as their id serves its own name until
	// its next update switches it (servedNameOnUpdate), so a read reports
	// that name rather than claim the id early. Empty is the id.
	if model.Spec.GetModel() != nil && model.Spec.Model.GetName() == "" {
		model.Spec.Model.Name = model.GetId()
	}
	delete(annotations, gitVersionAnnotation)
	delete(annotations, gitClustersAnnotation)
	delete(annotations, legacyModelNameAnnotation)
	if len(annotations) == 0 {
		model.Metadata.Annotations = nil
	}
	// The backend sets ai-grid/model and app.kubernetes.io/managed-by itself,
	// and a caller may not send them back, so they don't show in the API.
	delete(model.Metadata.Labels, modelLabelKey)
	delete(model.Metadata.Labels, managedByLabelKey)
	if len(model.Metadata.Labels) == 0 {
		model.Metadata.Labels = nil
	}
	model.Metadata.Version = int32(version)
	model.Status = &aigridv1.ModelStatus{
		Reason:  "SavedToGit",
		Message: "Saved to Git; no cluster has reported the model's status",
	}
	if len(clusters) > 0 {
		model.Status.Placement = &aigridv1.ModelPlacementStatus{NumberOfSelectedClusters: int32(len(clusters)), SelectedClusters: clusters}
		model.Spec.Placement = &aigridv1.ModelPlacement{LabelSelector: &aigridv1.ModelLabelSelector{
			MatchExpressions: []*aigridv1.ModelLabelSelectorRequirement{{Key: clusterNameLabelKey, Operator: "In", Values: clusters}},
		}}
	}
	return &gitModel{model: model, clusters: clusters}, nil
}

// gitEnsureNotReferenced refuses to delete a model that a subscription in the
// repository still names.
func (s *ModelsServer) gitEnsureNotReferenced(head gitops.Snapshot, id string) error {
	names, err := head.List(subscriptionsGitDir)
	if err != nil {
		return modelGitError("delete", id, err)
	}
	for _, name := range names {
		subID, ok := strings.CutSuffix(name, ".yaml")
		if !ok {
			continue
		}
		doc, err := readSubscriptionDoc(head, subID)
		if err != nil {
			return err
		}
		for _, ref := range doc.sub.GetSpec().GetModelRefs() {
			if ref.GetName() == id && (ref.GetNamespace() == "" || ref.GetNamespace() == s.subscriptionNamespace) {
				return connect.Errorf(connect.CodeFailedPrecondition, "Model %q is referenced by Subscription %q", id, subID)
			}
		}
	}
	return nil
}

// gitPlacementClusters returns the clusters a placement names. Git mode
// places by cluster name only (the name label ACM sets on every
// ManagedCluster): Argo CD delivers a cluster's index to that cluster, so
// cluster sets, other labels, a cluster count and rollouts have nothing to
// map to. A placement is required: without one the model would be published
// to the ingress and deployed nowhere.
func gitPlacementClusters(spec *aigridv1.ModelSpec) ([]string, error) {
	if !defaultRollout(spec.GetRollout()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.rollout supports only strategy All in git mode")
	}
	placement := spec.GetPlacement()
	if placement == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.placement must name at least one cluster in git mode")
	}
	if len(placement.GetClusterSets()) > 0 || placement.NumberOfClusters != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.placement supports only the cluster name label in git mode")
	}
	var clusters []string
	selector := placement.GetLabelSelector()
	// A label selector ANDs its requirements, so two name requirements would
	// select only the clusters in both.
	if len(selector.GetMatchLabels())+len(selector.GetMatchExpressions()) > 1 {
		return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement must use one %q requirement in git mode: a label selector ANDs them", clusterNameLabelKey)
	}
	for key, value := range selector.GetMatchLabels() {
		if key != clusterNameLabelKey {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement label %q is not supported in git mode; use %q", key, clusterNameLabelKey)
		}
		clusters = append(clusters, value)
	}
	for _, req := range selector.GetMatchExpressions() {
		if req.GetKey() != clusterNameLabelKey || req.GetOperator() != "In" {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "spec.placement supports only %q In [...] in git mode", clusterNameLabelKey)
		}
		clusters = append(clusters, req.GetValues()...)
	}
	if len(clusters) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "spec.placement must name at least one cluster in git mode")
	}
	for _, cluster := range clusters {
		if !modelNameRE.MatchString(cluster) {
			return nil, connect.Errorf(connect.CodeInvalidArgument, "cluster name %q is not a valid RFC 1123 DNS label", cluster)
		}
	}
	slices.Sort(clusters)
	return slices.Compact(clusters), nil
}

// defaultRollout reports whether r asks for what git mode does anyway: no
// rollout, or strategy All with no other field. Argo CD delivers every
// cluster's index at once, which is All, so that request is accepted and not
// stored.
func defaultRollout(r *aigridv1.ModelRollout) bool {
	if r == nil {
		return true
	}
	switch r.GetStrategy() {
	case aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_UNSPECIFIED, aigridv1.ModelRolloutStrategy_MODEL_ROLLOUT_STRATEGY_ALL:
	default:
		return false
	}
	return r.GetMaxConcurrency() == "" && r.GetMaxFailures() == "" && r.GetProgressDeadline() == "" && r.GetMinSuccessTime() == ""
}

// putIndexChanges adds to put the index files that change when model id
// moves from the clusters in from to those in to. With inIngress the model
// is in the ingress index, without it the model leaves it.
func putIndexChanges(head gitops.Snapshot, put map[string][]byte, id string, from, to []string, inIngress bool) error {
	deployment := "../../" + path.Join("models", id, "deployment")
	for _, cluster := range from {
		if !slices.Contains(to, cluster) {
			if err := putIndexEntry(head, put, clusterGitIndex(cluster), deployment, false); err != nil {
				return err
			}
		}
	}
	for _, cluster := range to {
		if err := putIndexEntry(head, put, clusterGitIndex(cluster), deployment, true); err != nil {
			return err
		}
	}
	return putIndexEntry(head, put, ingressGitIndex, "../"+path.Join("models", id, "catalog"), inIngress)
}

// putIndexEntry adds or removes one resource in a kustomization index, and
// puts the file only when it changes.
func putIndexEntry(head gitops.Snapshot, put map[string][]byte, index, resource string, present bool) error {
	resources, err := readKustomizationResources(head, index)
	if err != nil {
		return err
	}
	if slices.Contains(resources, resource) == present {
		return nil
	}
	if present {
		resources = append(resources, resource)
		slices.Sort(resources)
	} else {
		resources = slices.DeleteFunc(resources, func(r string) bool { return r == resource })
	}
	put[index] = renderKustomization(resources)
	return nil
}

// readKustomizationResources returns an index's resources; a missing index
// has none.
func readKustomizationResources(head gitops.Snapshot, index string) ([]string, error) {
	data, err := head.Read(index)
	if errors.Is(err, gitops.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, modelGitError("read", index, err)
	}
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(data, &k); err != nil {
		return nil, connect.Errorf(connect.CodeInternal, "read %s: %v", index, err).WithCause(err)
	}
	return k.Resources, nil
}

func renderKustomization(resources []string) []byte {
	if resources == nil {
		resources = []string{}
	}
	data, _ := yaml.Marshal(map[string]interface{}{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  resources,
	})
	return data
}

func modelGitError(op, id string, err error) error {
	switch {
	case errors.Is(err, gitops.ErrNotFound):
		return connect.Errorf(connect.CodeNotFound, "Model %q not found", id).WithCause(err)
	case errors.Is(err, gitops.ErrConflict):
		return staleModelError(op, id).WithCause(err)
	case errors.Is(err, gitops.ErrNotConfigured):
		// Not one model's problem, so the message names the API, not the id.
		return connect.Errorf(connect.CodeFailedPrecondition, "Models: %v", err).WithCause(err)
	default:
		return connect.Errorf(connect.CodeUnavailable, "%s Model %q: git: %v", op, id, err).WithCause(err)
	}
}

// servedModelName is the name vLLM serves the model as and the ExternalModel
// targets: its id. MaaS lists the model under its id and forwards the request
// body unchanged (the AI gateway applies an ExternalModel's targetModel only
// with API translation on), so the "model" a tenant sends, and the grid routes
// on, is the id. renderModelDeployment sets the LLMInferenceService's
// spec.model.name to it. Ids are unique, so no two models share a served name.
func servedModelName(object *aigridv1.Model) string {
	return object.GetId()
}

// servedNameOnUpdate switches a model that still serves another name, written
// before the backend served models as their id, to its id. Any update does
// it, whatever it changes, and so does one that sends back the name a read
// reported: vLLM's served name changes, so its pods restart once. A name other
// than the stored one or the id is left for normalizeServedName to refuse.
func servedNameOnUpdate(current, merged *aigridv1.Model) {
	source := merged.GetSpec().GetModel()
	if source != nil && source.GetName() == current.GetSpec().GetModel().GetName() {
		source.Name = ""
	}
}

// normalizeServedName accepts a spec.model.name that is empty or the id, and sets
// it to the id. As in odh-dashboard, a model's served name is its resource
// name; the human-readable name is annotation openshift.io/display-name.
// Any other name would be one nothing serves, so it is InvalidArgument.
func normalizeServedName(id string, spec *aigridv1.ModelSpec) error {
	source := spec.GetModel()
	if source == nil {
		return nil
	}
	if name := source.GetName(); name != "" && name != id {
		return connect.Errorf(connect.CodeInvalidArgument,
			"spec.model.name %q must be empty or the model id %q: a model is served as its id, which is the name tenants send; set a human-readable name in annotation %s",
			name, id, annotationDisplayName)
	}
	source.Name = id
	return nil
}
