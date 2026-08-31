# Upstream merge runbook (kube-dc fork)

This file documents the merge of `clastix/kamaji@26.5.2-edge-3` (branch
`upstream/master` at HEAD `1ed13ce6`) into our fork's
`kube-dc-external-endpoints` branch, performed 2026-05-09.

> **Status**: merged on test branch
> [`kube-dc-external-endpoints-merge-test`](https://github.com/kube-dc/kamaji/tree/kube-dc-external-endpoints-merge-test).
> NOT yet promoted to `kube-dc-external-endpoints`. NOT yet built or
> released.

---

## What's in the merge

- 45 upstream commits since merge-base `a9c2c0d` (Sep 2025), including:
  - `feat: support for v1.36 (#1132)`
  - `feat: multi strings arguments for kube-apiserver (#1130)`
  - `feat: add advertiseAddress to NetworkProfile for split management/tenant addressing (#1111)`
  - `feat: configurable startup probe failure threshold (#1086)` ← interesting overlap with our `Relax startup probe`
  - `feat: dropping support for v1.27 (#1127)` ← potential conflict with our `strip v1.35-only kubelet config fields for older tenant versions`
  - `fix(datastore): consistent password update if user exists (#1097)`
  - `fix: reinit kubelet configuration upon patch for op remove (#1099)`
  - `feat: kubelet configuration json patching (#1052)`
  - `feat(deps): bump controller-runtime 0.22.4 → 0.23.3 (#1125)`
  - `feat(deps): bump k8s.io/kubernetes to v1.36 (#1107)`
- 9 of our customization commits, all preserved post-merge:
  | Commit | What |
  |---|---|
  | `f4bc98b` | use kube-dc external endpoint services for cross-VPC access |
  | `6c639f1` | auto-add external endpoint DNS to API cert SANs (later reverted by ae43ebd) |
  | `967ddab` + `2d63c5a` | add ServerName to TLS config for external endpoint DNS |
  | `ae43ebd` | remove -ext DNS from cert validation, validate via ServerName instead |
  | `020b98d` | cleanup debug logging |
  | `8749302` | relax CP startup probe (`failureThreshold: 3 → 10`, `timeoutSeconds: 1 → 5`) |
  | `58407f3` | strip v1.35-only kubelet config fields for older tenant versions |
  | `465f762` | `ResizePolicy: NotRequired` on CP containers (in-place vertical scaling) |
- 1 chart-pin commit: `71473b4 chart: pin appVersion=edge-26.02.11-v3-kube-dc, version=1.0.0-kube-dc, kamaji-etcd@0.15.0`

Auto-merge succeeded on three files where both sides had changes:
`internal/builders/controlplane/deployment.go`, `internal/datastore/connection.go`,
`internal/kubeadm/uploadconfig.go`. All our customization markers
verified intact post-merge (`grep` for `ServerName`, `-ext`,
`ResizePolicy=NotRequired`, `failureThreshold: 10`, etc.).

`go build ./...` is clean.

---

## Rollback path

The merge is **only** on the test branch. Until promotion, rollback is "do nothing".

Snapshot of the pre-merge state preserved as the tag
`pre-upstream-merge-snapshot-20260509` (pushed to fork). To roll back at
any future point:

```bash
git checkout kube-dc-external-endpoints
git reset --hard pre-upstream-merge-snapshot-20260509
git push --force-with-lease origin kube-dc-external-endpoints
```

For the fleet side (after image is published and pinned), reverting is
just bumping `KAMAJI_IMAGE_TAG` back to `edge-26.02.11-v3-kube-dc` in
`kube-dc-fleet/clusters/<cluster>/cluster-config.env`.

---

## Build + release steps (when ready to promote)

These commands are NOT auto-run from the agent — they need explicit
operator authorization for the registry pushes.

### 1. Promote test branch

```bash
cd /home/voa/projects/kamaji
git checkout kube-dc-external-endpoints
git merge --ff-only kube-dc-external-endpoints-merge-test
git push origin kube-dc-external-endpoints
git branch -d kube-dc-external-endpoints-merge-test
git push origin --delete kube-dc-external-endpoints-merge-test
```

### 2. Build + push controller image

```bash
cd /home/voa/projects/kamaji
TAG=edge-26.5.2-v1-kube-dc  # 26.5.2 = upstream tag on this merge

# Build with ko, push to docker.io/shalb/kamaji
make build \
  CONTAINER_REPOSITORY=docker.io/shalb/kamaji \
  VERSION=${TAG} \
  KO_LOCAL=false \
  KO_PUSH=true

# Verify
docker pull docker.io/shalb/kamaji:${TAG}
```

### 3. Bump chart appVersion (if image tag changed)

```bash
sed -i "s/^appVersion: .*/appVersion: ${TAG}/" charts/kamaji/Chart.yaml
sed -i "s/^version: .*/version: 1.0.1-kube-dc/" charts/kamaji/Chart.yaml
```

### 4. Build + push chart to OCI registry

```bash
helm package charts/kamaji
helm push kamaji-1.0.1-kube-dc.tgz oci://registry-1.docker.io/shalb
```

### 5. Update fleet pin (per-cluster, gradual rollout)

Stage first:
```bash
cd /home/voa/projects/kube-dc-fleet
sed -i 's/KAMAJI_IMAGE_TAG=.*/KAMAJI_IMAGE_TAG=edge-26.5.2-v1-kube-dc/' clusters/stage/cluster-config.env
sed -i 's/^KAMAJI_VERSION=.*/KAMAJI_VERSION=1.0.1-kube-dc/' clusters/stage/cluster-config.env  # if pinned
git add -p && git commit -m "stage: bump kamaji 1.0.0-kube-dc → 1.0.1-kube-dc (upstream merge to 26.5.2-edge-3)"
git push origin main
flux reconcile kustomization platform --kubeconfig=$STAGE_KUBECONFIG
```

Verify on stage:
- Tenant control planes still reach Ready
- `kubectl logs -n kamaji-system deploy/kamaji` clean (no informer wedge)
- An e2e test (`make test-e2e`) passes

If stage holds for 24h, repeat for `clusters/cs/zrh` then `clusters/cloud`.

### 6. Verify Bug E behavior

The merge does NOT yet include a Bug E fix (the soot manager's per-TCP
informer wedging on unreachable tenant API). After the merge is on a
cluster, set up the repro:
1. Suspend one canceled-org's KdcCluster (replicas=0 on the rendered
   Deployment via kube-dc lifecycle).
