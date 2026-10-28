package servers

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var applicationsResource = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}

// CatalogApps names the Argo CD ApplicationSets on the hub that deliver what
// git mode commits. Each generates one Application per cluster, named
// <ApplicationSet>-<cluster> and owned by the ApplicationSet. Empty fields
// take the defaults, the names of kahowell/ai-grid-tenant's
// apps/acm-models/hub/catalog-04-applicationsets.yaml.
type CatalogApps struct {
	// Namespace holds the ApplicationSets and their Applications
	// (AI_GRID_CATALOG_APP_NAMESPACE, default openshift-gitops).
	Namespace string
	// Models delivers each cluster's LLMInferenceServices
	// (AI_GRID_CATALOG_MODELS_APPSET).
	Models string
	// Ingress delivers every model's ExternalModel and MaaSModelRef
	// (AI_GRID_CATALOG_INGRESS_APPSET).
	Ingress string
	// Subscriptions delivers every MaaSSubscription and MaaSAuthPolicy
	// (AI_GRID_CATALOG_SUBSCRIPTIONS_APPSET).
	Subscriptions string
}

// The defaults for CatalogApps.
const (
	DefaultCatalogAppNamespace        = "openshift-gitops"
	DefaultCatalogModelsAppSet        = "acm-models-catalog-models"
	DefaultCatalogIngressAppSet       = "acm-models-catalog-ingress"
	DefaultCatalogSubscriptionsAppSet = "acm-models-catalog-subscriptions"
)

func (a CatalogApps) withDefaults() CatalogApps {
	for _, field := range []struct {
		value *string
		def   string
	}{
		{&a.Namespace, DefaultCatalogAppNamespace},
		{&a.Models, DefaultCatalogModelsAppSet},
		{&a.Ingress, DefaultCatalogIngressAppSet},
		{&a.Subscriptions, DefaultCatalogSubscriptionsAppSet},
	} {
		if *field.value == "" {
			*field.value = field.def
		}
	}
	return a
}

// CatalogSet picks one of the catalog's ApplicationSets.
type CatalogSet int

const (
	ModelsSet CatalogSet = iota
	IngressSet
	SubscriptionsSet
)

func (a CatalogApps) name(set CatalogSet) string {
	switch set {
	case ModelsSet:
		return a.Models
	case IngressSet:
		return a.Ingress
	default:
		return a.Subscriptions
	}
}

// The kinds git mode reads status for.
const (
	kindLLMInferenceService = "LLMInferenceService"
	kindExternalModel       = "ExternalModel"
	kindMaaSModelRef        = "MaaSModelRef"
	kindMaaSSubscription    = "MaaSSubscription"
	kindMaaSAuthPolicy      = "MaaSAuthPolicy"
)

// CatalogStatusReader reads what the spoke clusters report about the objects
// git mode delivers, from the health Argo CD records for each resource of the
// catalog's Applications on the hub (status.resources[]). The hub's Argo CD
// computes that health with its resource health checks, such as
// kahowell/ai-grid-tenant's for LLMInferenceService and the MaaS kinds.
type CatalogStatusReader struct {
	client dynamic.Interface
	apps   CatalogApps
	logger *slog.Logger
	// warned is set once a missing or forbidden list has been logged.
	warned atomic.Bool
}

// NewCatalogStatusReader returns a reader for the Applications apps names.
func NewCatalogStatusReader(client dynamic.Interface, apps CatalogApps) *CatalogStatusReader {
	return &CatalogStatusReader{client: client, apps: apps.withDefaults(), logger: slog.Default()}
}

// SetLogger sets where the reader warns that it can't list Applications.
func (r *CatalogStatusReader) SetLogger(logger *slog.Logger) *CatalogStatusReader {
	r.logger = logger
	return r
}

// ObjectState is what a cluster reports about one object.
type ObjectState int

