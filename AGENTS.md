# AI Grid Provider Backend

Go 1.26.3 service built on connect-go: one set of handlers answers the Connect, gRPC and
gRPC-Web protocols, and vanguard transcodes the `google.api.http` annotations into the REST
surface. It manages OCM/RHACM `ManagedCluster` resources and Tenants as Keycloak
Organizations in a shared platform realm (`KEYCLOAK_REALM`, default `ai-grid`) — not one
Keycloak realm per tenant. Local integration uses Kind and Podman; OpenShift deployment is
supported.

## Instruction precedence

1. Follow this file for repository-wide invariants and validation.
2. For API, protobuf, REST, resource-shape, server-layer, error, logging, testing, and security work, read and follow [`docs/API.md`](docs/API.md).
   For any change to an existing message, field, enum or RPC in `proto/ai_grid/v1`, also read [`docs/API_CONTRACT.md`](docs/API_CONTRACT.md): who consumes the contract, what CI checks, and the procedure for breaking changes.
3. For UI-facing REST payloads and examples, read [`docs/UI_API_REFERENCE.md`](docs/UI_API_REFERENCE.md).
4. Use [`README.md`](README.md) for supported setup and operational workflows.
5. If documentation and executable configuration disagree, stop and report the mismatch; do not silently choose one.

The OSAC fulfillment-service standards are the reference architecture. Apply their API and service invariants where this repository implements the same concern. Do not copy fulfillment-service-specific PostgreSQL, event, reconciler, GoReleaser, or public/private API patterns unless the change intentionally introduces those capabilities.

## Non-negotiable invariants

- Protobuf is the API contract. Edit sources under `proto/`; never hand-edit generated files under `gen/`, `docs/openapi/`, or `internal/restgateway/openapi/`.
- After any protobuf change, run `make generate`, commit all generated outputs, and verify regeneration leaves the worktree clean.
- `buf breaking` must pass for `ai_grid.v1`. Never remove, renumber, rename or retype a field, enum value, message or RPC in one step: expand (add the replacement, deprecate the old), let consumers migrate, then contract (remove, and `reserved` both the number and the name) as an approved break. Never reuse a field number. A changed meaning is a new field, not an edited comment. See [`docs/API_CONTRACT.md`](docs/API_CONTRACT.md).
- Managed resources use the OSAC shape: `id`, `metadata`, `spec`, and `status`.
  - `spec` is caller-controlled desired state.
  - `status` is service-controlled observed state.
- Resource services use plural names and standard `List`, `Get`, `Create`, `Update`, and `Delete` methods. Use `{Service}{Method}Request` and `{Service}{Method}Response` message names.
- REST collections remain flat under `/api/v1/`; do not introduce nested collection URLs.
- Use `CamelCase` for protobuf types, `snake_case` for fields, and type-prefixed `UPPER_SNAKE_CASE` enum values. Every enum starts with an `_UNSPECIFIED = 0` value.
- Use typed reference messages rather than unstructured ID strings when introducing resource relationships.
- Put field and cross-field rules in protobuf validation annotations where possible. Put external lookups, uniqueness, quota, and other cross-resource checks in server logic.
- Preserve gRPC status semantics. Match errors by status code or typed error, never by message text. Wrap Go errors with `%w`.
- Use `log/slog` with structured fields. Never log credentials, bearer tokens, kubeconfigs, Keycloak secrets, or complete sensitive request bodies.
- Do not commit credentials, tokens, private keys, kubeconfigs containing credentials, or production Keycloak administrator passwords.
- Local `admin` / `admin` Keycloak credentials are development-only. Non-local deployments use a least-privilege service account.
- Prefer in-cluster Kubernetes ServiceAccount credentials. Use `--kubeconfig` only for local, out-of-cluster development.
- Prefer Podman for local container and Kind workflows.

## The console's draft API