2. Watch `kubectl logs -n kamaji-system deploy/kamaji` for repeated
   `controller-runtime.source.Kind: failed to get informer from cache`.
3. Confirm `tcp.Status.Kubernetes.Version.Status` stays empty (not
   transitioning to `VersionSleeping` because the deployment status
   reconciler can't reach the apiserver to refresh).

If reproducible, ship a focused fix (separate PR upstream + cherry-pick
to our fork). Suggested fix design: in `controllers/soot/manager.go`,
add a per-TCP informer-readiness watchdog that calls `m.cleanup()` when
the source.Kind cache hasn't synced after `Y` seconds. PRD context:
`/home/voa/projects/kube-dc/docs/prd/tenant-cluster-teardown-gc.md` §9.

### 7. Upstream PR (after stage validation)

After at least 7 days on stage with no regression, consider opening an
upstream PR for the parts of our customization that benefit upstream
broadly:
- Probe relax + `ResizePolicy=NotRequired` are general-purpose
- ServerName-for-TLS could be a PR if reframed as "support arbitrary
  external endpoints"
- The kubelet-config strip is specific to multi-version support

The external-endpoint customizations are kube-dc-specific and stay in
the fork.

---

## Files touched in the merge auto-resolution

(For reviewer reference — these are the ones where 3-way merge had to
combine both sides.)

- `internal/builders/controlplane/deployment.go` — upstream added
  `feat: configurable startup probe failure threshold`,
  `feat: advertiseAddress to NetworkProfile`, and
  `feat: multi strings arguments for kube-apiserver`. Our hardcoded
  `failureThreshold: 10` survives because it's the literal value in the
  default StartupProbe block; upstream's `Probes.Startup` override
  applies on top via `applyProbeOverrides`.
- `internal/datastore/connection.go` — upstream added
  `fix: consistent password update if user exists`. Our
  `cc.TLSConfig.ServerName = cc.Endpoints[0].Host` calls survive on the
  three datastore variants we touched.
- `internal/kubeadm/uploadconfig.go` — upstream added
  `feat: dropping support for v1.27` + `fix: reinit kubelet config on patch op remove`.
  Our v1.35-field-stripping logic remains intact at the same insertion
  point; verify with the merged `kubelet config` test that's still
  passing locally.

---

# Merge #2 — clastix/kamaji `26.8.5-edge` (2026-08-31)

> **Status**: merged, built-locally-verified, on test branch
> `kube-dc-merge-26.8.5-edge` (from deployed tip `0ea073e`, chart
> 1.0.9-kube-dc / image edge-26.5.2-v13-kube-dc). NOT yet promoted,
> NOT yet built or released. Snapshot tag: `pre-upstream-merge-snapshot-20260831`.
> Full ledger and validation matrix: `kube-dc/docs/internal/upstream-vs-fork-strategy.md`.

## What's in the merge

- 69 upstream commits since merge-base `1ed13ce` (26.5.2-edge-3), notably:
  `fix: rework the trigger channels around how source.Channel consumes them (#1259)`,
  `fix(soot): restarting manager when cert rotates (#1191)`,
  `feat: configurable securityContext for containers and pod (#1181)`,
  `fix(deployment): render readiness probe for scheduler and controller-manager (#1193)`,
  `feat: dual stack support for pod and service cidrs (#1176)`,
  `fix(sec): escaping datastore user and schema in sql statements (#1239)`,
  `feat(datastore): triggering tcp reconciliation upon secret changes (#1161)`,
  `fix(kubeadm): call AllowAPIServerToAccessKubeletAPI ... v1.36.1 (#1172)`.
- Four of our commits were BACKPORTS of upstream PRs and are absorbed by the
  merge itself: #1152, #1160, #1165, #1172. Our `451e84a` (konnectivity
  args order) was already deduped to #1160 before this merge.
