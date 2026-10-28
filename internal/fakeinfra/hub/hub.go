package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const (
	managedClustersPath = "/apis/" + Group + "/" + Version + "/" + Resource
	ssarPath            = "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	// maxRequestBodyBytes is the API server's default request body limit
	// (apiserver pkg/server/config.go MaxRequestBodyBytes, 3 MiB).
	maxRequestBodyBytes = 3 << 20
	// Messages the API server writes verbatim.
	msgObjectModified = "the object has been modified; please apply your changes to the latest version and try again"
	msgRVOnCreate     = "resourceVersion should not be set on objects to be created"
	msgPatchMedia     = "the body of the request was in an unknown format - accepted media types include: application/json-patch+json, application/merge-patch+json, application/apply-patch+yaml"
)

// Hub is the fake hub API server. It is safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	clusters map[string]*ManagedCluster
	maas     map[string]map[string]*unstructured.Unstructured // resource -> ns/name -> object
	policies map[string]*unstructured.Unstructured            // ns/name -> fixture Policy
	// applications are the fixture Argo CD Applications, by namespace and name.
	applications map[string]map[string]*unstructured.Unstructured
	rv           int64
	scenarios    []*scenario
	detaches     map[string]time.Time
	enabledAt    map[string]time.Time
	access       Access
	speed        float64
	detachAfter  time.Duration
	start        time.Time
	clock        func() time.Time
	logger       *slog.Logger
	// nodes are what the fake Search indexes (search.go).
	nodes []SearchNode
}

// Option configures a Hub.
type Option func(*Hub)

// WithClock replaces time.Now, for tests. The scenario clock starts at the
// clock's first reading.
func WithClock(clock func() time.Time) Option { return func(h *Hub) { h.clock = clock } }

// WithLogger sets the logger (default slog.Default()).
func WithLogger(logger *slog.Logger) Option { return func(h *Hub) { h.logger = logger } }