- `ai_grid.draft.v1` (`proto/ai_grid/draft/v1`, `gen/ai_grid/draft/v1`) is the console's proposal for APIs this backend does not implement yet, and `internal/fakeinfra/draft` is its in-memory fake, served by `make dev-fake` on `:9094`. See [`docs/draft/README.md`](docs/draft/README.md).
- The draft is not this backend's API: never import `gen/ai_grid/draft/v1` from `cmd/ai-grid-provider-backend`, `internal/servers`, `internal/restgateway` or `client`, and never give it HTTP annotations or OpenAPI output.
- The draft is gRPC only, on purpose: no Connect, gRPC-Web or REST. Each area moves to `ai_grid.v1` (gRPC, Connect and REST) when it is implemented for real. The console is moving every page to generated clients of `ai_grid.v1` and keeps no proto or fake of its own; once no page needs the draft, the draft package and `internal/fakeinfra/draft` are deleted. Don't add new areas to the draft: add what the console needs to `ai_grid.v1`.
- The draft is excluded from `buf breaking` (`proto/buf.yaml`); `ai_grid.v1` is not.
- When an area is implemented for real in `ai_grid.v1`, delete that area's draft proto, generated code, fake code and tests, fixtures and `docs/draft/areas` page (`docs/draft/README.md`, "When an area moves to the real backend").
- For real services, fake only infrastructure (`internal/fakeinfra/hub`, `internal/fakeinfra/keycloak`); never fake a real server.
- The draft proto keeps the console's messages, fields, numbers and types; only its package, `go_package`, imports and the comments that described the console's setup (the file headers) differ. Keep it passing `buf lint`. The API invariants above govern `ai_grid.v1`; the draft keeps the console's shapes (for example the flat `ManagedCluster`) until an area is implemented for real.
- `fixtures/draft/` is the console's protojson, not converted: edit it with `go test ./internal/fakeinfra/draft/...` running, not with `hack/convert-console-fixtures`.
- `internal/fakeinfra/draft/validation/cases.json` is a copy of the console's `mock/validation/cases.json`; keep the two in sync until the console stops needing it.

## Repository map

```text
cmd/ai-grid-provider-backend/   service entrypoint
client/                         reusable gRPC client
proto/                          protobuf sources and buf configuration
gen/ai_grid/v1/                 generated protobuf, gRPC, and Connect code
docs/API.md                     authoritative API/service standards for this repo
docs/API_CONTRACT.md            who consumes proto/ai_grid/v1, what CI checks, how to change it
docs/UI_API_REFERENCE.md        UI-facing REST payload handout
docs/openapi/                   generated OpenAPI snapshot
internal/servers/               per-resource connect-go handlers
internal/keycloak/              Keycloak Admin API client (Tenants = Orgs in KEYCLOAK_REALM)
internal/restgateway/           REST surface (vanguard) and Swagger UI
integration/                    Kind-backed integration tests
deploy/kind/                    Kind manifests
deploy/openshift/               OpenShift manifests
deploy/acm-hub/                 RHACM hub development assets
hack/                           development, smoke, and deployment helpers
cmd/ai-grid-fake-infra/         fake hub API and fake Keycloak for make dev-fake
internal/fakeinfra/             fakes of the hub API (hub/) and Keycloak (keycloak/); TLS and kubeconfig (devserve/)
fixtures/                       fake infrastructure fixtures (default/, poc/ converted; locations/ hand-written); see fixtures/README.md
proto/ai_grid/draft/v1/         the console's draft proto (ai_grid.draft.v1); proto/buf.gen.draft.yaml generates it
gen/ai_grid/draft/v1/           generated draft protobuf and gRPC code (no gateway, no OpenAPI)
internal/fakeinfra/draft/       fake of the draft API, served on --draft-address (make dev-fake :9094)
fixtures/draft/                 the draft fake's fixtures (default/, poc/), the console's verbatim
docs/draft/                     the draft's docs, research and per-area pages
```

Go module: `github.com/osac-project/ai-grid-provider-backend`.

## Change workflow

1. Read the relevant source, tests, `docs/API.md`, and nearby patterns before editing.
2. Keep changes scoped; do not refactor unrelated code.
3. For a new managed resource, update every applicable layer:
   - protobuf type and service contract;
   - generated Go, gRPC, gateway, and OpenAPI outputs;
   - server implementation and dependency wiring;
   - REST registration;
   - unit tests and integration/smoke coverage;
   - deployment/RBAC/configuration when new permissions or settings are required;
   - user and API documentation.
4. Add or update tests with production changes. Cover success, validation, not-found, conflict/already-exists, authorization, and dependency-failure behavior as applicable.
5. Run the smallest relevant checks while iterating, then the required final checks below.
6. Re-read the complete diff before committing. Confirm generated files, docs, manifests, and tests agree with the contract.

## Generated files

Run:

```bash
make generate
```

Commit the protobuf sources and regenerated outputs in:

- `gen/ai_grid/v1/`
- `gen/ai_grid/draft/v1/` (the draft, from `proto/buf.gen.draft.yaml`)
- `docs/openapi/`
- `internal/restgateway/openapi/`

After generation, rerun `make generate` and require `git status --porcelain` to print
nothing. Use `git status`, not `git diff --exit-code`: `git diff` ignores the index, so a
regenerated file that is already staged passes it silently, and untracked new output is
invisible to it as well.