const (
	// StatePending: not delivered yet, out of sync, or still progressing.
	StatePending ObjectState = iota
	// StateReady: in sync and healthy.
	StateReady
	// StateFailed: degraded, or its sync failed.
	StateFailed
)

// ObjectReport is one cluster's report about one object.
type ObjectReport struct {
	State ObjectState
	// Message is Argo CD's health or sync message, when it has one.
	Message string
	// Retrying: pending because its sync failed and Argo CD is retrying it,
	// such as an AppProject refusing the kind or namespace. Message then
	// says why.
	Retrying bool
}

// ObjectReports maps each cluster that has an Application in the set to the
// objects of one kind it lists, by name. A cluster without an Application in
// the set isn't in the map. An object its Application doesn't list isn't on
// that cluster yet.
type ObjectReports map[string]map[string]ObjectReport

// any reports whether at least one cluster has an Application in the set.
func (r ObjectReports) any() bool {
	return len(r) > 0
}

// Read returns, for each cluster with an Application in set, what it reports
// about the objects of kind. A missing or forbidden list (Argo CD isn't
// installed, or the ServiceAccount can't read Applications) reports nothing
// rather than failing, so git mode still answers from Git. A nil reader
// reports nothing. Read lists the namespace's Applications on every call: one
// request, for the handful of clusters a grid has.
func (r *CatalogStatusReader) Read(ctx context.Context, set CatalogSet, kind string) (ObjectReports, error) {
	if r == nil {
		return nil, nil
	}
	list, err := r.client.Resource(applicationsResource).Namespace(r.apps.Namespace).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		// Once per process: every model then shows saved to Git, which is
		// easy to miss when the RBAC or Argo CD is what's missing.
		if r.warned.CompareAndSwap(false, true) {
			r.logger.Warn("can't list the catalog's Argo CD Applications; models and subscriptions report no status",
				"namespace", r.apps.Namespace, "error", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list Applications in %s: %w", r.apps.Namespace, err)
	}
	appSet := r.apps.name(set)
	out := ObjectReports{}
	for _, app := range list.Items {
		cluster, ok := clusterOf(app, appSet)
		if !ok {
			continue
		}
		out[cluster] = applicationReports(app, kind)
	}
	return out, nil
}

// clusterOf returns the cluster an Application of ApplicationSet appSet is
// for: its name after "<appSet>-". An Application the ApplicationSet doesn't
// own isn't one of its, whatever its name, so a hand-made Application can't
// stand in for a cluster's catalog, and an ApplicationSet whose name extends
// another's doesn't count as that one.
func clusterOf(app unstructured.Unstructured, appSet string) (string, bool) {
	cluster, found := strings.CutPrefix(app.GetName(), appSet+"-")
	if !found || cluster == "" {
		return "", false
	}
	for _, owner := range app.GetOwnerReferences() {
		if owner.Kind == "ApplicationSet" && owner.Name == appSet {
			return cluster, true
		}
	}
	return "", false
}

// applicationReports reads the objects of kind in an Application's
// status.resources. A resource is ready when it is Synced and Healthy;
// failed when Degraded, or out of sync or missing after a failed sync; and
// pending otherwise (OutOfSync, Progressing, Missing, Suspended, Unknown, or
// no health). OutOfSync outranks Healthy: the live object is then the
// previous revision. A failed sync counts only when it was a sync to the
// revision the Application now wants, so an old failure doesn't fail an
// object edited since. While Argo CD retries a failed sync (phase Running,
// retryCount above zero), an object it hasn't synced is pending with the
// operation's message: an AppProject refusal shows only there, not in
// status.conditions, while the resource reads Missing and the Application
// Healthy. Once the retries run out the phase is Failed. Resources Argo CD is pruning (requiresPruning) are
// left out, and when a name repeats (the same object in two namespaces while
// the models namespace moves) the worse report wins.
func applicationReports(app unstructured.Unstructured, kind string) map[string]ObjectReport {
	syncFailed, retrying := false, false
	phase, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "phase")
	syncMessage, _, _ := unstructured.NestedString(app.Object, "status", "operationState", "message")
	retries, _, _ := unstructured.NestedInt64(app.Object, "status", "operationState", "retryCount")
	tried, hasTried, _ := unstructured.NestedString(app.Object, "status", "operationState", "syncResult", "revision")
	wanted, _, _ := unstructured.NestedString(app.Object, "status", "sync", "revision")
	switch {
	case (phase == "Failed" || phase == "Error") && tried == wanted:
		syncFailed = true
	case phase == "Running" && retries > 0 && (!hasTried || tried == wanted):
		retrying = true
	}
	out := map[string]ObjectReport{}
	resources, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	for _, raw := range resources {
		res, _ := raw.(map[string]interface{})
		if k, _ := res["kind"].(string); k != kind {
			continue
		}
		name, _ := res["name"].(string)
		if pruning, _ := res["requiresPruning"].(bool); name == "" || pruning {
			continue
		}
		synced, _ := res["status"].(string)
		health, _, _ := unstructured.NestedString(res, "health", "status")
		message, _, _ := unstructured.NestedString(res, "health", "message")
		report := ObjectReport{State: StatePending, Message: message}
		switch {
		case health == "Degraded":
			report.State = StateFailed
		case synced != "Synced" && syncFailed:
			report = ObjectReport{State: StateFailed, Message: syncMessage}
		case synced != "Synced" && retrying:
			report = ObjectReport{State: StatePending, Message: syncMessage, Retrying: true}
		case synced == "Synced" && health == "Healthy":
			report.State = StateReady
		}
		if previous, seen := out[name]; seen && worse(previous.State, report.State) {
			continue
		}
		out[name] = report
	}
	return out
}

