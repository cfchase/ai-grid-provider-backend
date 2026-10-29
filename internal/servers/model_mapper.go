package servers

import (
	"regexp"

	aigridv1 "github.com/osac-project/ai-grid-provider-backend/gen/ai_grid/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	defaultModelsNamespace = "ai-grid-internal-models"
	defaultGridNamespace   = "redhat-ai-grid"

	modelLabelKey = "ai-grid/model"

	defaultGatewayName      = "openshift-ai-inference"
	defaultGatewayNamespace = "openshift-ingress"

	llmInferenceAPIVersion = "serving.kserve.io/v1alpha2"
	llmInferenceKind       = "LLMInferenceService"

	maasModelRefAPIVersion = "maas.opendatahub.io/v1alpha1"
	maasModelRefKind       = "MaaSModelRef"
)

var (
	modelNameRE     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	modelURISchemes = []string{"hf://", "oci://", "s3://", "pvc://", "http://", "https://"}
)

func modelID(object *aigridv1.Model) string {
	if object == nil {
		return ""
	}
	if id := object.GetId(); id != "" {
		return id
	}
	if object.GetMetadata() != nil {
		return object.GetMetadata().GetName()
	}
	return ""
}

func modelFromLLMInferenceService(item unstructured.Unstructured) *aigridv1.Model {
	name := item.GetName()
	meta := &aigridv1.Metadata{
		Name:        name,
		Labels:      item.GetLabels(),
		Annotations: item.GetAnnotations(),
		Version:     resourceVersionToVersion(item.GetResourceVersion()),
	}
	if t := item.GetCreationTimestamp(); !t.IsZero() {
		meta.CreationTimestamp = timestamppb.New(t.Time)
	}
	if ts := item.GetDeletionTimestamp(); ts != nil && !ts.IsZero() {
		meta.DeletionTimestamp = timestamppb.New(ts.Time)
	}

	spec := &aigridv1.ModelSpec{
		Model: modelSourceFromUnstructured(item.Object),
		Main:  modelContainerFromUnstructured(item.Object),
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(item.Object, "spec", "replicas"); found {
		v := int32(nestedInt64(item.Object, "spec", "replicas"))
		spec.Replicas = &v
	}
	if refs, found, err := unstructured.NestedSlice(item.Object, "spec", "baseRefs"); err == nil && found {
		for _, raw := range refs {
			switch v := raw.(type) {
			case string:
				if v != "" {
					spec.BaseRefs = append(spec.BaseRefs, v)
				}
			case map[string]interface{}:
				if n := stringField(v, "name"); n != "" {
					spec.BaseRefs = append(spec.BaseRefs, n)
				}
			}
		}
	}
	if runtime, found, err := unstructured.NestedString(item.Object, "spec", "runtime"); err == nil && found {
		spec.Runtime = runtime
	}
	spec.Router = modelRouterFromUnstructured(item.Object)
	spec.Scaling = modelScalingFromUnstructured(item.Object)

	status := modelStatusFromLLMInferenceService(item.Object)
	return &aigridv1.Model{
		Id:       name,
		Metadata: meta,
		Spec:     spec,
		Status:   status,
	}
}

func modelSourceFromUnstructured(obj map[string]interface{}) *aigridv1.ModelSource {
	uri, _, _ := unstructured.NestedString(obj, "spec", "model", "uri")
	name, _, _ := unstructured.NestedString(obj, "spec", "model", "name")
	if uri == "" && name == "" {
		return nil
	}
	return &aigridv1.ModelSource{Uri: uri, Name: name}
}

func modelContainerFromUnstructured(obj map[string]interface{}) *aigridv1.ModelContainer {
	containers, found, err := unstructured.NestedSlice(obj, "spec", "template", "containers")
	if err != nil || !found {
		return nil
	}
	var main map[string]interface{}
	for _, raw := range containers {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if stringField(m, "name") == "main" || main == nil {
			main = m
			if stringField(m, "name") == "main" {
				break
			}
		}
	}
	if main == nil {
		return nil
	}
	out := &aigridv1.ModelContainer{
		Image: stringField(main, "image"),
	}
	if req, found, _ := unstructured.NestedStringMap(main, "resources", "requests"); found {
		out.Requests = req
	}
	if lim, found, _ := unstructured.NestedStringMap(main, "resources", "limits"); found {
		out.Limits = lim
	}
	if env, found, err := unstructured.NestedSlice(main, "env"); err == nil && found {
		for _, raw := range env {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			out.Env = append(out.Env, &aigridv1.ModelEnvVar{
				Name:  stringField(m, "name"),
				Value: stringField(m, "value"),
			})
		}
	}
	return out
}

func modelRouterFromUnstructured(obj map[string]interface{}) *aigridv1.ModelRouter {
	router, found, err := unstructured.NestedMap(obj, "spec", "router")
	if err != nil || !found {
		return nil
	}
	out := &aigridv1.ModelRouter{}
	if _, ok, _ := unstructured.NestedFieldNoCopy(router, "route"); ok {
		out.ManagedRoute = true
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(router, "scheduler"); ok {
		out.Scheduler = true
	}
	if refs, found, err := unstructured.NestedSlice(router, "gateway", "refs"); err == nil && found {
		for _, raw := range refs {
			m, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			out.GatewayRefs = append(out.GatewayRefs, &aigridv1.ModelGatewayRef{
				Name:        stringField(m, "name"),
				Namespace:   stringField(m, "namespace"),
				SectionName: stringField(m, "sectionName"),
			})
		}
	}
	return out
}

func modelScalingFromUnstructured(obj map[string]interface{}) *aigridv1.ModelScaling {
	scaling, found, err := unstructured.NestedMap(obj, "spec", "scaling")
	if err != nil || !found {
		return nil
	}
	out := &aigridv1.ModelScaling{
		MinReplicas: int32(nestedInt64(scaling, "minReplicas")),
		MaxReplicas: int32(nestedInt64(scaling, "maxReplicas")),
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(scaling, "keda"); ok {
		out.Keda = true
		if triggers, found, err := unstructured.NestedSlice(scaling, "keda", "triggers"); err == nil && found {
			for _, raw := range triggers {
				m, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				t := &aigridv1.ModelKedaTrigger{Type: stringField(m, "type")}
				if md, found, _ := unstructured.NestedStringMap(m, "metadata"); found {
					t.Metadata = md
				}
				out.Triggers = append(out.Triggers, t)
			}
		}
	}
	return out
}

func modelStatusFromLLMInferenceService(obj map[string]interface{}) *aigridv1.ModelStatus {
	status := &aigridv1.ModelStatus{}
	if url, found, err := unstructured.NestedString(obj, "status", "url"); err == nil && found {
		status.Url = url
	}
	if og := nestedInt64(obj, "status", "observedGeneration"); og != 0 {
		status.ObservedGeneration = og
	}
	if conds, found, err := unstructured.NestedSlice(obj, "status", "conditions"); err == nil && found {
		status.Conditions = conditionsFromSlice(conds)
	}
	deriveModelPhase(status)
	return status
}

func deriveModelPhase(status *aigridv1.ModelStatus) {
	if status == nil {
		return
	}
	ready := conditionByType(status.Conditions, "Ready")
	if ready == nil {
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		return
	}
	reason := ""
	if ready.Reason != nil {
		reason = *ready.Reason
	}
	message := ""
	if ready.Message != nil {
		message = *ready.Message
	}
	status.Reason = reason
	status.Message = message
	switch ready.Status {
	case aigridv1.ConditionStatus_CONDITION_STATUS_TRUE:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_READY
	case aigridv1.ConditionStatus_CONDITION_STATUS_FALSE:
		switch reason {
		case "ConfigNotFound", "RefsInvalid", "GatewaysNotReady":
			status.Phase = aigridv1.ModelPhase_MODEL_PHASE_FAILED
		default:
			status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
		}
	default:
		status.Phase = aigridv1.ModelPhase_MODEL_PHASE_PENDING
	}
}

func conditionByType(conds []*aigridv1.Condition, typ string) *aigridv1.Condition {
	for _, c := range conds {
		if c != nil && c.GetType() == typ {
			return c
		}
	}
	return nil
}

// --- builders ---

func llmInferenceServiceFromModel(object *aigridv1.Model, namespace string) *unstructured.Unstructured {
	id := modelID(object)
	meta := map[string]interface{}{
		"name":      id,
		"namespace": namespace,
	}
	labels := map[string]string{modelLabelKey: id, managedByLabelKey: managedByLabelValue}
	annotations := map[string]string{}
	if object.GetMetadata() != nil {
		for k, v := range object.GetMetadata().GetLabels() {
			labels[k] = v
		}
		for k, v := range object.GetMetadata().GetAnnotations() {
			annotations[k] = v
		}
	}
	meta["labels"] = stringMapToInterfaceMap(labels)
	if len(annotations) > 0 {
		meta["annotations"] = stringMapToInterfaceMap(annotations)
	}

	spec := modelSpecToLLMMap(object.GetSpec(), id)
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": llmInferenceAPIVersion,
		"kind":       llmInferenceKind,
		"metadata":   meta,
		"spec":       spec,
	}}
}

func modelSpecToLLMMap(spec *aigridv1.ModelSpec, id string) map[string]interface{} {
	out := map[string]interface{}{}
	if spec == nil {
		return out
	}
	modelName := id
	uri := ""
	if spec.GetModel() != nil {
		uri = spec.GetModel().GetUri()
		if n := spec.GetModel().GetName(); n != "" {
			modelName = n
		}
	}
	out["model"] = map[string]interface{}{
		"uri":  uri,
		"name": modelName,
	}
	if spec.Replicas != nil {
		out["replicas"] = int64(*spec.Replicas)
	}
	if len(spec.GetBaseRefs()) > 0 {
		refs := make([]interface{}, 0, len(spec.GetBaseRefs()))
		for _, name := range spec.GetBaseRefs() {
			refs = append(refs, map[string]interface{}{"name": name})
		}
		out["baseRefs"] = refs
	}
	if spec.GetRuntime() != "" {
		out["runtime"] = spec.GetRuntime()
	}
	out["router"] = modelRouterToMap(spec.GetRouter())
	if main := modelContainerToMap(spec.GetMain()); main != nil {
		out["template"] = map[string]interface{}{
			"containers": []interface{}{main},
		}
	}
	if scaling := modelScalingToMap(spec.GetScaling()); scaling != nil {
		out["scaling"] = scaling
	}
	return out
}

// schedulerMetricsCertDirArg makes the scheduler (the llm-d EPP) serve its
// metrics port, 9090, over HTTPS with the serving certificate KServe already
// mounts for it. The grid operator scrapes that port with a ServiceAccount
// token and refuses to send the token in plain text, so without this flag the
// provider reports no load and the grid can't balance the model. KServe merges
// these args with the scheduler preset's: the preset starts the EPP with
// `command`, so `args` add to it and drop none of its flags. Once the RHOAI
// payload has opendatahub-io/kserve#2128, the preset passes the same flag
// itself when TLS is on, and the repeated flag is harmless. The flag and its
// directory match kahowell/ai-grid-tenant's model definitions
// (apps/acm-models/*/10-*-cpu.yaml, 960bd69).
const schedulerMetricsCertDirArg = "--metrics-cert-dir=/var/run/kserve/tls"

func schedulerWithHTTPSMetrics() map[string]interface{} {
	return map[string]interface{}{
		"template": map[string]interface{}{
			"containers": []interface{}{
				map[string]interface{}{
					"name": "main",
					"args": []interface{}{schedulerMetricsCertDirArg},
				},
			},
		},
	}
}

func modelRouterToMap(router *aigridv1.ModelRouter) map[string]interface{} {
	out := map[string]interface{}{}
	managedRoute := true
	scheduler := false
	var refs []*aigridv1.ModelGatewayRef
	if router != nil {
		refs = router.GetGatewayRefs()
		scheduler = router.GetScheduler()
		// Honor explicit managed_route; otherwise default on when nothing else is set.
		if router.GetManagedRoute() || router.GetScheduler() || len(refs) > 0 {
			managedRoute = router.GetManagedRoute()
		}
	}
	if managedRoute {
		out["route"] = map[string]interface{}{}
	}
	if scheduler {
		out["scheduler"] = schedulerWithHTTPSMetrics()
	}
	if len(refs) == 0 {
		refs = []*aigridv1.ModelGatewayRef{{
			Name:      defaultGatewayName,
			Namespace: defaultGatewayNamespace,
		}}
	}
	gwRefs := make([]interface{}, 0, len(refs))
	for _, r := range refs {
		if r == nil {
			continue
		}
		entry := map[string]interface{}{"name": r.GetName()}
		if r.GetNamespace() != "" {
			entry["namespace"] = r.GetNamespace()
		}
		if r.GetSectionName() != "" {
			entry["sectionName"] = r.GetSectionName()
		}
		gwRefs = append(gwRefs, entry)
	}
	out["gateway"] = map[string]interface{}{"refs": gwRefs}
	return out
}

func modelContainerToMap(main *aigridv1.ModelContainer) map[string]interface{} {
	if main == nil {
		return map[string]interface{}{"name": "main"}
	}
	out := map[string]interface{}{"name": "main"}
	if main.GetImage() != "" {
		out["image"] = main.GetImage()
	}
	resources := map[string]interface{}{}
	if len(main.GetRequests()) > 0 {
		resources["requests"] = stringMapToInterfaceMap(main.GetRequests())
	}
	if len(main.GetLimits()) > 0 {
		resources["limits"] = stringMapToInterfaceMap(main.GetLimits())
	}
	if len(resources) > 0 {
		out["resources"] = resources
	}
	if len(main.GetEnv()) > 0 {
		env := make([]interface{}, 0, len(main.GetEnv()))
		for _, e := range main.GetEnv() {
			if e == nil || e.GetName() == "" {
				continue
			}
			env = append(env, map[string]interface{}{
				"name":  e.GetName(),
				"value": e.GetValue(),
			})
		}
		if len(env) > 0 {
			out["env"] = env
		}
	}
	return out
}

func modelScalingToMap(scaling *aigridv1.ModelScaling) map[string]interface{} {
	if scaling == nil {
		return nil
	}
	out := map[string]interface{}{
		"maxReplicas": int64(scaling.GetMaxReplicas()),
	}
	min := scaling.GetMinReplicas()
	if min <= 0 {
		min = 1
	}
	out["minReplicas"] = int64(min)
	if scaling.GetKeda() {
		triggers := make([]interface{}, 0, len(scaling.GetTriggers()))
		for _, t := range scaling.GetTriggers() {
			if t == nil {
				continue
			}
			entry := map[string]interface{}{"type": t.GetType()}
			if len(t.GetMetadata()) > 0 {
				entry["metadata"] = stringMapToInterfaceMap(t.GetMetadata())
			}
			triggers = append(triggers, entry)
		}
		out["keda"] = map[string]interface{}{"triggers": triggers}
	}
	return out
}

func maasModelRefFromModel(object *aigridv1.Model, namespace string) *unstructured.Unstructured {
	id := modelID(object)
	meta := map[string]interface{}{
		"name":      id,
		"namespace": namespace,
		"labels": stringMapToInterfaceMap(map[string]string{
			modelLabelKey:     id,
			managedByLabelKey: managedByLabelValue,
		}),
	}
	annotations := map[string]string{}
	if object.GetMetadata() != nil {
		for k, v := range object.GetMetadata().GetAnnotations() {
			annotations[k] = v
		}
	}
	if len(annotations) > 0 {
		meta["annotations"] = stringMapToInterfaceMap(annotations)
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": maasModelRefAPIVersion,
		"kind":       maasModelRefKind,
		"metadata":   meta,
		"spec": map[string]interface{}{
			"modelRef": map[string]interface{}{
				"kind": llmInferenceKind,
				"name": id,
			},
		},
	}}
}