// New builds a hub seeded from fixtures. Fixture objects are validated the
// way the API server validates object metadata; the global resourceVersion
// counter continues from the highest fixture resourceVersion.
func New(fx *Fixtures, opts ...Option) (*Hub, error) {
	h := &Hub{
		clusters:     map[string]*ManagedCluster{},
		maas:         map[string]map[string]*unstructured.Unstructured{},
		policies:     map[string]*unstructured.Unstructured{},
		applications: map[string]map[string]*unstructured.Unstructured{},
		detaches:     map[string]time.Time{},
		enabledAt:    map[string]time.Time{},
		access:       fx.Access,
		speed:        1,
		detachAfter:  DefaultDetachAfter,
		clock:        time.Now,
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	if s := fx.Settings.Speed; s != nil {
		if *s <= 0 {
			return nil, fmt.Errorf("speed must be greater than zero, got %v", *s)
		}
		h.speed = *s
	}
	if d := fx.Settings.DetachAfter; d != nil {
		h.detachAfter = d.Duration()
	}
	if h.access.ServiceAccount.Token == "" {
		h.access.ServiceAccount.Token = DefaultServiceAccountToken
	}
	if h.access.ServiceAccount.Username == "" {
		h.access.ServiceAccount.Username = DefaultServiceAccountUsername
	}
	for i := range fx.Clusters {
		c := fx.Clusters[i].DeepCopy()
		c.APIVersion, c.Kind = APIVersion, Kind
		if errs := apivalidation.ValidateObjectMeta(&c.ObjectMeta, false, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata")); len(errs) > 0 {
			return nil, fmt.Errorf("fixture ManagedCluster %q: %w", c.Name, errs.ToAggregate())
		}
		if _, dup := h.clusters[c.Name]; dup {
			return nil, fmt.Errorf("fixture ManagedCluster %q listed twice", c.Name)
		}
		rv, err := strconv.ParseInt(c.ResourceVersion, 10, 64)
		if err != nil || rv < 1 {
			return nil, fmt.Errorf("fixture ManagedCluster %q: resourceVersion %q is not a positive integer", c.Name, c.ResourceVersion)
		}
		h.rv = max(h.rv, rv)
		if c.UID == "" || c.CreationTimestamp.IsZero() || c.Generation < 1 {
			return nil, fmt.Errorf("fixture ManagedCluster %q: uid, creationTimestamp and generation are required", c.Name)
		}
		h.clusters[c.Name] = c
	}
	for _, n := range fx.SearchNodes {
		if n.Cluster == "" || n.Name == "" {
			return nil, fmt.Errorf("%s: every node needs a cluster and a name, got %q/%q", SearchNodesFile, n.Cluster, n.Name)
		}
		if _, ok := h.clusters[n.Cluster]; !ok {
			return nil, fmt.Errorf("%s: node %q names cluster %q, which no fixture ManagedCluster has", SearchNodesFile, n.Name, n.Cluster)
		}
		h.nodes = append(h.nodes, n)
	}
	scenarios, err := parseScenarios(fx.Scenarios, h.clusters)
	if err != nil {
		return nil, err
	}
	h.scenarios = scenarios
	if err := seedMAASFixtures(h, fx); err != nil {
		return nil, err
	}
	if err := seedApplicationFixtures(h, fx); err != nil {
		return nil, err
	}
	if err := seedPolicyFixtures(h, fx); err != nil {
		return nil, err
	}
	h.start = h.clock()
	return h, nil
}

// ServiceAccountToken is the bearer token the backend's kubeconfig must carry.
func (h *Hub) ServiceAccountToken() string { return h.access.ServiceAccount.Token }

// Handler returns the HTTP handler.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ssarPath, h.selfSubjectAccessReview)
	mux.HandleFunc("GET "+managedClustersPath, h.list)
	mux.HandleFunc("POST "+managedClustersPath, h.create)
	mux.HandleFunc("DELETE "+managedClustersPath, h.unsupported("deletecollection"))
	mux.HandleFunc("GET "+managedClustersPath+"/{name}", h.get)
	mux.HandleFunc("PUT "+managedClustersPath+"/{name}", h.update)
	mux.HandleFunc("PATCH "+managedClustersPath+"/{name}", h.patch)
	mux.HandleFunc("DELETE "+managedClustersPath+"/{name}", h.delete)
	mux.HandleFunc("GET "+policiesPath+"/{namespace}/policies/{name}", h.getPolicy)
	mux.HandleFunc("GET "+applicationsPath+"/{namespace}/applications", h.listApplications)
	mux.HandleFunc("GET "+applicationsPath+"/{namespace}/applications/{name}", h.getApplication)
	h.registerMAAS(mux)
	h.registerModels(mux)
	mux.HandleFunc("POST "+SearchPath, h.search)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.authenticate(w, r); !ok {
			return
		}
		// What the API server answers for a path it does not serve
		// (the fake serves no discovery, no other resource and no
		// subresource).
		writeStatus(w, &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Code: http.StatusNotFound, Reason: metav1.StatusReasonNotFound,
			Message: "the server could not find the requested resource", Details: &metav1.StatusDetails{},
		}})
	})
	return mux
}

// authenticate checks the bearer token like the API server's token
// authenticator: no token, or a token access.json rejects, is 401
// Unauthorized. It returns the user name the token stands for.
func (h *Hub) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || slices.Contains(h.access.RejectedTokens, token) {
		writeStatus(w, apierrors.NewUnauthorized("Unauthorized"))
		return "", false
	}
	return h.username(token), true
}

// username is the user a token stands for: the ServiceAccount's name for its
// token, "dev-user" for any other (the console fake's SelfSubjectAccessReview
// names every console user so; the fake hub has no other user database).
func (h *Hub) username(token string) string {
	if token == h.access.ServiceAccount.Token {
		return h.access.ServiceAccount.Username
	}
	return "dev-user"
}

// allowed is the one RBAC decision of the fake hub, used both by
// SelfSubjectAccessReview and by the managedclusters authorizer, so the two
// always agree: a denied token is refused, the ServiceAccount is allowed,
// every other token follows defaultAllowed. Verb and resource do not matter.
func (h *Hub) allowed(token string) bool {
	if slices.Contains(h.access.DeniedTokens, token) {
		return false
	}
	return h.access.DefaultAllowed || token == h.access.ServiceAccount.Token
}