Better still, run `make check-generated`. It generates into a temporary directory and
compares, so it also catches a file the protobuf no longer produces — which neither
command above can see, because an orphaned generated file is committed and unmodified.

## Validation

Run commands from the repository root.

### Required for every Go change

```bash
make build
make test
make lint
make help             # list targets
make prereqs          # toolchain check (Podman, Kind, kubectl, grpcurl, Go, clusteradm)
make build            # bin/ai-grid-provider-backend
make unit             # unit tests (excludes ./integration)
make smoke            # live gRPC/REST smoke (needs make run; optional keycloak-up)
make test             # alias for unit
make lint             # gofmt check, go vet; golangci-lint when installed
make verify           # build + unit + lint
make generate         # regenerate proto / gateway / OpenAPI
make run              # local API (gRPC :8080, REST/Swagger :8081) against kubeconfig
make run-hub          # local API on 127.0.0.1:9090/:9092 against your oc login's hub, set up like its console; KEYCLOAK=hub
make keycloak-up      # local Keycloak on :8180 + seed tenant-a/tenant-b
make keycloak-seed    # re-seed ai-grid realm / Organizations (idempotent)
make keycloak-down    # remove local Keycloak container
make kind-test        # full Kind integration (KIND_EXPERIMENTAL_PROVIDER=podman)
make image            # build container image (IMAGE/TAG, default ghcr.io :latest)
make push             # push container (podman login ghcr.io first)
make build-and-push   # image + push
make openshift-up     # deploy to current oc context (ghcr.io image by default)
make openshift-down   # remove OpenShift deploy
make acm-hub-up       # RHACM via Helm/OLM + playground ManagedClusters
make acm-hub-down     # tear down ACM hub Helm release
```

`make verify` is the aggregate check and must pass before a change is considered ready.

### Required for protobuf or API changes

```bash
buf lint proto
buf breaking proto --against '.git#branch=main,subdir=proto'   # docs/API_CONTRACT.md
make generate
git status --porcelain   # must print nothing
make check-generated     # also catches output the protobuf no longer produces
make verify
```

Also verify the generated OpenAPI and REST routes match the protobuf contract.

### Required for shell-script changes

```bash
bash -n path/to/changed-script.sh
shellcheck path/to/changed-script.sh
```

If `shellcheck` is unavailable, report that gap; do not claim shell validation passed.

### Required for container or deployment changes

```bash
make image
```

Use an explicit non-production image destination for development. Do not push images or deploy to a cluster unless the user explicitly requests it and the destination/context has been confirmed.

### Required for integration-sensitive changes

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
make kind-test
```

Run this for changes affecting Kubernetes behavior, OCM/RHACM resources, Keycloak integration, generated API behavior, deployment manifests, or end-to-end request flow. If the environment cannot run it, state exactly what was not validated.

### Local development without a cluster

```bash
make dev-fake         # real backend on the fake hub (:9091) and fake Keycloak (:9093); gRPC :9090, REST :9092; draft fake :9094
make dev-fake-poc     # the same on fixtures/poc and fixtures/draft/poc; override ports with DEV_*_PORT=...
make dev-fake-locations  # the same on fixtures/locations (and fixtures/draft/default for the draft fake), one cluster per status.location edge case
```

`make dev-fake` runs the unmodified backend against `internal/fakeinfra` (README.md, "Dev mode with fake infrastructure"). With the console, run `make -j2 dev-bff-fake dev-frontend-fake` in the console repo in a second terminal. It is a development aid, not a substitute for `make kind-test`.

### Optional live smoke and deploy helpers

```bash
make smoke            # live gRPC/REST smoke against a running API
make smoke-openshift  # same suite via oc port-forward
make openshift-up     # deploy to current oc context; auto-installs ACM if ManagedCluster CRD missing
make openshift-down   # remove OpenShift deploy (does not tear down ACM)
make acm-hub-up       # RHACM via Helm/OLM + playground ManagedClusters
make acm-hub-down     # tear down ACM hub Helm release
```

## Testing conventions

- Keep unit tests beside the package under test using `*_test.go`.
- Keep Kind-backed integration coverage under `integration/` and its supporting scripts.
- Keep `internal/fakeinfra` faithful to the real hub and Keycloak: every status code, reason, and message it writes cites its source, and `go test ./internal/fakeinfra/...` runs the real servers and clients against it. Change fixtures by re-running `hack/convert-console-fixtures` (fixtures/README.md), not by hand-editing the converted JSON.
- Prefer deterministic tests and typed assertions over matching human-readable error strings.
- Test gRPC status codes and response contracts.
- Do not weaken or delete tests merely to make a change pass.
- Do not introduce a new test framework without an explicit repository decision; follow the framework already used by neighboring tests.

## Error handling and logging

- Map invalid input to `codes.InvalidArgument`, missing resources to `codes.NotFound`, duplicates to `codes.AlreadyExists`, authorization failures to `codes.PermissionDenied`, and retryable version/conflict failures to `codes.Aborted` where applicable.
- Preserve underlying errors with `fmt.Errorf("context: %w", err)`.
- Log with `log/slog` and stable structured keys.
- Include useful resource identifiers and operation names, but redact secrets and sensitive payload fields.
## CI

[`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) runs Check on pull
requests and on pushes to `main` and `v*` tags (`make build`, `fmt-check`,
`vet`, pinned golangci-lint + `.golangci.yml`, `unit`, `buf lint`, `buf
breaking` against the base branch on pull requests (skipped by the `buf skip
breaking` label for an approved break, docs/API_CONTRACT.md), `make generate`
dirty diff). After Check passes, pushes, tags and manual runs publish the image to
`ghcr.io/ai-grid-cloud/ai-grid-provider-backend` (never on PRs).