// worse reports whether state a is worse than b: failed, then pending, then
// ready.
func worse(a, b ObjectState) bool {
	rank := map[ObjectState]int{StateReady: 0, StatePending: 1, StateFailed: 2}
	return rank[a] > rank[b]
}

// readReports reads one kind's reports for a git-mode request. Status is read
// on every request, with no cache. A failed read is logged and reports
// nothing, so the request still answers from Git.
func readReports(ctx context.Context, logger *slog.Logger, status *CatalogStatusReader, set CatalogSet, kind string) ObjectReports {
	reports, err := status.Read(ctx, set, kind)
	if err != nil {
		logger.Warn("read catalog status", "kind", kind, "error", err)
		return nil
	}
	return reports
}

// objectClusters returns the clusters in reports, sorted, split into those
// that report object name ready, pending or failed, and those whose
// Application doesn't list it (missing).
func objectClusters(reports ObjectReports, name string) (ready, pending, failed, missing []string) {
	for cluster, objects := range reports {
		report, ok := objects[name]
		switch {
		case !ok:
			missing = append(missing, cluster)
		case report.State == StateReady:
			ready = append(ready, cluster)
		case report.State == StateFailed:
			failed = append(failed, cluster)
		default:
			pending = append(pending, cluster)
		}
	}
	slices.Sort(ready)
	slices.Sort(pending)
	slices.Sort(failed)
	slices.Sort(missing)
	return ready, pending, failed, missing
}

// clusterSummary says where an object is ready, for example "Failed on
// site-a; ready on site-b".
func clusterSummary(failed, notReady, ready, noReport []string) string {
	var parts []string
	for _, part := range []struct {
		prefix   string
		clusters []string
	}{{"failed on ", failed}, {"not ready on ", notReady}, {"ready on ", ready}, {"no report from ", noReport}} {
		if len(part.clusters) > 0 {
			parts = append(parts, part.prefix+strings.Join(part.clusters, ", "))
		}
	}
	message := strings.Join(parts, "; ")
	if message == "" {
		return ""
	}
	return strings.ToUpper(message[:1]) + message[1:]
}