// authorize is RBAC for managedclusters as access.json decides it: denied
// tokens get the API server's 403 for any verb, everyone else is allowed.
func (h *Hub) authorize(w http.ResponseWriter, r *http.Request, verb, name string) bool {
	user, ok := h.authenticate(w, r)
	if !ok {
		return false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !h.allowed(token) {
		writeStatus(w, apierrors.NewForbidden(groupResource, name,
			verbatim(fmt.Sprintf("User %q cannot %s resource %q in API group %q at the cluster scope", user, verb, Resource, Group))))
		return false
	}
	return true
}

func (h *Hub) unsupported(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.authorize(w, r, action, "") {
			writeStatus(w, apierrors.NewMethodNotSupported(groupResource, action))
		}
	}
}

func (h *Hub) list(w http.ResponseWriter, r *http.Request) {
	verb := "list"
	if isTrueParam(r.URL.Query().Get("watch")) {
		verb = "watch"
	}
	if !h.authorize(w, r, verb, "") {
		return
	}
	if verb == "watch" {
		writeStatus(w, apierrors.NewMethodNotSupported(groupResource, "watch"))
		return
	}
	selector := labels.Everything()
	if s := r.URL.Query().Get("labelSelector"); s != "" {
		parsed, err := labels.Parse(s)
		if err != nil {
			writeStatus(w, apierrors.NewBadRequest(err.Error()))
			return
		}
		selector = parsed
	}
	h.mu.Lock()
	h.advance()
	items := make([]ManagedCluster, 0, len(h.clusters))
	for _, c := range h.clusters {
		if selector.Matches(labels.Set(c.Labels)) {
			items = append(items, *c.DeepCopy())
		}
	}
	rv := h.rv
	h.mu.Unlock()
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	writeJSON(w, http.StatusOK, &ManagedClusterList{
		TypeMeta: metav1.TypeMeta{APIVersion: APIVersion, Kind: ListKind},
		ListMeta: metav1.ListMeta{ResourceVersion: strconv.FormatInt(rv, 10)},
		Items:    items,
	})
}

