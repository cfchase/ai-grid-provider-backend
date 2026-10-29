# AI Grid backend

`ai-grid-provider-backend` is a Go service that manages Open Cluster
Management (OCM)/RHACM `ManagedCluster` resources via the Kubernetes API
(hub-side CRUD; it does not provision spoke clusters). It is built with
[connect-go](https://connectrpc.com), so `:8080` answers the Connect, gRPC and
gRPC-Web protocols from one set of handlers and a browser can call it without
a proxy.

The current API includes:

- `ai_grid.v1.AiGrid/Ping`
- `ai_grid.v1.Clusters/List`
- `ai_grid.v1.Clusters/Get`
- `ai_grid.v1.Clusters/Create`
- `ai_grid.v1.Clusters/Update`
- `ai_grid.v1.Clusters/Delete`
- `ai_grid.v1.Tenants/List`
- `ai_grid.v1.Tenants/Get`
- `ai_grid.v1.Tenants/Create`
- `ai_grid.v1.Tenants/Update`
- `ai_grid.v1.Tenants/Delete`
- `ai_grid.v1.Models/List`
- `ai_grid.v1.Models/Get`
- `ai_grid.v1.Models/Create` (commits the model to the Git catalog; see [Git catalog](#git-catalog))
- `ai_grid.v1.Models/Update`
- `ai_grid.v1.Models/Delete`
- `ai_grid.v1.Subscriptions/List`
- `ai_grid.v1.Subscriptions/Get` (gRPC/REST join with the managed AuthPolicy + ModelRegistrations; the Go `client.GetSubscription` helper returns the Subscription object only)
- `ai_grid.v1.Subscriptions/Create` (commits the subscription and its managed `MaaSAuthPolicy` `<id>-policy`; returned in response)
- `ai_grid.v1.Subscriptions/Update` (rewrites both)
- `ai_grid.v1.Subscriptions/Delete` (removes both)

REST and Swagger UI are also exposed on HTTP `:8081`:

- Swagger UI: `http://localhost:8081/docs/`
- OpenAPI (Swagger 2.0): `http://localhost:8081/openapi.json`
- REST routes: `/api/v1/ping`, `/api/v1/clusters`, `/api/v1/tenants`, `/api/v1/models`, `/api/v1/subscriptions`
- UI handout (payloads + examples): [`docs/UI_API_REFERENCE.md`](docs/UI_API_REFERENCE.md)

[vanguard](https://github.com/connectrpc/vanguard-go) serves REST by reading
the `google.api.http` annotations off the descriptors at runtime, so there is
no generated gateway code. The same routes answer on `:8080` as well.

Regenerate protobuf, RPC stubs and OpenAPI with `make generate` (requires [buf](https://buf.build);
without it, `make generate BUF='go run github.com/bufbuild/buf/cmd/buf@v1.50.0'`). It also
generates the console's draft API (see [The console's draft API](#the-consoles-draft-api-ai_griddraftv1)).

Before changing an existing message, field, enum or RPC in `proto/ai_grid/v1`,
read [`docs/API_CONTRACT.md`](docs/API_CONTRACT.md): the console and other
clients are generated from it, so CI runs `buf lint` and, on pull requests,
`buf breaking` against the base branch, and a break follows expand, migrate,
contract. Run the same check locally:

```sh
buf breaking proto --against '.git#branch=main,subdir=proto'
```

ManagedCluster RPCs use the Kubernetes dynamic client against:

```text
cluster.open-cluster-management.io/v1/managedclusters
```

### Cluster membership, state and location

Every cluster the Clusters API returns carries three fields the backend
derives, so no client has to:

- `status.ai_grid_member`: the `ai-grid/enabled` label is exactly `"true"`,
  the selector the hub's AI Grid Placements match. A cluster joins or leaves
  the grid through `Clusters/Update` by setting or removing that label.
- `status.state` and `status.state_message`: the lifecycle state the ACM
  console shows (`READY`, `OFFLINE`, `UNKNOWN`, `PENDING_IMPORT`, `IMPORTING`,
  `IMPORT_FAILED`, `NOT_ACCEPTED`, `DETACHING`; `UNSPECIFIED` before the hub
  reports any condition), derived from the conditions in the ACM console's
  order, and the message of the condition that decided it. The order is in
  the comment on `ManagedClusterState` in
  `proto/ai_grid/v1/managed_cluster_type.proto`.
- `status.location`: where the cluster is, for a map, described below.

#### Cluster locations

ACM has no field for a cluster's position, so a hub admin sets it once as
annotations on the `ManagedCluster`. Annotations, not labels: a label value
can't start with `-`, and half the world's coordinates are negative.

| Annotation | Value |
| --- | --- |
| `ai-grid/latitude` | Decimal degrees, `-90` to `90` (north is positive), for example `39.9612` |
| `ai-grid/longitude` | Decimal degrees, `-180` to `180` (east is positive), for example `-82.9988` |
| `ai-grid/site-name` | Optional; the name a map shows, for example `Columbus DC2` |

Write plain decimal numbers: a `.` for the decimal point, a leading `-` for
south and west, no exponent, no `N`/`S`/`E`/`W`, no degrees-minutes-seconds.
Surrounding whitespace is ignored.

With `oc` (or the ManagedCluster's YAML editor in the OpenShift console):

```sh
oc annotate managedcluster site-a \
  ai-grid/latitude=39.9612 ai-grid/longitude=-82.9988 \
  ai-grid/site-name="Columbus DC2" --overwrite
# remove them again
oc annotate managedcluster site-a ai-grid/latitude- ai-grid/longitude- ai-grid/site-name-
```

With GitOps, apply a partial `ManagedCluster` server-side, so only these
annotations are owned by Git and ACM keeps the rest of the object. Quote the
values: unquoted, YAML reads `-82.9988` as a number and the API server
refuses it, since annotations are strings.

```yaml
apiVersion: cluster.open-cluster-management.io/v1
kind: ManagedCluster
metadata:
  name: site-a
  annotations:
    ai-grid/latitude: "39.9612"
    ai-grid/longitude: "-82.9988"
    ai-grid/site-name: "Columbus DC2"
    # Argo CD: never prune or delete the ManagedCluster itself; deleting it
    # detaches the cluster from the hub.
    argocd.argoproj.io/sync-options: ServerSideApply=true,Prune=false,Delete=false
```

`oc apply --server-side --field-manager=ai-grid-locations -f site-a.yaml`
does the same by hand.

What a client gets in `status.location` (`site_name` is always the trimmed
`ai-grid/site-name`, and `region` the cluster's
`region.open-cluster-management.io` claim, whatever else applies):

| The cluster has | `latitude`, `longitude` | `source` | `problem` |
| --- | --- | --- | --- |
| both annotations, valid | the annotations | `ANNOTATIONS` | empty |
| `0` and `0` | `0`, `0` (a valid position) | `ANNOTATIONS` | empty |
| no annotations, a known AWS, Azure or GCP region | the region's city, approximate | `CLOUD_REGION` | empty |
| no annotations, an unknown region or none (on-premises) | unset | `NONE` | empty |
| only one of the two | unset | `NONE` | names the missing one |
| a value that isn't a plain decimal (`40,7`, `40.7N`, DMS, `NaN`, `Inf`, `4e1`) | unset | `NONE` | quotes the value |
| a value out of range | unset | `NONE` | quotes it and gives the range |
| out of range, but valid with the two swapped | unset | `NONE` | says they look swapped |

Wrong annotations never fall back to the region: the cluster has no
coordinates until the admin fixes them, and `problem` (all problems, joined
with `; `) says what to fix. `Clusters/Update` refuses the same wrong values.
The region table is `internal/servers/cloud_regions.go`, with its sources;
a UI should say a `CLOUD_REGION` position is approximate. `make
dev-fake-locations` serves one cluster per row (see
[fixtures/README.md](fixtures/README.md)).


### Git catalog

The Models and Subscriptions RPCs write only to a Git repository, and Argo CD
delivers the files to the clusters; the backend never creates, updates or
deletes a model or subscription object on a cluster. Each create, update or
delete is one commit to the branch. Reads come from the repository too, and
status from the hub's Argo CD Applications ([Status](#status-from-the-spokes)).

| Setting | Meaning |
|---|---|
| `AI_GRID_GIT_URL` | Repository URL, without credentials. Without it the backend still starts: Models' and Subscriptions' list, get, create, update and delete answer `FailedPrecondition`, naming this setting, and `Models/ListPlacementClusters` and `Models/ListLocations` (which read the cluster) and the other APIs serve as usual. Setting `AI_GRID_GIT_TOKEN_FILE` without it is an error at startup. The backend clones it in the background and retries a failed clone, waiting up to a minute between attempts; until a clone succeeds, Models and Subscriptions answer `Unavailable` and the other APIs serve as usual. A `file://` URL or local path is served in-process, so the image needs no `git`. |
| `AI_GRID_GIT_BRANCH` | Branch to commit to. Default `main`. It must have at least one commit. |
| `AI_GRID_GIT_TOKEN_FILE` | File holding a GitHub personal access token with `contents: write` on that repository only, for example a mounted Secret. Sent as HTTPS basic auth, so `AI_GRID_GIT_URL` must be `https://` when it is set; never logged. |
| `AI_GRID_MODELS_NAMESPACE` | Namespace of each model's `LLMInferenceService` on the inference spokes. Default `ai-grid-internal-models`. An override must name a namespace that's already enabled for grid routing on every inference spoke: labelled `grid.opendatahub.io/allowed: "true"`, allowed by the inference Gateway's `allowedRoutes`, and with NetworkPolicies that let the grid reach the model and its scheduler's metrics. The backend writes one NetworkPolicy per model that admits the grid namespace; the namespace and its other policies come from the environment. |
| `AI_GRID_SUBSCRIPTION_NAMESPACE` | Namespace of the subscriptions, their policies and the models' catalog entries on the ingress spokes. Default `models-as-a-service`. |
| `AI_GRID_CATALOG_APP_NAMESPACE` | Hub namespace of the catalog's Argo CD ApplicationSets and Applications, which status is read from. Default `openshift-gitops`. |
| `AI_GRID_CATALOG_MODELS_APPSET` | ApplicationSet that delivers each cluster's models (`catalog/clusters/<cluster>/`). Default `acm-models-catalog-models`. |
| `AI_GRID_CATALOG_INGRESS_APPSET` | ApplicationSet that delivers every model's `ExternalModel` and `MaaSModelRef` (`catalog/ingress/`). Default `acm-models-catalog-ingress`. |
| `AI_GRID_CATALOG_SUBSCRIPTIONS_APPSET` | ApplicationSet that delivers every subscription (`catalog/subscriptions/`). Default `acm-models-catalog-subscriptions`. |
| `AI_GRID_GRID_NAMESPACE` | Namespace of the grid on the inference spokes, which each model's NetworkPolicy admits. Default `redhat-ai-grid`. |

The repository holds:

```text
catalog/
  subscriptions/<id>.yaml                 # MaaSSubscription <id> and MaaSAuthPolicy <id>-policy (AI_GRID_SUBSCRIPTION_NAMESPACE)
  models/<id>/deployment/                 # LLMInferenceService <id> and NetworkPolicy <id>-grid (AI_GRID_MODELS_NAMESPACE)
  models/<id>/catalog/                    # ExternalModel <id> (to grid-provider) and MaaSModelRef <id> (AI_GRID_SUBSCRIPTION_NAMESPACE)
  clusters/<name>/kustomization.yaml      # the deployments placed on ManagedCluster <name>
  ingress/kustomization.yaml              # every model's catalog entry
```

An Argo CD ApplicationSet delivers `catalog/clusters/<name>` to the
ManagedCluster `<name>`, `catalog/ingress` and `catalog/subscriptions` to the
ingress spokes. The backend never deletes an index file, so a cluster with no
models still has its path. The ApplicationSets must sync with `prune: true`
and `allowEmpty: true`, so deleting the last object also removes it from the
spokes.

For both models and subscriptions:

- `metadata.version` is the file's `ai-grid/version` annotation, which each
  write increments. A stale version, or a write to the same file by another
  backend since the read, answers `Aborted`. Delete's `resource_version` is
  checked the same way; zero skips the check. A deleted and re-created id
  starts again at version 1, so a client still holding version 1 of the old
  object can update or delete the new one.
- The files belong to the backend. An update rewrites them from the stored
  object and the request, so a hand edit to a catalog file is lost on the
  next write to that object.
- Labels and annotations go into the files Argo CD applies, so a request may
  set only keys without a prefix, such as `team`, and the annotations
  `openshift.io/display-name` and `openshift.io/description`. Any other
  prefixed key, such as one of Argo CD's, Helm's or `ai-grid/`, is
  `InvalidArgument`, and so is a key or value Kubernetes would reject. The backend adds its own keys, and a read doesn't return
  them.
- The backend can't see the spokes. Argo CD adopts an object with the same
  name that already exists there, so keep hand-applied `MaaSAuthPolicy`
  objects off the `<subscription id>-policy` names; the backend can't refuse
  to create over an unmanaged policy as it did when it wrote to the cluster. Argo CD's
  `FailOnSharedResource=true` sync option on the catalog apps catches objects
  another Argo CD app manages, but not hand-applied ones.

For subscriptions:

- `spec.organization.id` is required. The file holds each owner group as
  `<organization id>/<group>`, the tenant-qualified form, in both the
  subscription and its policy, and reads return the bare names. A group sent
  already qualified with the same organization is accepted; one qualified with
  another organization is `InvalidArgument`; surrounding spaces are trimmed and
  repeats are dropped.
- The policy lives in the subscription's file, so Argo CD prunes both
  together; it has no ownerReference.
- A model ref without a namespace gets the subscription's namespace
  (`models-as-a-service` by default). A ref in that namespace must name a
  model in the repository, or Create and Update answer `FailedPrecondition`;
  a ref to another namespace isn't checked.
- Every read and every write's answer joins each model ref to the model's
  catalog entry (`catalog/models/<id>/catalog/model.yaml`): when the ref's
  name and namespace are those of the entry's `MaaSModelRef`, the ref gets
  `model_id`, the model's id, and `display_name` and `description` from that
  `MaaSModelRef`'s `openshift.io/display-name` and `openshift.io/description`
  annotations (the model's own, which the Models API copies there). A ref to
  anything else has none of them, and neither has a ref to a model whose entry
  can't be read or parsed: that is logged, and the call still answers. They're output only: never written to the
  file, and ignored on write. A client finds a ref's model by
  `model_id == Model.id`, and the subscriptions using a model with the List
  filter `this.spec.model_refs.exists(r, r.model_id == "<id>")`, without
  knowing any namespace.
- `status.phase` comes from the spokes (see "Status from the spokes" below):
  `Active` when every cluster with a subscriptions Application reports it
  ready; `Degraded` when any reports it failed; `Pending` when any reports it
  pending or doesn't list it yet; and empty when no cluster has reported.
  `Get` and `Create` return the file's policy with the same phases from its
  `MaaSAuthPolicy` reports, and a `Ready` condition whose message says where
  it is ready, such as `Ready on site-a`. `Get` also returns one
  `model_registrations` entry per model
  ref: phase `Ready`, `Failed` or `Pending` from the clusters' `MaaSModelRef` reports,
  or, with no report, no phase and a `Ready` condition of unknown status,
  reason `SavedToGit`, so a client doesn't take the models as missing.
- Keep a file that isn't `*.yaml`, such as a `README`, in
  `catalog/subscriptions/`. Git drops an empty directory, so without it
  deleting the last subscription removes the path, and Argo CD reports an error
  instead of pruning. List reads only `*.yaml` files.

For models:

- `spec.placement` is required and names at least one cluster, by the `name`
  label that ACM sets on every ManagedCluster, as `name In [...]` or a `name`
  match label, but not both: a label selector ANDs its requirements. A model
  with no placement answers `InvalidArgument`, because it
  would be published to the ingress and deployed nowhere. Cluster sets,
  other labels and `number_of_clusters` answer `InvalidArgument`, and so does a
  `spec.rollout` other than strategy `All` alone. Argo CD delivers to every
  cluster at once, which is `All`, so that rollout is accepted and not stored.
- Each cluster a create or update adds to the placement must be a
  ManagedCluster on the hub, or the call answers `InvalidArgument` naming it,
  with a `google.rpc.ResourceInfo` detail per unknown cluster
  (`resource_type` `type.googleapis.com/ai_grid.v1.ManagedCluster`,
  `resource_name` the cluster): a name no cluster has, such as a `location` label, would be committed and
  delivered nowhere. A cluster the model is already on is not checked again,
  so a model on a cluster that has left the hub can still be changed or moved
  off it. `Models/ListPlacementClusters` lists the clusters a model can run
  on; `Models/ListLocations` is its deprecated name, answering the same
  cluster names.
- The model namespace must exist on the inference spokes with the label
  `grid.opendatahub.io/allowed: "true"`, so the platform creates the grid's
  `InferenceProvider`. The NetworkPolicy admits the grid namespace on ports
  8000 and 9090.
- The version is an `ai-grid/version` annotation on the `LLMInferenceService`,
  and `ai-grid/clusters` records the placement. Neither shows in the API.
- The model is served and routed as its id. MaaS lists the model under its id
  and forwards the request body unchanged, so the `model` a tenant sends is
  the id all the way to vLLM: the `LLMInferenceService`'s `spec.model.name`
  (vLLM's served name) and the `ExternalModel`'s `targetModel` are the id.
  As in odh-dashboard, the served name is the resource name. The proto
  deprecates `spec.model.name` for clients: Create takes it empty or equal to
  the id, Update also takes the name a read returned, and any other value is
  `InvalidArgument`. A human-readable name goes in annotation
  `openshift.io/display-name`.
- **Models saved before this rule** may serve another name, such as
  `facebook/opt-125m`. Until such a model is rewritten, a read returns the
  name it actually serves. Its next update, whatever it changes (a replica
  count, a display name), switches it to serve the id: vLLM's served name
  changes, so its pods restart once, and callers that used the old name must
  send the id from then on. Through MaaS those callers already got 404. The
  same update drops annotation `ai-grid/model-name`, which only the first
  builds that served the id wrote (those entries already serve the id). To
  switch every model at a time you choose, save each one once, for example
  by re-saving its display name; a one-time migration is a follow-up.
- `status.placement` lists the clusters the file places the model on
  (`selected_clusters`, cluster names), and the distinct values of their
  `location` labels (`locations`, read from the ManagedClusters; not cluster
  names). `status.clusters` has one entry per placed cluster: `READY`,
  reason `Ready`, when the cluster reports the `LLMInferenceService` ready
  (and, on a combined cluster, the `MaaSModelRef` and `ExternalModel` too);
  `FAILED`, reason `Failed`, when one of them is failed; `PENDING`, reason
  `NotReady`, when one is pending; `PENDING`, reason `NoReport`, when one is
  missing. Its message says what the cluster reported, such as
  `LLMInferenceService ready; MaaSModelRef not ready; ExternalModel ready`.
  `status.phase` is, in this order: `READY` when every placed cluster is
  ready and every ingress cluster (the ones with an ingress Application, and
  the ones labelled `ai-grid/enabled: "true"` and `ai-grid/role` `ingress`
  or `combined`) reports its `MaaSModelRef` and `ExternalModel` ready, and
  there is at least one; `PARTIALLY_AVAILABLE`, reason `PartiallyAvailable`,
  when every ingress cluster is ready and some, not all, placed clusters
  are, with at least one ingress cluster too; `FAILED`, reason `Failed`,
  when any cluster reports one of them failed; otherwise `PENDING`, reason
  `NotReady`. The message covers every cluster, such as `Failed on site-a;
  ready on site-b`, followed by Argo CD's message for each failed object.
  With no report at all, it has no phase, reason `SavedToGit` and no
  `clusters`.
- **Two statuses.** The phase answers "can tenants use it?"; two more fields
  keep the deployment and MaaS apart, so a running model with no
  subscription doesn't read as starting:
  - `status.clusters[].deployment` is each placed cluster's
    `LLMInferenceService`: `RUNNING`, `STARTING` (not synced yet,
    progressing, or a step KServe reports still under way, such as
    `Deployment does not have minimum availability.`), `FAILED` (any other
    `Ready=False`, or a failed sync), or unspecified with no report;
    `deployment_message` is KServe's or Argo CD's message.
    `status.deployment` sums them: `RUNNING` when every placed cluster runs
    it, `FAILED` when one fails, `STARTING` otherwise.
  - `status.ingress[]` has one entry per ingress cluster with its MaaS
    `state`: `PUBLISHED` (`MaaSModelRef` ready), `NO_SUBSCRIPTION` (MaaS
    says `Awaiting governance pairing`: the backend is healthy but no
    `MaaSSubscription` and `MaaSAuthPolicy` pair covers the model),
    `NOT_READY`, `FAILED`, or unspecified with no report. `status.maas` sums
    them, worst first: `FAILED`, `NOT_READY` (also while one hasn't
    reported), `NO_SUBSCRIPTION`, `PUBLISHED`.
  A `MaaSModelRef` waiting only for a subscription counts as ready for the
  phase, so such a model is `READY` with `status.maas` `NO_SUBSCRIPTION`.
  `STARTING` has no deadline: a pod that keeps crashing, can't pull its
  image or finds no GPU also leaves its Deployment below minimum
  availability, so it shows as `STARTING` with that message until it's
  fixed. Argo CD's health keeps only the `Ready` condition's message, not
  the pods'. `status.deployment` can differ from the phase: failed on one
  cluster and running on another is `FAILED` there and
  `PARTIALLY_AVAILABLE` in the phase.
- `spec.main.requests` and `limits` must be Kubernetes quantities that
  Kubernetes accepts for a container: non-negative, whole numbers for
  extended resources such as `nvidia.com/gpu`, a request at most its limit,
  and equal to it for extended resources and hugepages; otherwise
  `InvalidArgument`. Such a resource with a request and no limit is saved
  with the request as its limit.
- Delete answers `FailedPrecondition` while a subscription in the repository
  names the model. A subscription written between the check and the commit
  makes the delete `Aborted`, and a model deleted before a subscription that
  names it commits makes that write `Aborted`.

#### Status from the spokes

The backend reads status back from the hub's Argo CD, which delivers the
catalog. Three ApplicationSets in `AI_GRID_CATALOG_APP_NAMESPACE` (default
`openshift-gitops`) each generate one Application per cluster, named
`<ApplicationSet>-<cluster>` (kahowell/ai-grid-tenant
`apps/acm-models/hub/catalog-04-applicationsets.yaml`):

| ApplicationSet | Delivers | Kinds read |
|---|---|---|
| `acm-models-catalog-models` | the cluster's models | `LLMInferenceService` |
| `acm-models-catalog-ingress` | every model's MaaS entries | `MaaSModelRef`, `ExternalModel` |
| `acm-models-catalog-subscriptions` | every subscription | `MaaSSubscription`, `MaaSAuthPolicy` |

An Application counts only when its ApplicationSet owns it. For each resource
in its `status.resources[]`, `Synced` and health `Healthy` is ready; health
`Degraded`, or out of sync after a failed sync, is failed, except the
`Degraded` messages of steps still under way, which are pending; out of sync while
Argo CD retries a failed sync (such as an AppProject refusing the kind or
namespace, which shows only in `status.operationState.message`) is pending
with that message; anything else
(`OutOfSync`, `Progressing`, `Missing`, no health) is pending, because the live
object is then the previous revision or still starting. A resource the
Application doesn't list isn't on that cluster yet. The health comes from the
hub's Argo CD resource health checks, which the hub must have for
`LLMInferenceService`, `ExternalModel`, `MaaSModelRef`, `MaaSSubscription` and
`MaaSAuthPolicy`, as kahowell/ai-grid-tenant's `apps/gitops-config/manifests/argocd.yaml`
does: Argo CD has none built in, and without them every model and subscription
stays pending.

Those health checks map every `Ready=False` to `Degraded` and keep only the
condition's message, but KServe and MaaS also set `Ready=False` for steps
still under way. So a `Degraded` resource is pending when its message is one
of these:

| Kind | Message | Means |
|---|---|---|
| `LLMInferenceService` | names a step under way, in KServe's wording (any case): `does not have minimum availability`, `rollout in progress`, `is progressing`, `has not been observed yet`, `accepted them yet`, `accepted it yet`, `is not yet accepted by gateway`, a ScaledObject `not yet` visible, available or ready, `The following Gateways are not ready`, `is not ready` (an inference pool) or `waiting for`; and doesn't dump a rejected condition (` (reason `) | starting: KServe copies the first unready step, such as its Deployment's `Available=False` |
| `MaaSModelRef` | `Awaiting governance pairing` | published, no subscription yet (`NO_SUBSCRIPTION`) |
| `MaaSModelRef` | `Awaiting governance pairing and backend readiness`, `Governed but backend is not ready` | not ready yet |

Any other `Degraded` message is failed. The messages are KServe's
(`pkg/controller/v1alpha2/llmisvc`) and maas-controller's
(`deriveModelPhase`); a release that rewords them turns that state into
failed until this list follows. A health check that reported these steps
as `Progressing` would make the list unnecessary.

A model the file places on a cluster that has no models
Application (not yet created, or the cluster left the catalog's Placement)
reads "no report from" that cluster and stays pending until the placement
changes. Until some cluster lists a new model, it stays saved to Git.
It reads on every request, with no cache, which suits the handful of clusters
a grid has. The ServiceAccount needs `get` and `list` on Applications in that
namespace, which lets it read every Application there, so keep secrets out of
their specs (inline Helm values, plugin settings). A missing or forbidden list means no report, not an error, so the
backend works without Argo CD.

The service runs in-cluster using its Kubernetes ServiceAccount. Its RBAC role
(`deploy/openshift/rbac.yaml`, and the same rules for Kind) writes only
ManagedClusters: `get`, `list`, `watch`, `create`, `update`, `patch` and
`delete` on `ManagedCluster` resources, plus the ManagedClusterSet join and
accept subresources the Clusters API needs. Otherwise it may only `get` the
`ai-grid-ready` Policy and `get` and `list` Argo CD Applications in `openshift-gitops`. It has no rights on MaaS, KServe or ACM placement objects: Models and
Subscriptions write to Git. `internal/deploycheck` fails on a write verb
outside the ManagedCluster rules.

Tenants CRUD maps to **Keycloak Organizations** in one shared platform realm
(`KEYCLOAK_REALM`, default `ai-grid`) through the Keycloak Admin REST API.
A Tenant is an Organization — **not** a Keycloak realm. `id` is the
organization UUID; `metadata.name` is the organization name (and alias). The
Kind setup creates two sample Organizations, `tenant-a` and `tenant-b`, and
configures the backend with the development Keycloak admin credentials. Use a
dedicated service account with organization manage/query roles instead of these
credentials outside local testing.

## Local architecture

The local environment is Kubernetes-native:

```text
Kind cluster using Podman
├── OCM Cluster Manager
├── ai-grid-provider-backend
├── Keycloak
└── PostgreSQL for Keycloak
```

The backend and Keycloak are deployed as Kubernetes workloads. Podman is used
by Kind to build and run the cluster nodes; Podman Compose is not required.

## Prerequisites

Install or make available:

- Go 1.26.3
- Podman
- Kind
- kubectl
- clusteradm
- grpcurl

The repository is aligned with fulfillment-service and declares `go 1.26.3`.

Install upstream OCM's CLI with Go 1.26.3 if needed:

```sh
go install open-cluster-management.io/clusteradm/cmd/clusteradm@v1.4.0
```

Ensure `$(go env GOPATH)/bin` is on `PATH`.

Kind supports Podman through:

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
```

## Run the complete integration test

From the repository root:

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
bash hack/test-kind.sh
```

The test starts by running the prerequisite check directly. You can run that
check without changing the cluster:

```sh
bash hack/check-prereqs.sh
```

It validates Podman, Kind, kubectl, grpcurl, Go, and clusteradm. `clusteradm`
may be installed with:

```sh
go install open-cluster-management.io/clusteradm/cmd/clusteradm@v1.4.0
```

The scripts also recognize an explicit `CLUSTERADM_BIN` or the Go bin
directory.

The script:

1. Creates or reuses a Kind cluster named `ai-grid-hub`.
2. Builds `ai-grid-provider-backend` with Podman and loads it into Kind.
3. Deploys the backend, Keycloak, and PostgreSQL. Kind runs the backend
   against an in-pod Git repository: an init container creates a bare
   repository with one seed commit on an emptyDir, so Models and
   Subscriptions commits are lost with the pod.
4. Installs the OCM hub control plane with `clusteradm init`.
5. Creates two real `ManagedCluster` custom resources.
6. Creates the `ai-grid` Keycloak realm and two sample Organizations.
7. Port-forwards the backend gRPC Service.
8. Runs typed Go gRPC integration tests and verifies ping, clusters, and tenants.

The sample resources are defined in
[deploy/kind/sample-managedclusters.yaml](deploy/kind/sample-managedclusters.yaml).

## Run the environment manually

Create the cluster, build the image, and deploy the workloads:

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
bash hack/kind-up.sh
```

Install OCM and apply the sample clusters:

```sh
bash hack/ocm-up.sh
```

Expose the backend locally:

```sh
kubectl -n ai-grid port-forward service/ai-grid-provider-backend 8080:8080 8081:8081
```

Open Swagger UI at [http://localhost:8081/docs/](http://localhost:8081/docs/).

List available gRPC services:

```sh
grpcurl -plaintext localhost:8080 list
```

API request authn/authz is out of scope for this MVP — gRPC, Connect and REST endpoints
are open. Keycloak is used only as the Tenants Organizations Admin API backend.
See [`docs/UI_API_REFERENCE.md`](docs/UI_API_REFERENCE.md).

The same port speaks the Connect protocol, which is a plain HTTP POST with a
JSON body — what a browser sends:

```sh
curl -X POST -H 'Content-Type: application/json' -d '{"name":"world"}' \
  http://localhost:8080/ai_grid.v1.AiGrid/Ping
```

Call the Clusters API (hub ManagedCluster list / get / create / update / delete):

```sh
grpcurl -plaintext -d '{}' localhost:8080 \
  ai_grid.v1.Clusters/List

grpcurl -plaintext -d '{"id":"playground-east"}' localhost:8080 \
  ai_grid.v1.Clusters/Get

grpcurl -plaintext \
  -d '{"object":{"id":"demo-cluster","metadata":{"name":"demo-cluster","labels":{"environment":"dev"}},"spec":{"hub_accepts_client":true}}}' \
  localhost:8080 ai_grid.v1.Clusters/Create

grpcurl -plaintext \
  -d '{"object":{"id":"demo-cluster","metadata":{"labels":{"ai-grid/enabled":"true"}},"spec":{"hub_accepts_client":true}},"update_mask":{"paths":["metadata.labels"]}}' \
  localhost:8080 ai_grid.v1.Clusters/Update

grpcurl -plaintext -d '{"id":"demo-cluster"}' localhost:8080 \
  ai_grid.v1.Clusters/Delete
```

REST equivalents use `/api/v1/clusters` (GET list, GET/`PATCH`/`DELETE` by id, POST create).
Objects use OSAC shape: `id` + `metadata` + `spec` + `status`.

Call the Tenants API:

```sh
grpcurl -plaintext -d '{}' localhost:8080 \
  ai_grid.v1.Tenants/List

grpcurl -plaintext \
  -d '{"object":{"metadata":{"name":"demo-tenant"},"spec":{"domains":["demo.example.com"]}}}' \
  localhost:8080 ai_grid.v1.Tenants/Create
```

REST equivalents use `/api/v1/tenants` (GET list, GET/`PATCH`/`DELETE` by id, POST create).

Call the service ping API:

```sh
grpcurl -plaintext -d '{"name":"Ada"}' localhost:8080 \
  ai_grid.v1.AiGrid/Ping
```

Or run the full gRPC + REST smoke suite:

```sh
# Local: KEYCLOAK_URL=http://127.0.0.1:8180 make run   # then in another shell:
make smoke

# OpenShift (port-forwards backend):
make smoke-openshift
# SKIP_CLUSTERS=1 make smoke-openshift
```

`make smoke-openshift` expects the backend already deployed (`make openshift-up`)
with an image that exposes `ai_grid.v1.Clusters` and full `ai_grid.v1.Tenants`
CRUD. If `grpcurl list` still shows `ManagedClusters`, rebuild/push and redeploy.

Keycloak is available inside the cluster at `keycloak.ai-grid.svc:8080`.
For local browser access, forward it with:

```sh
kubectl -n ai-grid port-forward service/keycloak 8180:8080
```

The development Keycloak credentials are `admin` / `admin`. The initialization
Job creates the `ai-grid` realm and Organizations `tenant-a` / `tenant-b`.

## Run Go tests

```sh
go test ./...
```

The repository also provides standard developer targets (`make help` lists them):

```sh
make build            # build bin/ai-grid-provider-backend
make unit             # unit tests (excludes Kind integration)
make smoke            # live API smoke (needs make run)
make lint             # gofmt check and go vet; uses golangci-lint when installed
make verify           # build, unit, and lint
make run              # run the API locally against your kubeconfig
make run-hub          # the same against your oc login's hub, set up like its console
make image            # build container image (Podman); alias: container
make push             # push container image (ghcr.io)
make build-and-push   # image + push
make kind-test        # full Podman/Kind integration test
make dev-fake         # real backend on fake hub + fake Keycloak (see below)
make dev-fake-poc     # the same on the PoC fixtures
```

## Run the API locally

Requires a kubeconfig that can reach a hub with the ManagedCluster CRD (Kind or
OpenShift after `oc login`). gRPC listens on `:8080`; REST and Swagger UI on
`:8081`. Models and Subscriptions commit to `bin/dev-catalog.git` unless
`AI_GRID_GIT_URL` is set ([Developing](#developing)). For Tenants, start a
local Keycloak first (Podman):

```sh
make keycloak-up      # :8180, seeds realm ai-grid + tenant-a / tenant-b
make run              # KEYCLOAK_URL=http://127.0.0.1:8180 by default
# LISTEN_ADDRESS=:9090 HTTP_LISTEN_ADDRESS=:9091 make run
make keycloak-seed    # re-seed orgs if needed
make keycloak-down
```

See [deploy/keycloak/README.md](deploy/keycloak/README.md). Swagger UI:
`http://localhost:8081/docs/`. With the API up, smoke every RPC/REST route:

```sh
make smoke
# GRPC_ADDR=localhost:8080 HTTP_ADDR=http://localhost:8081 make smoke
```

### Against a hub, set up like its console

`make run-hub` runs this checkout against the hub of your `oc login`, set up
like the console deployed there, for the console's `make dev`:

```sh
oc login https://api.<hub>:6443 ...
make run-hub              # gRPC/Connect 127.0.0.1:9090, REST 127.0.0.1:9092
make run-hub KEYCLOAK=hub # also the deployed console's Keycloak (Organizations)
```

It uses the current kubeconfig context as it is (`KUBECONFIG_PATH` picks
another kubeconfig, for `oc` and the backend alike) and stops when `oc whoami`
fails. While it runs, anything on this machine that reaches those ports acts
with your login, a web page through DNS rebinding included (`make run` too):
stop it when you're done. It listens on loopback only (`LISTEN_ADDRESS`, `HTTP_LISTEN_ADDRESS`
move it; any other address is refused, as the API has no authentication),
port-forwards ACM Search (`open-cluster-management/search-search-api`) for
cluster GPUs, or runs with `--search-api-url=off` without it, and copies the
plain Git and namespace settings (`AI_GRID_GIT_URL`, `AI_GRID_GIT_BRANCH`,
`AI_GRID_GRID_NAMESPACE`, `AI_GRID_MODELS_NAMESPACE`,
`AI_GRID_SUBSCRIPTION_NAMESPACE`) of the console's `ai-grid-backend` container,
the Deployment labelled `app.kubernetes.io/name=ai-grid-provider-console`.
Without exactly one such Deployment it keeps `make run`'s defaults. It never
reads the Git token, so Models and Subscriptions read the console's catalog,
and writes to it fail with the backend's error. `KEYCLOAK=hub` reads the console's Keycloak Secret into the
backend's environment only; without it Tenants use `make run`'s
`KEYCLOAK_URL`. The API acts as you: adding or removing clusters, and with
`KEYCLOAK=hub` organization changes, are real. Ctrl-C stops the API and the
port-forward. See [hack/run-hub.sh](hack/run-hub.sh).

The unit test uses a Kubernetes dynamic fake client. The Kind integration test
uses the real Kubernetes API server, real OCM CRD, real `ManagedCluster`
objects, Keycloak Organizations, and the deployed gRPC service.

## Dev mode with fake infrastructure

`make dev-fake` runs the real, unmodified backend on a laptop without Kind,
a hub or Keycloak. It starts `cmd/ai-grid-fake-infra`, which fakes the two
systems the backend talks to, then the backend itself against them, in one
terminal; Ctrl-C stops both.

```text
make dev-fake
├── bin/ai-grid-fake-infra --fixtures fixtures/default --draft-fixtures fixtures/draft/default
│   ├── fake hub API   127.0.0.1:9091  ManagedClusters + SelfSubjectAccessReview
│   │                                  (TLS for the backend, plain HTTP for the console BFF),
│   │                                  the catalog's Argo CD Applications (fixtures/default/applications.json)
│   │                                  and a fake ACM Search at /searchapi/graphql (GPU nodes)
│   ├── fake Keycloak  127.0.0.1:9093  token endpoint + Organizations Admin API
│   └── draft fake     127.0.0.1:9094  gRPC ai_grid.draft.v1, the console's draft API (own state)
├── bin/dev-catalog.git                bare Git repository Models and Subscriptions commit to
└── bin/ai-grid-provider-backend --kubeconfig bin/fake-kubeconfig
    ├── gRPC           127.0.0.1:9090  (what the console BFF dials by default)
    └── REST + Swagger 127.0.0.1:9092  (/api/v1/clusters, /api/v1/tenants, /docs/)
```

The fakes answer the way the real systems do: the hub with the Kubernetes API
server's status codes and `metav1.Status` messages, optimistic concurrency on
`resourceVersion`, finalizers and the ACM controllers' conditions, taints and
labels; Keycloak with 26.8's status codes and verbatim error messages. The
backend gets `KEYCLOAK_URL=http://127.0.0.1:9093` with the fake's admin
credentials, a kubeconfig carrying the fake ServiceAccount token, and
`--search-api-url` / `--search-ca-file` for the fake Search, which answers each
cluster's GPU nodes from `searchnodes.json`. The data
is fictional; see [fixtures/README.md](fixtures/README.md) for what each
preset contains and where it comes from.

```sh
make dev-fake                         # fixtures/default: ten clusters in every ACM state, six organizations,
                                      # catalog Applications, and a timeline
                                      # (a cluster joins at 20s, one detaches at 30s, ...)
make dev-fake-poc                     # fixtures/poc: the PoC's four sites and three organizations, static
                                      # (and fixtures/draft/poc for the draft fake)
make dev-fake-locations               # fixtures/locations: one cluster per status.location edge case
                                      # (and fixtures/draft/default for the draft fake)
make dev-fake DEV_GRPC_PORT=19090 DEV_HUB_PORT=19091 DEV_HTTP_PORT=19092 DEV_KEYCLOAK_PORT=19093 DEV_DRAFT_PORT=19094
                                      # when the default ports are taken

curl -s http://127.0.0.1:9092/api/v1/clusters
curl -s http://127.0.0.1:9092/api/v1/tenants
curl -s http://127.0.0.1:9092/api/v1/subscriptions
curl -s http://127.0.0.1:9092/api/v1/models
grpcurl -plaintext -d '{"id":"site-paris-1"}' 127.0.0.1:9090 ai_grid.v1.Clusters/Get
```

Writes to clusters and organizations go to the fakes' memory and are gone on
restart. Models and Subscriptions commit to `bin/dev-catalog.git`, which
stays; see [Developing](#developing). A cluster deleted
through the API stays detaching for 30 seconds (divided by `speed` in
`settings.json`) while the hub controllers drain it; then they remove their
own finalizers, and the cluster disappears unless a finalizer of someone
else is still on it.

The kubeconfig is written to `bin/fake-kubeconfig` (`DEV_KUBECONFIG`) with the
context `ai-grid-fake-hub`, and removed on exit. An existing file at that path
that is not such a kubeconfig is never replaced or removed: `make dev-fake`
stops with an error instead.
To see the console as a non-admin or with a rejected token, use the tokens in
`fixtures/default/access.json` (`viewer-token`, `expired-token`).

### With the console

Two terminals:

```sh
# terminal 1, this repository: fake hub + fake Keycloak + real backend + draft fake
make dev-fake

# terminal 2, ai-grid-provider-console: its BFF (dials 127.0.0.1:9090 and sends its
# SelfSubjectAccessReview to http://127.0.0.1:9091) and its frontend
make -j2 dev-bff-fake dev-frontend-fake
```

Then open http://127.0.0.1:9000. Do not run the console's own `make dev-fake`
at the same time: it starts the console's fake backend on the same 9090 and
9091. The console's `dev-bff-fake` passes `--fake-backend`, so the UI shows its
fake-backend banner although the backend is real here. The console's BFF
is built on the console's draft `ai_grid.v1` proto, whose services are named
differently from this backend's (`ManagedClusters/ListManagedClusters` and
`Tenants/ListTenants` there, `Clusters/List` and `Tenants/List` here; checked
at console commit `537df38`), so until the two protos converge the gRPC calls
it makes for clusters and tenants answer `Unimplemented` and those pages show
"not available on this backend". The sign-in and provider-admin check
(SelfSubjectAccessReview on the fake hub) work as with the console's own
fake. The draft fake on 9094 serves the console's draft services
(`ai_grid.draft.v1`); pointing the console's BFF at it is the console's side of
the move.

## Developing

Models and Subscriptions write only to Git, so a change to them is tested by
reading what the backend committed, and then, on a cluster, what Argo CD
delivered.

### Level 1: laptop only

`make dev-fake` commits to `bin/dev-catalog.git` (`DEV_CATALOG`), a bare
repository that it creates on first run with one seed commit holding
`catalog/subscriptions/README`. `bin/` is git-ignored, and the repository
survives restarts. The fake hub serves the catalog's Applications from
`fixtures/default/applications.json`, so models and subscriptions named there
show status.

```sh
make dev-fake                         # terminal 1
# terminal 2: a model on two sites, then a subscription to it
curl -s -X POST http://127.0.0.1:9092/api/v1/models -H 'Content-Type: application/json' -d '{
  "id": "granite-3b",
  "metadata": {"annotations": {"openshift.io/display-name": "Granite 3B"}},
  "spec": {"model": {"uri": "hf://ibm-granite/granite-3b"}, "replicas": 1,
    "placement": {"label_selector": {"match_expressions": [{"key": "name", "operator": "In", "values": ["site-paris-1", "site-madrid-1"]}]}}}}'
curl -s -X POST http://127.0.0.1:9092/api/v1/subscriptions -H 'Content-Type: application/json' -d '{
  "id": "team-a",
  "spec": {"organization": {"id": "org-a"}, "owner": {"groups": ["team-a"]},
    "model_refs": [{"name": "granite-3b", "unlimited": true}]}}'
make dev-catalog-log                  # the commits and the files at the branch head
make dev-catalog-reset                # delete the repository and start from the seed commit again
```

`granite-3b` shows `PARTIALLY_AVAILABLE`: the fixtures report it ready on
`site-paris-1`, not ready on `site-madrid-1`, and its `MaaSModelRef` ready on
the ingress `site-ashburn-1`, and `status.clusters` has an entry for each of
the two placed clusters. Any other name shows `SavedToGit`. On
`make dev-fake-poc`, place `granite-3b` on `site1` and `site2` instead:
`fixtures/poc/policies.json` reports it the same way. `team-a`'s model ref
reads back joined to the model: `namespace` `models-as-a-service`,
`model_id` `granite-3b` and `display_name` `Granite 3B`. `make run` commits to the
same repository. To commit somewhere else, such as a clone you push from, set
`AI_GRID_GIT_URL` (and `AI_GRID_GIT_BRANCH`); it overrides the default.

### Level 2: a dev cluster

Point the backend at a branch of your own fork of the catalog repository, and
point the dev cluster's Argo CD at that branch, so your commits reach real
spokes without touching anyone else's:

1. Fork the repository the environment's Argo CD reads the catalog from, and
   create a branch.
2. Change the catalog Applications or ApplicationSets on your dev cluster to
   follow your fork and branch (in the environment repository, or by hand on a
   cluster only you use).
3. Run the backend with `AI_GRID_GIT_URL` set to your fork,
   `AI_GRID_GIT_BRANCH` to your branch and `AI_GRID_GIT_TOKEN_FILE` to a file
   holding a fine-grained token with `contents: write` on the fork only:
   locally with `make run` against the dev cluster's kubeconfig, or deployed
   with `make openshift-up`.
4. Create a model or subscription, then watch Argo CD sync it and report its
   health.

## The console's draft API (ai_grid.draft.v1)

The console needs more API than this backend implements yet. Its proposal for
the missing APIs is the **draft**, package `ai_grid.draft.v1`
(`proto/ai_grid/draft/v1`, generated into `gen/ai_grid/draft/v1`), with a fake
that implements it in memory (`internal/fakeinfra/draft`, fixtures in
`fixtures/draft/`). `make dev-fake` serves it on `127.0.0.1:9094`
(`DEV_DRAFT_PORT`; `make dev-fake DEV_DRAFT_ADDRESS=` runs without it). Both moved here from the console repository at `ac80c25`
so the proposal lives next to the real API; [docs/draft/README.md](docs/draft/README.md)
has the details, the research behind it and a page per area.

- The draft is not this backend's API: no real server, client or REST route
  uses it, it has no OpenAPI, and its package is separate from `ai_grid.v1`
  so the two can be linked into one binary.
- When the backend implements an area in `ai_grid.v1`, that area's draft proto,
  fake code, fixtures and docs are deleted in the same change or the next one.
- For the real services, `make dev-fake` fakes only infrastructure (the hub
  and Keycloak); the draft fake exists only for areas without a real server.
- The draft fake keeps its own state and does not read the fake hub's clusters
  or the fake Keycloak's organizations yet; that integration is a later step.

## Clean up

Delete the local Kind cluster when finished:

```sh
kind delete cluster --name ai-grid-hub
```

## Install ACM hub on OpenShift (playground)

`make openshift-up` installs RHACM automatically when the `ManagedCluster` CRD
is missing (needs `helm` + OperatorHub; allow 15–20+ minutes). To install ACM
alone, or re-run it explicitly:

```sh
make acm-hub-up
# CHANNEL=release-2.13 make acm-hub-up   # OCP 4.18; default chart channel is release-2.13
# CHANNEL=release-2.16 make acm-hub-up   # OCP ≥ 4.19
# SKIP_SAMPLES=1 make acm-hub-up
```

See [deploy/acm-hub/README.md](deploy/acm-hub/README.md). Tear down with
`make acm-hub-down` (not part of `make openshift-down`). On hubs that already
have real ACM clusters, skip backend sample CRs with
`APPLY_SAMPLES=0 make openshift-up` (and `SKIP_ACM=1` if you must not touch ACM).

## Deploy to OpenShift

Kind-parity stack for a logged-in OpenShift cluster (`oc whoami`): backend,
optional embedded Keycloak + Postgres (Tenants API), RHACM when needed
(Clusters API), and sample ManagedClusters. There is no Route: the API has no
authentication of its own, and anyone who reaches it can commit a model (any
image, onto the spokes) or a subscription to the catalog. Reach it with
`oc port-forward`, or put an authenticating proxy in front of the Service.
Earlier versions created two public Routes, and `oc apply` doesn't prune, so
`make openshift-up` deletes `ai-grid-provider-backend-grpc` and
`ai-grid-provider-backend-http` if they exist.

Default image: [`ghcr.io/ai-grid-cloud/ai-grid-provider-backend`](https://github.com/ai-grid-cloud/ai-grid-provider-backend/pkgs/container/ai-grid-provider-backend).

CI (`.github/workflows/ci.yaml`) runs the Check job on pull requests and on
pushes to `main` and `v*` tags (build, lint, unit tests, generated-file
verify). After Check passes, pushes, tags and manual runs publish the image to
ghcr.io: `sha-<full sha>` every time (pin deployments to it), `latest` on
`main`, and semver from `v*` tags. It pushes with `GITHUB_TOKEN`; no repository
secrets are needed.
A new ghcr package starts private: when CI first publishes a new image, an
organization owner sets the package's visibility to Public, or clusters need a
pull secret.

Pull requests also get an image: `publish-image.yml` builds it without
credentials, `publish-pr-image.yml` pushes it as `pr-<number>` (never `sha-*`,
`latest` or semver), `cleanup-pr-image.yml` deletes it when the PR closes, and
`prune-images.yml` removes `pr-*` and untagged versions older than 30 days
every Monday.

Publish locally (login required once: `podman login ghcr.io`):

```sh
make image              # build linux/amd64 → ghcr.io/ai-grid-cloud/ai-grid-provider-backend:latest
make push               # push that tag
make build-and-push     # image + push
# TAG=dev make build-and-push
# PLATFORM=linux/arm64 make build-and-push   # only if the cluster is arm64
```

Images default to `--platform=linux/amd64` so builds on Apple Silicon run on typical
OpenShift nodes. Rebuild and push after changing the Containerfile, then restart
the deployment (`make openshift-up` or `oc -n ai-grid rollout restart deploy/ai-grid-provider-backend`).

Then deploy:

```sh
# Embedded Keycloak + ACM (if ManagedCluster CRD missing) + sample clusters.
# hack/openshift-up.sh requires AI_GRID_GIT_URL, the repository Models and Subscriptions commit to.
AI_GRID_GIT_URL=https://github.com/<you>/<catalog>.git \
  AI_GRID_GIT_TOKEN_FILE=$HOME/.config/ai-grid/catalog-token \
  make openshift-up

# Or build/push and deploy in one step
BUILD_IMAGE=1 make openshift-up

# Use an existing Keycloak instead of deploying one
EXTERNAL_KEYCLOAK=1 \
  KEYCLOAK_URL=https://keycloak.example.com \
  KEYCLOAK_ADMIN_USERNAME=admin \
  KEYCLOAK_ADMIN_PASSWORD=... \
  make openshift-up

# Skip ACM install (Clusters API needs the CRD already present)
SKIP_ACM=1 make openshift-up

# Force ACM install even if the CRD already exists (idempotent helm upgrade)
INSTALL_ACM=1 make openshift-up

# Skip sample ManagedClusters on hubs that already have real ACM clusters
APPLY_SAMPLES=0 make openshift-up
```

Clusters `List`, `Get` and `Update` report each cluster's `status.provisioning`
from the hub Policy named by `AI_GRID_READY_POLICY` (`namespace/name`, default
`ai-grid-policies/ai-grid-ready`). The ClusterRole in
`deploy/openshift/rbac.yaml` grants `get` on Policies with that name only, in
any namespace; change its `resourceNames` if you change the variable. Without
that permission, or without the Policy, `provisioning` is empty.

Clusters `List`, `Get` and `Update` also report each cluster's `status.gpus`
(the GPU product, memory, count, architecture, driver and sharing of every GPU
node) from ACM Search, which indexes the managed clusters' nodes with NVIDIA
GPU Feature Discovery's labels; the hub's own ManagedClusterInfo keeps none of
them. The backend asks Search once a minute at most, one query at a time. A
caller gets the last answer at once while a newer one is fetched, unless that
answer is more than five minutes old: then it waits for the query, only as long
as its own deadline. The backend keeps only the GPU labels it reads from each
node, and refuses an answer larger than 16 MiB. It calls Search with its
own credential: the ServiceAccount token in a pod, the kubeconfig's token, exec
plugin or auth provider out of one. A kubeconfig with only a client certificate
leaves Search calls anonymous, so clusters carry no `gpus` (the backend warns at
startup). Settings:

| Variable | Default | Meaning |
| --- | --- | --- |
| `--search-api-url` | `https://search-search-api.open-cluster-management.svc:4010/searchapi/graphql` | Search's GraphQL endpoint, https only; `off` turns GPUs off (the Kind deployment does). Redirects are never followed, so the credential can't be sent elsewhere |
| `--search-ca-file` | the pod's service CA (`/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt`), else the system roots | CA of Search's serving certificate |
| `--search-insecure-skip-verify` | off | Skips the certificate check; only for a local run against a port-forwarded Search, so the backend refuses to start with it unless `--search-api-url` is a loopback address (`127.0.0.1`, `::1` or `localhost`) |

The ClusterRole grants `get` on `searches/allManagedData`
(`search.open-cluster-management.io`), which Search reads as "may see every
managed cluster's resources": broader than the GPU nodes the backend asks for
(see the comment in `deploy/openshift/rbac.yaml`). When Search is missing, slow or refuses the
token, the backend keeps serving the last answer for up to five minutes, then
clusters carry no `gpus`; the call still succeeds, and the backend logs a
warning once per minute.

Locally against a hub (`make run`), port-forward Search and point the backend at it:

```sh
oc -n open-cluster-management port-forward svc/search-search-api 4010:4010 &
make run RUN_ARGS='--search-api-url=https://127.0.0.1:4010/searchapi/graphql --search-insecure-skip-verify'
```

If Keycloak or Postgres pods are rejected by SCC, re-run with
`GRANT_ANYUID=1` (or grant `anyuid` to the `default` SA in `ai-grid`).

After deploy, reach REST, Swagger UI and gRPC through a port-forward:

```sh
oc -n ai-grid port-forward svc/ai-grid-provider-backend 8080:8080 8081:8081
```

Tear down with `make openshift-down` (keeps the namespace unless
`DELETE_NAMESPACE=1`).

Manifests live under [deploy/openshift/](deploy/openshift/).

## Repository layout

```text
cmd/ai-grid-provider-backend/             service entrypoint
client/                          reusable gRPC client (GetSubscription returns object only; REST Get joins authPolicies + modelRegistrations)
proto/                           protobuf + buf config
proto/ai_grid/v1/                per-resource *_type.proto / *_service.proto + metadata
gen/ai_grid/v1/                  generated protobuf, gRPC, and Connect code
docs/API_CONTRACT.md             who consumes the proto, what CI checks, how to change it
docs/UI_API_REFERENCE.md         REST payloads for UI/BFF consumers
docs/openapi/                    generated OpenAPI (Swagger 2.0) JSON
internal/servers/                Per-resource connect-go handlers (OSAC-style builders)
internal/gitops/                 Git writer the Models and Subscriptions servers commit through (go-git)
internal/servers/clusters_server.go         Kubernetes-backed Clusters API
internal/servers/tenants_server.go Keycloak-backed tenant API
internal/keycloak/               Keycloak Admin API client
internal/restgateway/            HTTP gateway + Swagger UI
deploy/kind/                     Kind Kubernetes manifests
deploy/openshift/                OpenShift manifests (kustomize; no Route)
deploy/acm-hub/                  Helm chart + scripts for RHACM hub playground
hack/kind-up.sh                  build/load/deploy helper
hack/ocm-up.sh                   OCM bootstrap helper
hack/openshift-up.sh             OpenShift deploy helper
hack/openshift-down.sh           OpenShift teardown helper
hack/check-prereqs.sh            local toolchain and runtime preflight
hack/smoke-apis.sh               gRPC + REST smoke for Ping/Clusters/Tenants/Models/Subscriptions (make smoke)
hack/test-kind.sh                end-to-end integration test
integration/kind_test.go         typed gRPC integration test
cmd/ai-grid-fake-infra/          fake hub API + fake Keycloak + draft fake for make dev-fake
internal/fakeinfra/              the fakes (hub, keycloak, devserve) and their tests
internal/fakeinfra/draft/        fake of the console's draft API (ai_grid.draft.v1)
proto/ai_grid/draft/v1/          the console's draft proto (ai_grid.draft.v1)
gen/ai_grid/draft/v1/            generated draft protobuf and gRPC code
fixtures/                        fake infrastructure fixtures (default, poc; draft/ for the draft fake)
docs/draft/                      the draft's docs, research and per-area pages
hack/dev-fake.sh                 runs the fakes and the backend for make dev-fake
hack/dev-catalog.sh              creates, resets and shows bin/dev-catalog.git, the dev Git catalog
hack/convert-console-fixtures/   regenerates fixtures/ from the console's fixtures
Makefile                         build, test, lint, generate, Kind, OpenShift, ACM targets
```


