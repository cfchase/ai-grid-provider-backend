package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Fixture files the hub reads from a fixtures directory (fixtures/README.md
// describes each one). A missing file leaves its part empty or at its
// default; the directory itself must exist.
const (
	ManagedClustersFile      = "managedclusters.json"
	AccessFile               = "access.json"
	ScenariosFile            = "scenarios.json"
	SettingsFile             = "settings.json"
	MaaSSubscriptionsFile    = "maassubscriptions.json"
	MaaSAuthPoliciesFile     = "maasauthpolicies.json"
	MaaSModelRefsFile        = "maasmodelrefs.json"
	LLMInferenceServicesFile = "llminferenceservices.json"
	PoliciesFile             = "policies.json"
	ApplicationsFile         = "applications.json"
	SearchNodesFile          = "searchnodes.json"
)

// Defaults for the backend's ServiceAccount when access.json names none: the
// identity the Kind deployment gives the backend (deploy/kind/ai-grid-provider-backend.yaml,
// ServiceAccount ai-grid-provider-backend in namespace ai-grid) and a token
// that is obviously fake.
const (
	DefaultServiceAccountToken    = "fake-ai-grid-provider-backend-sa-token"
	DefaultServiceAccountUsername = "system:serviceaccount:ai-grid:ai-grid-provider-backend"
	// DefaultDetachAfter is how long a ManagedCluster deleted through the API
	// stays detaching before the fake's hub controllers have drained it and
	// removed their finalizers. The hub takes "tens of seconds to minutes"
	// (the console's mock/research/acm.md); 30s is the offset at which the
	// shipped scenario lets edge-lyon-2 finish detaching.
	DefaultDetachAfter = 30 * time.Second
)

// Fixtures is the content of a fixtures directory, as far as the hub uses it.
type Fixtures struct {
	// Clusters are the ManagedCluster CRs the hub starts with.
	Clusters []ManagedCluster
	Access   Access
	Settings Settings
	// Scenarios are the timed transitions applied to Clusters.
	Scenarios []ScenarioEvent
	// MaaS CRs for Subscriptions (optional; missing files leave empty stores).
	MaaSSubscriptions []unstructured.Unstructured
	MaaSAuthPolicies  []unstructured.Unstructured
	MaaSModelRefs     []unstructured.Unstructured
	// LLMInferenceService CRs for Models (optional; missing means empty List).
	LLMInferenceServices []unstructured.Unstructured
	// Policies are ACM Policies the hub serves as is by namespace and name
	// (optional). The ai-grid-ready Policy is computed from the clusters
	// instead (policy.go).
	Policies []unstructured.Unstructured
	// Applications are Argo CD Applications the hub serves by namespace: the
	// catalog's, whose status.resources[] the backend reads status from.
	Applications []unstructured.Unstructured
	// SearchNodes are the nodes the fake ACM Search indexes (search.go).
	SearchNodes []SearchNode
}

// Access decides who the fake hub accepts. It is the console fake's
// access.json plus the backend's ServiceAccount.
type Access struct {
	Comment string `json:"_comment,omitempty"`
	// DefaultAllowed is the answer for any token not listed below: allowed
	// in a SelfSubjectAccessReview, and allowed to use managedclusters.
	DefaultAllowed bool `json:"defaultAllowed"`
	// DeniedTokens get allowed: false in a SelfSubjectAccessReview and 403
	// on managedclusters, like a user without the ClusterRole.
	DeniedTokens []string `json:"deniedTokens"`
	// RejectedTokens get 401, like an expired or revoked OpenShift token.
	RejectedTokens []string `json:"rejectedTokens"`
	// ServiceAccount is the identity the backend's kubeconfig carries. Its
	// token is allowed even when DefaultAllowed is false, unless it is also
	// listed as denied or rejected.
	ServiceAccount ServiceAccount `json:"serviceAccount"`
}

// ServiceAccount is the backend's identity on the fake hub.
type ServiceAccount struct {
	Token    string `json:"token"`
	Username string `json:"username"`
}

// Settings are the timing knobs.
type Settings struct {
	Comment string `json:"_comment,omitempty"`
	// Speed multiplies the scenario clock; 2 fires a "20s" event after 10s.
	// Nil means 1.
	Speed *float64 `json:"speed,omitempty"`
	// DetachAfter overrides DefaultDetachAfter (also divided by Speed).
	DetachAfter *Duration `json:"detachAfter,omitempty"`
}

// Duration is a time.Duration that encodes as a Go duration string ("80ms").
type Duration time.Duration

// Duration returns the wrapped value.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// MarshalJSON writes a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON accepts a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// LoadFixtures reads the hub's files from dir. Every file is decoded
// strictly, so a misspelled key is an error instead of a silently different
// fake.
func LoadFixtures(dir string) (*Fixtures, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("fixtures directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fixtures directory %s is not a directory", dir)
	}
	fx := &Fixtures{Access: Access{DefaultAllowed: true}}

	var list ManagedClusterList
	if ok, err := readStrict(filepath.Join(dir, ManagedClustersFile), &list); err != nil {
		return nil, err
	} else if ok {
		if list.APIVersion != APIVersion || list.Kind != ListKind {
			return nil, fmt.Errorf("%s: want apiVersion %s and kind %s, got %q and %q", ManagedClustersFile, APIVersion, ListKind, list.APIVersion, list.Kind)
		}
		fx.Clusters = list.Items
	}
	if _, err := readStrict(filepath.Join(dir, AccessFile), &fx.Access); err != nil {
		return nil, err
	}
	if _, err := readStrict(filepath.Join(dir, SettingsFile), &fx.Settings); err != nil {
		return nil, err
	}
	if s := fx.Settings.Speed; s != nil && *s <= 0 {
		return nil, fmt.Errorf("%s: speed must be greater than zero, got %v", SettingsFile, *s)
	}
	var scenarios struct {
		Comment string          `json:"_comment,omitempty"`
		Events  []ScenarioEvent `json:"events"`
	}
	if _, err := readStrict(filepath.Join(dir, ScenariosFile), &scenarios); err != nil {
		return nil, err
	}
	fx.Scenarios = scenarios.Events

	if items, err := loadMAASList(filepath.Join(dir, MaaSSubscriptionsFile), maasSubscriptionKind); err != nil {
		return nil, err
	} else {
		fx.MaaSSubscriptions = items
	}
	if items, err := loadMAASList(filepath.Join(dir, MaaSAuthPoliciesFile), maasAuthPolicyKind); err != nil {
		return nil, err
	} else {
		fx.MaaSAuthPolicies = items
	}
	if items, err := loadMAASList(filepath.Join(dir, MaaSModelRefsFile), maasModelRefKind); err != nil {
		return nil, err
	} else {
		fx.MaaSModelRefs = items
	}
	if items, err := loadUnstructuredList(filepath.Join(dir, LLMInferenceServicesFile), llmInferenceAPIVersion, llmInferenceKind); err != nil {
		return nil, err
	} else {
		fx.LLMInferenceServices = items
	}
	if items, err := loadUnstructuredList(filepath.Join(dir, PoliciesFile), policyAPIVersion, policyKind); err != nil {
		return nil, err
	} else {
		fx.Policies = items
	}
	if items, err := loadUnstructuredList(filepath.Join(dir, ApplicationsFile), applicationAPIVersion, applicationKind); err != nil {
		return nil, err
	} else {
		fx.Applications = items
	}
	var nodes struct {
		Comment string       `json:"_comment,omitempty"`
		Items   []SearchNode `json:"items"`
	}
	if _, err := readStrict(filepath.Join(dir, SearchNodesFile), &nodes); err != nil {
		return nil, err
	}
	fx.SearchNodes = nodes.Items
	return fx, nil
}

func loadMAASList(path, kind string) ([]unstructured.Unstructured, error) {
	return loadUnstructuredList(path, maasAPIVersion, kind)
}

func loadUnstructuredList(path, apiVersion, kind string) ([]unstructured.Unstructured, error) {
	var envelope struct {
		Comment    string                      `json:"_comment,omitempty"`
		APIVersion string                      `json:"apiVersion"`
		Kind       string                      `json:"kind"`
		Metadata   map[string]any              `json:"metadata"`
		Items      []unstructured.Unstructured `json:"items"`
	}
	ok, err := readStrict(path, &envelope)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	wantKind := kind + "List"
	if envelope.APIVersion != apiVersion || envelope.Kind != wantKind {
		return nil, fmt.Errorf("%s: want apiVersion %s and kind %s, got %q and %q",
			filepath.Base(path), apiVersion, wantKind, envelope.APIVersion, envelope.Kind)
	}
	return envelope.Items, nil
}

// readStrict decodes path into v, rejecting unknown fields and trailing data.
// It reports false, without error, when the file does not exist.
func readStrict(path string, v any) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if dec.More() {
		return false, fmt.Errorf("%s: unexpected data after the JSON document", filepath.Base(path))
	}
	return true, nil
}