Tags: `sha-<full sha>` on every publish (deploy this one), `latest` on `main`,
and semver from `v*` tags. No repository secrets: the job pushes with
`GITHUB_TOKEN` (`packages: write`).

Pull requests also get an image: `publish-image.yml` builds it without
credentials, `publish-pr-image.yml` pushes it as `pr-<number>` (never `sha-*`,
`latest` or semver), `cleanup-pr-image.yml` deletes it when the PR closes, and
`prune-images.yml` removes `pr-*` and untagged versions older than 30 days
every Monday.

## Security

## Security and tenancy

- Treat tenant identity and authorization as server-side concerns; never trust caller-supplied ownership fields without validation.
- API request authn/authz is out of scope for this MVP; gRPC/REST endpoints are open. Keycloak is used only as the Tenants Organizations Admin API backend.
- Because the endpoints are open, never make them reachable except through a proxy that authenticates and authorizes the caller. On a hub the backend runs inside the provider console's pod, bound to loopback (`--listen-address=127.0.0.1:9090`, `--http-listen-address=127.0.0.1:8081`), reachable only through the console's kube-auth-proxy and its BFF, which allows provider admins only; the console's kind test and ai-grid-env-setup's `verify/07-console.sh` check that. Every caller acts with the backend's ServiceAccount and its Git token: it may create, delete and label any ManagedCluster, read the catalog's Argo CD Applications, read ACM Search's index of every managed cluster (`searches/allManagedData`: everything Search indexes, not only the GPU nodes the backend asks for; see `deploy/openshift/rbac.yaml`), and commit Models and Subscriptions to the catalog repository, which Argo CD delivers to the spokes. `deploy/openshift` has no Route on purpose (`hack/openshift-up.sh` deletes the ones earlier installs created); reach a development install with `oc port-forward`, and never add a Route, Ingress or LoadBalancer Service for it. A CLI, GitOps tool or other UI that calls the backend directly needs authentication in the backend first. `KEYCLOAK_REALM` is the shared platform realm name, not a per-tenant id.
- Use least-privilege Kubernetes RBAC and Keycloak roles. Any new API operation that touches Kubernetes or Keycloak must include a review of required permissions.
- Do not expose internal credentials or administrative APIs through generated REST routes.
- Validate authentication and authorization behavior for every new externally reachable method once auth is in scope.
- Outside local Kind, use a dedicated Keycloak service account with least privilege (not realm-admin credentials). OpenShift deploy stores Keycloak settings in Secret `ai-grid-provider-backend-keycloak`.

## Build and image rules

- `make build` builds `bin/ai-grid-provider-backend`.
- `make image` builds the container image; `make push` publishes it.
- Keep `Containerfile` multi-stage and the runtime non-root.
- Pin tool and action versions used by CI. Do not use moving `latest` versions for linters or build tools.
- Automated publication must target a team-owned image namespace, not a personal namespace.

## Git and review hygiene

- Use focused commits with DCO sign-off.
- Include the repository-required AI-assistance trailer for AI-assisted commits.
- Never rewrite or discard unrelated user changes.
- Do not commit generated artifacts without their source changes.
- Before reporting completion, provide the exact checks run and their outcomes, plus any validation that could not be performed.

## Documentation

Update documentation in the same change when behavior, API contracts, configuration, permissions, deployment steps, or developer commands change. Keep `README.md`, `docs/API.md`, `docs/API_CONTRACT.md`, `docs/UI_API_REFERENCE.md`, generated OpenAPI, manifests, and this file consistent with executable behavior.