func (h *Hub) get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.authorize(w, r, "get", name) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance()
	c, ok := h.clusters[name]
	if !ok {
		writeStatus(w, apierrors.NewNotFound(groupResource, name))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// create follows the API server's create path for a custom resource with a
// status subresource: decode (CRD defaults applied), mutating admission (the
// OCM webhook, see admit), PrepareForCreate (status dropped, generation 1),
// system fields (uid, creationTimestamp), validation, then storage
// (resourceVersion, AlreadyExists). The hub controllers then react with
// their own write (see reconcile).
func (h *Hub) create(w http.ResponseWriter, r *http.Request) {
	if !h.authorize(w, r, "create", "") {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	c, statusErr := decodeObject(body, "")
	if statusErr != nil {
		writeStatus(w, statusErr)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance()
	now := h.now()
	admit(c, nil, now)
	c.DeletionTimestamp = nil
	c.DeletionGracePeriodSeconds = nil
	c.Status = nil
	c.Generation = 1
	if c.Name == "" && c.GenerateName != "" {
		c.Name = c.GenerateName + utilrand.String(5)
	}
	c.UID = types.UID(newUID())
	c.CreationTimestamp = metav1.NewTime(now)
	if errs := apivalidation.ValidateObjectMeta(&c.ObjectMeta, false, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata")); len(errs) > 0 {
		writeStatus(w, apierrors.NewInvalid(groupKind, c.Name, errs))
		return
	}
	if c.ResourceVersion != "" {
		writeStatus(w, apierrors.NewInternalError(errors.New(msgRVOnCreate)))
		return
	}
	if _, exists := h.clusters[c.Name]; exists {
		writeStatus(w, apierrors.NewAlreadyExists(groupResource, c.Name))
		return
	}
	stored, _ := h.commit(nil, c)
	h.logger.Debug("managedcluster created", "name", c.Name, "resourceVersion", stored.ResourceVersion)
	writeJSON(w, http.StatusCreated, stored)
	h.reconcileAfterWrite(stored, nil)
}

func (h *Hub) update(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.authorize(w, r, "update", name) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	next, statusErr := decodeObject(body, name)
	if statusErr != nil {
		writeStatus(w, statusErr)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance()
	cur, ok := h.clusters[name]
	if !ok {
		// Custom resources do not allow create on update.
		writeStatus(w, apierrors.NewNotFound(groupResource, name))
		return
	}
	h.writeUpdate(w, cur, next, name)
}

// patch applies an RFC 7386 merge patch or an RFC 6902 JSON patch to the
// stored object and stores the result through the update path, so a patch
// that carries a stale metadata.resourceVersion is a Conflict, exactly as on
// the API server.
func (h *Hub) patch(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.authorize(w, r, "patch", name) {
		return
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/merge-patch+json" && mediaType != "application/json-patch+json" {
		writeStatus(w, &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Code: http.StatusUnsupportedMediaType,
			Reason: metav1.StatusReasonUnsupportedMediaType, Message: msgPatchMedia,
		}})
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance()
	cur, ok := h.clusters[name]
	if !ok {
		writeStatus(w, apierrors.NewNotFound(groupResource, name))
		return
	}
	original, err := json.Marshal(cur)
	if err != nil {
		writeStatus(w, apierrors.NewInternalError(err))
		return
	}
	patched, statusErr := applyPatch(mediaType, original, body)
	if statusErr != nil {
		writeStatus(w, statusErr)
		return
	}
	next, statusErr := decodeObject(patched, name)
	if statusErr != nil {
		writeStatus(w, statusErr)
		return
	}
	h.writeUpdate(w, cur, next, name)
}

// maxJSONPatchOperations is the API server's limit on JSON patch operations
// (apiserver pkg/endpoints/handlers/patch.go).
const maxJSONPatchOperations = 10000

// applyPatch is apiserver pkg/endpoints/handlers/patch.go applyJSPatch: a
// JSON patch that does not decode is 400 BadRequest, one with too many
// operations 413, and one that decodes but does not apply (a failed `test`,
// a missing path) 422 from NewGenericServerResponse; a merge patch that is
// not valid JSON (ErrBadJSONPatch) is 400, any other merge failure is
// returned unwrapped, which the API server reports as 500.
func applyPatch(mediaType string, original, body []byte) ([]byte, *apierrors.StatusError) {
	if mediaType == "application/merge-patch+json" {
		patched, err := jsonpatch.MergePatch(original, body)
		switch {
		case errors.Is(err, jsonpatch.ErrBadJSONPatch):
			return nil, apierrors.NewBadRequest(err.Error())
		case err != nil:
			return nil, apierrors.NewInternalError(err)
		}
		return patched, nil
	}
	p, err := jsonpatch.DecodePatch(body)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if len(p) > maxJSONPatchOperations {
		return nil, apierrors.NewRequestEntityTooLargeError(fmt.Sprintf("The allowed maximum operations in a JSON patch is %d, got %d", maxJSONPatchOperations, len(p)))
	}
	patched, err := p.Apply(original)
	if err != nil {
		return nil, apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", err.Error(), 0, false)
	}
	return patched, nil
}

// writeUpdate is the API server's update path for a custom resource:
// name check, optimistic concurrency on resourceVersion, mutating admission,
// rest.BeforeUpdate (generation, uid, creation and deletion fields kept from
// the stored object; status kept because it has its own subresource),
// validation, then a write only when the object changed. Must be called
// under h.mu.
func (h *Hub) writeUpdate(w http.ResponseWriter, cur, next *ManagedCluster, name string) {
	if next.Name != name {
		writeStatus(w, apierrors.NewBadRequest(fmt.Sprintf("the name of the object (%s) does not match the name on the URL (%s)", next.Name, name)))
		return
	}
	if next.ResourceVersion == "" {
		// Custom resources do not allow unconditional updates; the API
		// server reports the parsed version, a uint64 zero.
		writeStatus(w, apierrors.NewInvalid(groupKind, name, field.ErrorList{
			field.Invalid(field.NewPath("metadata", "resourceVersion"), uint64(0), "must be specified for an update"),
		}))
		return
	}
	if next.ResourceVersion != cur.ResourceVersion {
		writeStatus(w, apierrors.NewConflict(groupResource, name, errors.New(msgObjectModified)))
		return
	}
	now := h.now()
	admit(next, cur, now)
	next.Generation = cur.Generation
	if next.UID == "" {
		next.UID = cur.UID
	}
	next.CreationTimestamp = cur.CreationTimestamp
	if cur.DeletionTimestamp != nil {
		next.DeletionTimestamp = cur.DeletionTimestamp
		next.DeletionGracePeriodSeconds = cur.DeletionGracePeriodSeconds
	}
	next.Status = cur.Status
	errs := apivalidation.ValidateObjectMetaUpdate(&next.ObjectMeta, &cur.ObjectMeta, field.NewPath("metadata"))
	errs = append(errs, apivalidation.ValidateObjectMeta(&next.ObjectMeta, false, apivalidation.NameIsDNSSubdomain, field.NewPath("metadata"))...)
	if len(errs) > 0 {
		writeStatus(w, apierrors.NewInvalid(groupKind, name, errs))
		return
	}
	before := cur.DeepCopy()
	stored, wrote := h.commit(cur, next)
	if stored == nil {
		// The last finalizer was removed from a terminating object: the API
		// server deletes it and answers with the object as last written.
		writeJSON(w, http.StatusOK, next)
		return
	}
	h.logger.Debug("managedcluster updated", "name", name, "changed", wrote, "resourceVersion", stored.ResourceVersion)
	writeJSON(w, http.StatusOK, stored)
	if wrote {
		h.reconcileAfterWrite(stored, before)
	}
}

// deleteOptions is the part of metav1.DeleteOptions the fake honours.
type deleteOptions struct {
	Preconditions *metav1.Preconditions `json:"preconditions,omitempty"`
}

// delete follows the registry store: preconditions first; an object without
// finalizers is removed and answered with a Success Status; an object with
// finalizers is marked deleting (deletionTimestamp, deletionGracePeriodSeconds
// 0, generation bump) and answered with the object itself. The hub's
// controllers then write their detach report and remove their finalizers
// after detachAfter.
func (h *Hub) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.authorize(w, r, "delete", name) {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var opts deleteOptions
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &opts); err != nil {
			writeStatus(w, apierrors.NewBadRequest(err.Error()))
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.advance()
	cur, ok := h.clusters[name]
	if !ok {
		writeStatus(w, apierrors.NewNotFound(groupResource, name))
		return
	}
	if p := opts.Preconditions; p != nil {
		if p.UID != nil && *p.UID != cur.UID {
			writeStatus(w, apierrors.NewConflict(groupResource, name, verbatim(fmt.Sprintf("Precondition failed: UID in precondition: %v, UID in object meta: %v", *p.UID, cur.UID))))
			return
		}
		if p.ResourceVersion != nil && *p.ResourceVersion != cur.ResourceVersion {
			writeStatus(w, apierrors.NewConflict(groupResource, name, verbatim(fmt.Sprintf("Precondition failed: ResourceVersion in precondition: %v, ResourceVersion in object meta: %v", *p.ResourceVersion, cur.ResourceVersion))))
			return
		}
	}
	if len(cur.Finalizers) == 0 {
		uid := cur.UID
		h.removeObject(cur)
		h.logger.Debug("managedcluster deleted", "name", name)
		writeJSON(w, http.StatusOK, &metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusSuccess,
			Details:  &metav1.StatusDetails{Name: name, Group: Group, Kind: Resource, UID: uid},
		})
		return
	}
	if terminating(cur) {
		writeJSON(w, http.StatusOK, cur)
		return
	}
	now := h.now()
	next := cur.DeepCopy()
	ts := metav1.NewTime(now)
	next.DeletionTimestamp = &ts
	var zero int64
	next.DeletionGracePeriodSeconds = &zero
	stored, _ := h.commit(cur, next)
	h.logger.Debug("managedcluster deletion requested", "name", name, "finalizers", len(stored.Finalizers))
	writeJSON(w, http.StatusOK, stored)
	detaching := stored.DeepCopy()
	markDetaching(detaching, now)
	h.commit(stored, detaching)
	h.detaches[name] = now
}

// reconcileAfterWrite runs the hub controllers on a live object a client just
// wrote and stores what they change as their own write. Must be called under
// h.mu.
func (h *Hub) reconcileAfterWrite(stored, before *ManagedCluster) {
	next := stored.DeepCopy()
	reconcile(next, before, h.now())
	h.commit(stored, next)
}

// commit stores next as the new state of cur (nil for a new object) the way
// the API server writes: nothing is written when the object would not change
// (the stored object comes back with its resourceVersion), generation moves
// when spec changes or deletion is first requested, every write takes the
// next value of the global resourceVersion counter, and a terminating object
// without finalizers is deleted. It returns the stored object (nil when it
// was deleted) and whether it wrote. Must be called under h.mu.
func (h *Hub) commit(cur, next *ManagedCluster) (*ManagedCluster, bool) {
	next.APIVersion, next.Kind = APIVersion, Kind
	if cur != nil {
		next.ResourceVersion = cur.ResourceVersion
		if !jsonEqual(cur.Spec, next.Spec) || (cur.DeletionTimestamp == nil && next.DeletionTimestamp != nil) {
			next.Generation = cur.Generation + 1
		}
		if jsonEqual(cur, next) {
			return cur, false
		}
	}
	if next.DeletionTimestamp != nil && len(next.Finalizers) == 0 {
		h.removeObject(next)
		return nil, true
	}
	next.ResourceVersion = h.nextResourceVersion()
	stored := next.DeepCopy()
	h.clusters[stored.Name] = stored
	h.trackEnabled(cur, stored)
	return stored, true
}

func (h *Hub) nextResourceVersion() string {
	h.rv++
	return strconv.FormatInt(h.rv, 10)
}

// now is the clock truncated to the second, the precision of metav1.Time on
// the wire.
func (h *Hub) now() time.Time { return h.clock().UTC().Truncate(time.Second) }

// decodeObject decodes a request body (or a patched object) as the API server
// does for this CRD: apiVersion and kind must be present and right, unknown
// fields are dropped (structural pruning), a value of the wrong JSON type is
// 422 Invalid with the schema validator's message, and the CRD default of
// spec.leaseDurationSeconds is applied.
func decodeObject(body []byte, name string) (*ManagedCluster, *apierrors.StatusError) {
	var typeMeta metav1.TypeMeta
	if err := json.Unmarshal(body, &typeMeta); err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	switch {
	case typeMeta.Kind == "":
		return nil, apierrors.NewBadRequest(fmt.Sprintf("Object 'Kind' is missing in '%s'", body))
	case typeMeta.APIVersion != APIVersion:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the API version in the data (%s) does not match the expected API version (%s)", typeMeta.APIVersion, APIVersion))
	case typeMeta.Kind != Kind:
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the kind in the data (%s) does not match the expected kind (%s)", typeMeta.Kind, Kind))
	}
	c := &ManagedCluster{}
	if err := json.Unmarshal(body, c); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			if name == "" {
				name = c.Name
			}
			path := typeErr.Field
			return nil, apierrors.NewInvalid(groupKind, name, field.ErrorList{
				field.Invalid(field.NewPath(path), typeErr.Value, fmt.Sprintf("%s in body must be of type %s: %q", path, openAPIType(typeErr), typeErr.Value)),
			})
		}
		return nil, apierrors.NewBadRequest(err.Error())
	}
	if c.Spec.LeaseDurationSeconds == nil {
		d := DefaultLeaseDurationSeconds
		c.Spec.LeaseDurationSeconds = &d
	}
	return c, nil
}

// openAPIType names the Go type json wanted the way the CRD schema does.
func openAPIType(e *json.UnmarshalTypeError) string {
	switch e.Type.Kind().String() {
	case "bool":
		return "boolean"
	case "string":
		return "string"
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
		return "integer"
	case "float32", "float64":
		return "number"
	case "slice", "array":
		return "array"
	default:
		return "object"
	}
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeStatus(w, apierrors.NewBadRequest(err.Error()))
		return nil, false
	}
	if len(body) > maxRequestBodyBytes {
		writeStatus(w, apierrors.NewRequestEntityTooLargeError(fmt.Sprintf("limit is %d", maxRequestBodyBytes)))
		return nil, false
	}
	return body, true
}

// verbatim is an error whose text is an API server message quoted exactly,
// capital letter included.
type verbatim string

func (v verbatim) Error() string { return string(v) }

func isTrueParam(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// writeStatus writes a metav1.Status the way the API server does.
func writeStatus(w http.ResponseWriter, err *apierrors.StatusError) {
	st := err.ErrStatus
	st.Kind, st.APIVersion = "Status", "v1"
	writeJSON(w, int(st.Code), &st)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// newUID returns a random version 4 UUID, which is what the API server
// assigns as metadata.uid.
func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
