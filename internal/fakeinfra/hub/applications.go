package hub

import (
	"fmt"
	"net/http"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Argo CD Applications the fake hub serves as is from applications.json, by
// namespace: the catalog's Applications whose status.resources[] the backend
// reads models' and subscriptions' status from (README, "Status from the
// spokes"). Static: the fixtures say what each cluster reports.
const (
	applicationsPath      = "/apis/argoproj.io/v1alpha1/namespaces"
	applicationAPIVersion = "argoproj.io/v1alpha1"
	applicationKind       = "Application"
	applicationListKind   = "ApplicationList"
)

var applicationGroupResource = schema.GroupResource{Group: "argoproj.io", Resource: "applications"}

func (h *Hub) listApplications(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, "list", "") {
		return
	}
	namespace := r.PathValue("namespace")
	h.mu.Lock()
	names := make([]string, 0, len(h.applications[namespace]))
	for name := range h.applications[namespace] {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]any, 0, len(names))
	for _, name := range names {
		items = append(items, h.applications[namespace][name].Object)
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": applicationAPIVersion,
		"kind":       applicationListKind,
		"metadata":   map[string]any{"resourceVersion": "1"},
		"items":      items,
	})
}

func (h *Hub) getApplication(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if !h.authorize(w, r, "get", name) {
		return
	}
	h.mu.Lock()
	app, ok := h.applications[namespace][name]
	h.mu.Unlock()
	if !ok {
		writeStatus(w, apierrors.NewNotFound(applicationGroupResource, name))
		return
	}
	writeJSON(w, http.StatusOK, app.Object)
}

// seedApplicationFixtures stores the fixture Applications by namespace and
// name. Each needs a name and a namespace; a missing resourceVersion becomes
// "1".
func seedApplicationFixtures(h *Hub, fx *Fixtures) error {
	for i := range fx.Applications {
		app := fx.Applications[i].DeepCopy()
		name, namespace := app.GetName(), app.GetNamespace()
		if name == "" || namespace == "" {
			return fmt.Errorf("fixture Application %d: metadata.name and metadata.namespace are required", i)
		}
		if h.applications[namespace] == nil {
			h.applications[namespace] = map[string]*unstructured.Unstructured{}
		}
		if _, dup := h.applications[namespace][name]; dup {
			return fmt.Errorf("fixture Application %s/%s listed twice", namespace, name)
		}
		if app.GetResourceVersion() == "" {
			app.SetResourceVersion("1")
		}
		h.applications[namespace][name] = app
	}
	return nil
}