- Upstream tag chosen over `upstream/master` (5 commits newer, incl.
  `fix(kubeadm): restore kube-proxy CIDR (#1291)`) to stay on a tagged edge.

## Conflicts (7) and resolution

| File | Resolution |
|---|---|
| `controllers/soot/manager.go` | union: keep our `deploymentNotAvailable` teardown/guard, route probe and endpoint-observation ack; take upstream's `certificateSha` rotation case and its relocated `GetRESTClientConfig` (+`ErrMissingKubeconfigKey` requeue); drop our now-duplicate `GetRESTClientConfig`. |
| `internal/builders/controlplane/deployment.go` | upstream's `defaultProbe()` (adds scheduler/CM readiness); our relaxed STARTUP probe preserved as `relaxedStartupProbe()` at the same three sites. |
| `internal/builders/controlplane/konnectivity_server.go` | upstream (only comments differed). |
| `internal/resources/kubeadm_phases.go` | upstream (our change was the #1172 backport). |
| `go.mod` / `go.sum` | upstream + `go mod tidy`; `sigs.k8s.io/yaml` stays a direct dep (our kube-proxy JSON-patch code imports it). |
| `charts/kamaji/Chart.lock` | upstream (Chart.yaml auto-merged to kamaji-etcd `>=0.15.0`; fleet runs `kamaji-etcd.deploy=false`). |

## Verification performed

- `go build ./...`, `go vet ./...` clean; `make generate manifests` reproduces
  the auto-merged deepcopy + CRDs with zero diff.
- `go test` (envtest 1.31.0, ABSOLUTE `KUBEBUILDER_ASSETS`): api/v1alpha1
  (48 specs), 11 internal/* packages, controllers, controllers/soot — all ok.
  Note: `make test` via the pinned `bin/ginkgo` CLI is unreliable now (CLI vs
  library version mismatch after upstream bumped ginkgo); use plain `go test`.
- TenantControlPlane/DataStore CRDs: additive only (no property removed) —
  safe for the HelmRelease `crds: CreateReplace` path.
- `helm lint` + `helm template` with fleet values (incl.
  `tenantClientClusterIPCapability.enabled=true`) render.
- Differential lint vs pure upstream at the same linter version; fork-owned
  code fixed for every linter upstream is clean on (see commit
  "fork: bring kube-dc code up to upstream's lint bar").
- All fork feature markers present post-merge (external endpoints/ServerName,
  ResizePolicy, kube-proxy configurationJSONPatches, datastore
  managementEndpoints, soot watchdog + scale-to-zero guard, endpoint
  observation, kubeconfig endpoint drift, konnectivity agent hardening,
  kubelet v1.35 field strip, tenantClientClusterIPCapability).
- Residual delta vs upstream: 30 files (was 40 against the old base).

## Companion: cluster-api-control-plane-provider-kamaji

Builds against `../kamaji` via `replace`. Re-pointed on branch
`kube-dc-kubeproxy-kamaji-26.8.5` (commit `87ab646`): tidy + regenerated
KCP CRDs (gain upstream's securityContext fields; `configurationJSONPatches`
intact). Still provider v0.19.0 + our 3 commits; **v0.20.0 (api/v1alpha2,
CAPI v1beta2) is a separate migration and was not taken.** Next image tag:
`v0.19.0-kube-dc-v3`; fleet manifest regenerates from
`kustomize build config/default` + image sed + clusterctl var defaults.

## Release naming for this line

Image `edge-26.8.5-v1-kube-dc` (counter restarts per upstream tag), chart
`1.0.10-kube-dc`. Roll out stage -> cs/zrh -> cloud as in Merge #1 §5.
