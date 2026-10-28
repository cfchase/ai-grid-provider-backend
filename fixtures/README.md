# Fake infrastructure fixtures

What the fake hub and the fake Keycloak (`internal/fakeinfra`, started by
`make dev-fake` and `make dev-fake-poc`) start with. Every name, id, host and
number in these files is fictional.

## Source

Converted from the console repository
[ai-grid-cloud/ai-grid-provider-console](https://github.com/ai-grid-cloud/ai-grid-provider-console)
at commit `537df38`:

| Directory here | Console directory | Preset |
|---|---|---|
| `default/` | `mock/fixtures/` | Ten clusters in every state the ACM console distinguishes, six organizations, a timeline of cluster events. |
| `poc/` | `mock/fixtures-poc/` | The PoC's four sites (`site1`-`site4`) and three organizations; no timeline, so nothing changes over time. Also seeds the same hand-authored MaaS / LLMInferenceService CRs as `default/` so `make dev-fake-poc` exercises Models and Subscriptions. |

`locations/` is **hand-written, not converted**: one ready cluster per edge case of
`status.location` (README, "Cluster locations"); `make dev-fake-locations` serves it.
`hack/convert-console-fixtures` doesn't touch it. Each cluster has only what the location,
state and membership code reads (the `ai-grid/*` annotations, the `ai-grid/enabled` label,
the platform and region claims, and the Accepted, Joined and Available conditions) plus what
the fake hub needs to load it and leave it settled: `uid`, `resourceVersion`, `generation`
and `creationTimestamp` (`LoadFixtures` requires them), and the `name` label, the
`open-cluster-management/created-via` annotation, the finalizers and the
`ManagedClusterImportSucceeded=True` condition (the fake's import controller adds them to a
cluster without them). No capacity, version or other claims.

| Cluster | Annotations | Claims |
|---|---|---|
| `loc-annotations` | latitude, longitude and site name | on-premises (`BareMetal`, no region) |
| `loc-whitespace` | the same, with surrounding spaces, tabs and newlines | on-premises |
| `loc-null-island` | `0`, `0` | on-premises |
| `loc-onprem-none` | none | on-premises |
| `loc-site-name-only` | site name only | on-premises |
| `loc-latitude-only` | latitude only | on-premises |
| `loc-decimal-comma` | `40,7128`, `-74,0060` | on-premises |
| `loc-hemisphere` | `40.7128N`, `74.0060W` | on-premises |
| `loc-dms` | degrees, minutes and seconds | on-premises |
| `loc-nan-inf` | `NaN`, `Inf` | on-premises |
| `loc-out-of-range` | `95`, `200` | on-premises |
| `loc-swapped` | `151.2093`, `-33.8688` (Sydney, swapped) and a site name | on-premises |
| `loc-invalid-in-region` | `39,96`, `-83.00` | AWS `us-east-2` |
| `loc-annotations-in-region` | valid coordinates and a site name | AWS `us-east-2` |
| `loc-aws-region` | none | AWS `us-east-2` |
| `loc-aws-region-named` | site name only | AWS `eu-central-1` |
| `loc-azure-region` | none | Azure `westeurope` |
| `loc-gcp-region` | none | GCP `asia-northeast1` |
| `loc-unknown-region` | none | AWS `mars-north-1`, a region no provider has |

The tests (`internal/fakeinfra`, `TestRealClustersServerLocationsOnFakeHub`) read each one
through the real Clusters server.

`draft/` is different: the draft fake's fixtures, the console's protojson of its draft API
copied **verbatim, not converted**. `hack/convert-console-fixtures` and everything below about
the conversion apply only to `default/` and `poc/`. See [`draft/README.md`](draft/README.md).

The console's fixtures are protojson of its draft `ai_grid.v1` proto, which
mirrors the hub's `ManagedCluster` and Keycloak's Organization field by
field. The files here are the real objects again, so the real backend reads
them through the real Kubernetes and Keycloak clients.

## Files

| File | Read by | Shape | From the console's |
|---|---|---|---|
| `managedclusters.json` | fake hub | A `ManagedClusterList` (`cluster.open-cluster-management.io/v1`) of full CRs: `metadata` (name, uid, resourceVersion, generation, creationTimestamp, deletionTimestamp, labels, annotations, finalizers), `spec` (managedClusterClientConfigs, hubAcceptsClient, leaseDurationSeconds, taints), `status` (conditions, capacity, allocatable, version, clusterClaims). | `managedclusters.json` |
| `organizations.json` | fake Keycloak | A JSON array of Keycloak 26.8 `OrganizationRepresentation`, as `GET /admin/realms/ai-grid/organizations?briefRepresentation=false` returns them: id, name, alias, enabled, description, attributes (Keycloak's map of string arrays), domains (`name`, `verified`, `autoRedirect`). | `tenants.json` |
| `access.json` | fake hub | Which bearer tokens the hub rejects (401), denies (SelfSubjectAccessReview `allowed: false`, 403 on managedclusters) or allows, plus the backend's ServiceAccount token that `bin/fake-kubeconfig` carries. | `access.json` |
| `scenarios.json` | fake hub | Timed events that the hub controllers apply to the CRs (joined, imported, add-ons available, lease lost or recovered, API server down or up, detaching, detached). | `scenarios.json` |
| `settings.json` | fake hub | `speed` of the scenario clock; optional `detachAfter`. | `settings.json` |
| `maassubscriptions.json` | fake hub | A `MaaSSubscriptionList` (`maas.opendatahub.io/v1alpha1`) for Subscriptions CRUD. Optional; missing means an empty store. | hand-authored (console `subscriptions.json` is draft protojson, not CR-shaped) |
| `maasauthpolicies.json` | fake hub | A `MaaSAuthPolicyList` paired with the seeded subscriptions (`<id>-policy`). Optional. | hand-authored |
| `maasmodelrefs.json` | fake hub | A `MaaSModelRefList` (often in namespace `llm`) for Get enrichment / model registrations; also pairs Models fixtures in `ai-grid-internal-models`. Optional. | hand-authored |
| `llminferenceservices.json` | fake hub | An `LLMInferenceServiceList` (`serving.kserve.io/v1alpha2`) in `ai-grid-internal-models` for Models List/Get. Optional; missing means empty Models List until Create. | hand-authored |
| `policies.json` | fake hub | A `PolicyList` (`policy.open-cluster-management.io/v1`) of ACM Policies served as is by namespace and name. Optional; no preset ships one. The `ai-grid-ready` Policy is computed from the clusters instead and must not be listed. | hand-authored |
| `applications.json` | fake hub | An `ApplicationList` (`argoproj.io/v1alpha1`) of Argo CD Applications served by namespace: the catalog's Applications in `openshift-gitops` (`<ApplicationSet>-<cluster>`, owned by the ApplicationSet), whose `status.resources[]` health the backend reads models' and subscriptions' status from. Optional; missing means no reports, so everything shows Saved to Git. `default/` reports `granite-3b` ready on `site-paris-1`, progressing on `site-madrid-1` and its MaaSModelRef ready on `site-ashburn-1`, so placed on the first two it is partially available (P13); `poc/` is the same file with `site1`, `site2` and `site3`. | hand-authored |
| `searchnodes.json` | fake ACM Search (on the fake hub's port) | `{"items": [{cluster, name, labels, ready}]}`: the nodes Search indexes, with NVIDIA GPU Feature Discovery's labels as an ACM hub's spokes carry them. Each cluster's GPUs add up to its `status.capacity["nvidia.com/gpu"]`. **Hand-written, not converted**: the console has no node fixtures, so `hack/convert-console-fixtures` doesn't touch this file. | (none) |

The backend no longer reads `maassubscriptions.json`, `maasauthpolicies.json`,
`maasmodelrefs.json` or `llminferenceservices.json`: Models and Subscriptions
live in Git (README, "Git catalog"). The fake hub still serves them.

Every file is decoded strictly: a misspelled key stops the fake with an error.
A missing file leaves its part empty or at its default (no `access.json`
allows every token).

## How the conversion maps fields

- **ManagedCluster.** Field names stay; the draft's flat fields go back to
  `metadata`, `spec` and `status` (`kubernetesVersion` becomes
  `status.version.kubernetes`). `generation` becomes a number again. A
  condition's `CONDITION_STATUS_TRUE`/`FALSE` becomes `"True"`/`"False"`, and
  a condition without a status (the draft's UNSPECIFIED) becomes `"Unknown"`.
  A cluster with a `deletionTimestamp` also gets
  `deletionGracePeriodSeconds: 0`, which the API server sets when it marks an
  object with finalizers deleting and which the draft has no field for.
- **Organization.** `domainDetails` become `domains` (with `verified` and
  `autoRedirect`), each attribute value becomes a one-element array, an
  omitted `alias` is the name and an omitted `enabled` is `true` (Keycloak's
  defaults), an organization without domains has no `domains` key. Three
  fields of the console's tenants are its proposals for the backend, not
  Keycloak data, and are dropped: `identityProvider` (P1, an organization
  identity provider), `resourceVersion` (P2) and `createTime` (P3).
- **access.json** gains `serviceAccount`.
- **scenarios.json**: the `spoke` of a `joined` event is written in the CR's
  status shape.
- **settings.json** keeps `speed` only; `latency` and the hub capabilities are
  settings of the console's fake gRPC backend, which the real backend replaces.

The console's `models.json` / `presets.json` are console-proto shapes; the
Models API talks to live LLMInferenceService / Placement / MWRS / MaaSModelRef
objects via the dynamic client. `llminferenceservices.json` seeds one hub-only
model (`fixture-cpu-llm`) whose shape matches Models Create; Subscriptions use
the MaaS CR fixtures above instead of the console's flat `subscriptions.json`.

`fixtures/poc` includes the same hand-authored MaaS / LLMInferenceService files, and
the same catalog Applications on its own sites (`applications.json`), as
`default/` (copied, not console-converted) so `make dev-fake-poc` can exercise
Models and Subscriptions against the PoC cluster set.

## Regenerating

The converter is `hack/convert-console-fixtures`. From this repository's root,
with the console checked out next to it:

```sh
mkdir -p /tmp/console-537df38
git -C ../ai-grid-provider-console archive 537df38 mock/fixtures mock/fixtures-poc | tar -x -C /tmp/console-537df38
go run ./hack/convert-console-fixtures --src /tmp/console-537df38/mock/fixtures --out fixtures/default
go run ./hack/convert-console-fixtures --src /tmp/console-537df38/mock/fixtures-poc --out fixtures/poc
go test ./internal/fakeinfra/...
```

The tests load both directories into the fakes, and check that every cluster
is one the hub controllers have already settled.
