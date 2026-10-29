package servers

import (
	"maps"

	connect "connectrpc.com/connect/v2"
	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// How a tenant's call reaches a model the backend placed:
//
//  1. The ingress cluster's grid consumer reads the body's model name into
//     X-Gateway-Model-Name and looks for an InferenceProvider serving that
//     name. catalog/grid.yaml (renderModelGridCatalog) gives every ingress
//     cluster one per placed cluster C: backend peer-<C>, at the GridSite
//     labelled ai-grid/site=<C>. A combined cluster also has the local one
//     from deployment/grid.yaml, on backend local-models.
//  2. The consumer forwards to that backend, C's provider gateway, which
//     forwards everything to the models Gateway on C.
//  3. The models Gateway matches the header against each model's HTTPRoute
//     (deployment/grid.yaml, renderModelGridDeployment) and sends the call
//     to the model's InferencePool, so the model's scheduler picks the pod.
//
// The template installs the models Gateway, the backends and the GridSite
// labels once per cluster; the backend writes only per-model objects.

// servedModelName is the name the grid routes the model by: its id. MaaS
// lists the model under its id and forwards the request body unchanged (the
// AI gateway applies an ExternalModel's targetModel only with API translation
// on), so tenants send the id in "model", the ingress consumer routes on it,
// and vLLM must serve it: renderModelDeployment sets the LLMInferenceService's
// spec.model.name, vLLM's served name, to the id and keeps the caller's name in
// gitModelNameAnnotation. Ids are unique, so no two models share a route.
func servedModelName(object *aigridv1.Model) string {
	return object.GetId()
}

// renderModelGridDeployment renders deployment/grid.yaml, delivered with the
// model to each placed cluster: the HTTPRoute on the models Gateway, and the
// InferenceProvider for a consumer on the same cluster. An empty siteSelector
// means the cluster's own site; on an inference-only cluster no consumer
// declares local-models, so the provider is unused.
func (s *ModelsServer) renderModelGridDeployment(object *aigridv1.Model) ([]byte, error) {
	id := object.GetId()
	served := servedModelName(object)
	route := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]interface{}{
			"name":      id + "-grid",
			"namespace": s.namespace,
			"labels":    gridLabels(id),
		},
		"spec": map[string]interface{}{
			"parentRefs": []interface{}{map[string]interface{}{
				"group":     "gateway.networking.k8s.io",
				"kind":      "Gateway",
				"name":      gridModelsGateway,
				"namespace": gridModelsGatewayNS,
			}},
			"rules": []interface{}{map[string]interface{}{
				"matches": []interface{}{map[string]interface{}{
					"path": map[string]interface{}{"type": "PathPrefix", "value": "/v1/"},
					"headers": []interface{}{map[string]interface{}{
						"name": gridModelHeader, "type": "Exact", "value": served,
					}},
				}},
				"backendRefs": []interface{}{map[string]interface{}{
					"group": "inference.networking.k8s.io",
					"kind":  "InferencePool",
					// KServe names the pool for the LLMInferenceService.
					"name": id + "-inference-pool",
					"port": int64(8000),
				}},
				// Generation can take minutes; the consumer and the
				// provider gateway hold their own limits.
				"timeouts": map[string]interface{}{"request": "0s", "backendRequest": "0s"},
			}},
		},
	}}
	local := gridInferenceProvider("grid-"+id+"-local", id, served, gridLocalBackend, nil)
	return marshalYAMLDocs(route, local)
}

// renderModelGridCatalog renders catalog/grid.yaml, delivered to every
// ingress cluster: one InferenceProvider per placed cluster, at the GridSite
// labelled with the cluster's name, on the consumer backend peer-<cluster>.
// On the placed cluster itself no GridSite carries its own name, so its entry
// matches nothing there and the local provider serves instead.
func renderModelGridCatalog(object *aigridv1.Model, clusters []string) ([]byte, error) {
	id := object.GetId()
	served := servedModelName(object)
	providers := make([]*unstructured.Unstructured, 0, len(clusters))
	for _, cluster := range clusters {
		providers = append(providers, gridInferenceProvider("grid-"+id+"-at-"+cluster, id, served,
			gridPeerBackendPrefix+cluster, map[string]string{gridSiteLabelKey: cluster}))
	}
	return marshalYAMLDocs(providers...)
}

// gridInferenceProvider is a cluster-scoped InferenceProvider that tells the
// consumer which backend serves model served. The endpoint is a placeholder:
// the consumer sends to the backend clusterName names, and without a
// healthCheck nothing probes the endpoint.
func gridInferenceProvider(name, id, served, backend string, siteLabels map[string]string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"gridNetworkRef": gridNetworkName,
		"providerKind":   "vllm",
		"backendKind":    "local_model",
		"clusterName":    backend,
		"endpoint":       "https://" + backend + ".grid.internal/v1",
		"models": []interface{}{map[string]interface{}{
			"name":         served,
			"capabilities": []interface{}{"text_generation"},
		}},
	}
	if len(siteLabels) > 0 {
		spec["siteSelector"] = map[string]interface{}{"matchLabels": stringMapToInterfaceMap(siteLabels)}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "grid.praxis.fast/v1alpha1",
		"kind":       "InferenceProvider",
		"metadata":   map[string]interface{}{"name": name, "labels": gridLabels(id)},
		"spec":       spec,
	}}
}

func gridLabels(id string) map[string]interface{} {
	return stringMapToInterfaceMap(map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue})
}

// validateModelGitMetadata is validateGitMetadata plus the models-only
// annotation modelAPIAnnotation, which must be modelAPIChat or
// modelAPICompletions.
func validateModelGitMetadata(meta *aigridv1.Metadata) error {
	annotations := meta.GetAnnotations()
	api, ok := annotations[modelAPIAnnotation]
	if !ok {
		return validateGitMetadata(meta)
	}
	if api != modelAPIChat && api != modelAPICompletions {
		return connect.Errorf(connect.CodeInvalidArgument, "metadata.annotations[%s] must be %q or %q, got %q", modelAPIAnnotation, modelAPIChat, modelAPICompletions, api)
	}
	rest := maps.Clone(annotations)
	delete(rest, modelAPIAnnotation)
	return validateGitMetadata(&aigridv1.Metadata{Labels: meta.GetLabels(), Annotations: rest})
}
