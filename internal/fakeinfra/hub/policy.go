package hub

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The env repository's ai-grid-ready Policy (apps/ai-grid-policies): placed
// on every ManagedCluster labelled ai-grid/enabled=true (Placement
// ai-grid-spokes), Compliant once the cluster's DataScienceCluster is Ready
// and its grid operator runs. The fake computes this root Policy's
// per-cluster status from the clusters. Every other Policy it serves comes as
// is from policies.json.
const (
	policiesPath         = "/apis/policy.open-cluster-management.io/v1/namespaces"
	ReadyPolicyNamespace = "ai-grid-policies"
	ReadyPolicyName      = "ai-grid-ready"

	// A newly enabled cluster is Pending (listed in the Policy's status with
	// no compliance yet) for DefaultEvaluateAfter, until the config-policy
	// controller first evaluates it. It is then NonCompliant, as a real one
	// is while OpenShift AI and the grid install, and turns Compliant
	// DefaultProvisionAfter after it was enabled. Both at speed 1; real
	// installs take about 20 minutes.
	DefaultEvaluateAfter  = 5 * time.Second
	DefaultProvisionAfter = 20 * time.Second

	policyAPIVersion = "policy.open-cluster-management.io/v1"
	policyKind       = "Policy"

	labelEnabled     = "ai-grid/enabled"
	labelProvisioned = "ai-grid/provisioned"
)

var policyGroupResource = schema.GroupResource{Group: "policy.open-cluster-management.io", Resource: "policies"}

// trackEnabled records when a cluster becomes ai-grid/enabled=true, so the
// ready Policy can report it Pending for a while. Must be called under h.mu.
func (h *Hub) trackEnabled(before, after *ManagedCluster) {
	wasEnabled := before != nil && before.Labels[labelEnabled] == "true"
	switch {
	case after.Labels[labelEnabled] != "true":
		delete(h.enabledAt, after.Name)
	case !wasEnabled:
		h.enabledAt[after.Name] = h.clock()
	}
}

// policyCompliance is one cluster's compliance with the ready Policy: the
// fixture's ai-grid/provisioned label when it has one ("true" Compliant,
// anything else NonCompliant), otherwise, after the cluster was enabled,
// Pending ("") until DefaultEvaluateAfter, NonCompliant until
// DefaultProvisionAfter, then Compliant. Must be called under h.mu.
func (h *Hub) policyCompliance(c *ManagedCluster) string {
	if v, ok := c.Labels[labelProvisioned]; ok {
		if v == "true" {
			return "Compliant"
		}
		return "NonCompliant"
	}
	at, ok := h.enabledAt[c.Name]
	if !ok {
		return "Compliant"
	}
	switch since := h.clock().Sub(at); {
	case since < time.Duration(float64(DefaultEvaluateAfter)/h.speed):
		return ""
	case since < time.Duration(float64(DefaultProvisionAfter)/h.speed):
		return "NonCompliant"
	default:
		return "Compliant"
	}
}

func (h *Hub) getPolicy(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	if !h.authorize(w, r, "get", name) {
		return
	}
	if namespace != ReadyPolicyNamespace || name != ReadyPolicyName {
		h.mu.Lock()
		policy, ok := h.policies[namespace+"/"+name]
		h.mu.Unlock()
		if !ok {
			writeStatus(w, apierrors.NewNotFound(policyGroupResource, name))
			return
		}
		writeJSON(w, http.StatusOK, policy.Object)
		return
	}
	h.mu.Lock()
	h.advance()
	names := make([]string, 0, len(h.clusters))
	for n, c := range h.clusters {
		if c.Labels[labelEnabled] == "true" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	entries := make([]map[string]any, 0, len(names))
	for _, n := range names {
		entry := map[string]any{"clustername": n, "clusternamespace": n}
		if compliant := h.policyCompliance(h.clusters[n]); compliant != "" {
			entry["compliant"] = compliant
		}
		entries = append(entries, entry)
	}
	rv := h.rv
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "policy.open-cluster-management.io/v1",
		"kind":       "Policy",
		"metadata": metav1.ObjectMeta{
			Name: ReadyPolicyName, Namespace: ReadyPolicyNamespace, ResourceVersion: strconv.FormatInt(rv, 10),
		},
		"spec":   map[string]any{"remediationAction": "inform", "disabled": false},
		"status": map[string]any{"status": entries},
	})
}

// seedPolicyFixtures stores the fixture Policies by namespace and name. Each
// needs a name and a namespace; a missing resourceVersion becomes "1".
func seedPolicyFixtures(h *Hub, fx *Fixtures) error {
	for i := range fx.Policies {
		p := fx.Policies[i].DeepCopy()
		name, namespace := p.GetName(), p.GetNamespace()
		if name == "" || namespace == "" {
			return fmt.Errorf("fixture Policy %d: metadata.name and metadata.namespace are required", i)
		}
		if namespace == ReadyPolicyNamespace && name == ReadyPolicyName {
			return fmt.Errorf("fixture Policy %s/%s: the hub computes this Policy from the clusters", namespace, name)
		}
		key := namespace + "/" + name
		if _, dup := h.policies[key]; dup {
			return fmt.Errorf("fixture Policy %s listed twice", key)
		}
		if p.GetResourceVersion() == "" {
			p.SetResourceVersion("1")
		}
		h.policies[key] = p
	}
	return nil
}
