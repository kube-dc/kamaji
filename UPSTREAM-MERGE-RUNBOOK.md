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

## Merge #2 — independent review and follow-up fixes (2026-08-31)

Codex (gpt-5.6-sol, xhigh) reviewed the merged tree against BOTH parents.
Round 1: every conflict resolution confirmed correct; verdict FIX on four
implementation items. Round 2 after the fixes: **SHIP to STAGE** (explicitly
not production approval). Raw reviews and the per-finding triage:
`kube-dc/docs/internal/kamaji-merge-26.8.5-codex-review.md` and
`upstream-vs-fork-strategy.md` (Implementation log).

Fix commits on this branch: `3ab42a8` (soot: pause gate = `spec.replicas == 0`
only — the old `availableReplicas` predicate collided with upstream #1193's
new scheduler/CM readiness probes; 30s wake-up requeue while paused;
goroutine-local `startErr` instead of Reconcile's named `err`; lifecycle
cases before client-identity cases in the running-manager switch;
DataStore fingerprint compared before the live `Check`; dead
`EndpointModeDirect` removed) and the watchdog comment correction.
Provider: `56c8aa9` aligns its k8s.io replace block with kamaji's (the
inherited v0.19.0 block had pinned apiserver/kubelet/kube-proxy to v0.35.0).

Gate dispositions that changed:
- Pre-render DataStore acknowledgement (round-1 CRITICAL): NOT a merge
  blocker — no deployed k8-manager reads the observation annotations (only
  the unmerged `codex/dual-home-productize` branch does). It is a HARD
  PRE-ACTIVATION GATE for that consumer: acknowledgements must be
  audience-specific and prove the intended rendered generation rolled out.
- Watchdog robustness (persist failure state before cancel, jittered probes,
  429/throttling not counted as unreachability): STAGE-EXIT gate before
  production.
- Konnectivity `/readyz` on the shipped agent image: unchanged stage gate.

Stage acceptance list = the MUST-VERIFY rows of the round-1 review
(endpoint/certificate state machine; pause→resume 1→0→1 with TCP untouched
and scheduler/CM-only unreadiness NOT tearing soot down; DataStore route and
rotation fan-out; admin kubeconfig drift incl. IPv6; rendered control plane
and addons on 1.34/1.35 tenants; chart capability handshake).

## Merge #2 — stage attempt #1 (2026-08-31 12:51–13:05 UTC): BLOCKED, rolled back

Published and pulled back: image `shalb/kamaji:edge-26.8.5-v1-kube-dc`
(sha256:ae18e7e8…), chart `1.0.10-kube-dc` (sha256:0b3cbbdb…). Stage pinned
(fleet `aff751c`), HelmRelease upgraded cleanly (CRDs additive, webhooks
answering, soot + webhook servers up) — but the **main TenantControlPlane
controller never started**:

    ERROR Could not wait for Cache to sync   kind source: *v1.TLSRoute: timed out
    ERROR setup problem running manager      -> exit, 4 restarts in 7 min

Root cause: upstream #1162 (gateway-api 1.5.1) owns `TLSRoute` at **v1**;
every kube-dc cluster's Gateway API CRDs come from Envoy Gateway v1.7.x
(bundle **v1.4.1**), which serves TLSRoute only as v1alpha2/v1alpha3 (HTTPRoute,
GRPCRoute, Gateway are v1). Verified identical on stage, cloud, webdock,
cs/zrh, cs/crk — this would have failed fleet-wide. The tenant control plane
kept serving throughout (its Deployment was never re-rendered). Rolled back
with fleet `aed69dd` (chart 1.0.8 / image v12): kamaji Running, 0 restarts,
TCP Ready, workers started.

Fix in the fork: `e070188` — own each Gateway API kind only when it is served
at v1 (per-kind discovery; TLSRoute absent → logged, not owned; the gateway
webhook already refuses the Gateway feature then; no TCP on the fleet uses
it). Upstream PR candidate. Platform-side alternative: Envoy Gateway v1.8.x
(bundle v1.5.1, TLSRoute v1 as storage version) — separate change.

Re-attempt uses image `edge-26.8.5-v2-kube-dc`, chart `1.0.11-kube-dc`.
Also noted on stage: two `Failed`-phase pods of an old b1-cp ReplicaSet on
master-1 (36–40 days old) — pre-existing garbage, unrelated.

## Merge #2 — stage attempt #2 (2026-08-31 13:08 UTC): ON STAGE, soak running

Image `shalb/kamaji:edge-26.8.5-v2-kube-dc` (sha256:b29fdd9d…), chart
`1.0.11-kube-dc` (sha256:88fb6313…), fleet `fb067ce`. Codex GO (round 4).
Startup gate: 0 restarts over 341 s, TCP workers started, the expected
`TLSRoute is not served` INFO line, no cache-sync errors. Re-render: b1-cp
Deployment gen 13→14 with scheduler/CM readiness (upstream #1193), fork startup
5s/10 and k8-manager apiserver 4/10 intact. Pause test: kamaji restores a raw
Deployment scale-to-0 within ~1 s, so the CP pod bounced; soot tore down on API
loss and restarted automatically 31 s after the pod returned (one failed start
recovered via the failed marker; no stale marker). Kubeconfig checksum changed
once (upstream #1191 checksum inputs; one soot rebuild) then stable ≥10 min.
Tenant-side checks were blocked by the stage tenant's pre-existing dead CNI
(Cilium crashlooping since 2026-07-18) — not a kamaji regression. Full gate
table: `kube-dc/docs/internal/upstream-vs-fork-strategy.md`.

Rollback: `KAMAJI_VERSION=1.0.8-kube-dc`, `KAMAJI_IMAGE_TAG=edge-26.5.2-v12-kube-dc`.
Next hops (cs/zrh, then cloud) and the provider v3 / shared-manifest change each
need a separate approval after ≥24 h on stage.

## Merge #2 — cs/zrh canary (2026-08-31 14:18 UTC): GREEN

Operator-directed hop ~1 h after stage (Codex conditional GO, round 5). Fleet
`6d3c9ab`. Startup gate clean (378 s, 0 restarts, TLSRoute INFO line); all 9
existing TCPs (incl. customer clusters) re-rendered exactly once and converged;
kubeconfig checksums changed once each; datastores 11/11 Ready; a customer
created a new cluster mid-window successfully. Full tenant E2E on a throwaway
cs-tsap cluster: CP Ready 3 min, worker join, konnectivity logs/exec, DNS
int+ext, kube-proxy ClusterIP + the fork's configurationJSONPatches applied,
port-forward 60/60 over 10 min, CSI PVC persistence across pod recreate AND
worker replacement, teardown 43 s. LoadBalancer untestable: account static
pool exhausted (CCM refuses dynamic IPs by design). Two pre-existing platform
findings recorded in `upstream-vs-fork-strategy.md`: fresh-CP VPA bootstrap
OOM (control-proven on v13), and the single-worker drain deadlock on the CSI
VolumeAttachment finalizer. Remaining: cloud (remove its TEMPORARY provider
freeze in a window), provider v3 + shared manifest, pins, upstream PRs.

---

# Merge #3 — clastix/kamaji `26.9.3-edge` (2026-09-20): Kubernetes 1.37

**Why:** the worker-image catalogs in all four CloudSigma regions had already
been given a v1.37.0 entry and customers were told 1.37 was available, but only
the node images were ever built. The fork could not host a 1.37 control plane:
`internal/upgrade/kubeadm_version.go` pinned `KubeadmVersion = "v1.36.0"`, and
`internal/webhook/handlers/tcp_version.go` refuses any TenantControlPlane above
that ceiling. Picking 1.37 in the console produced a **silent drift**, not an
error — see "The failure this fixes" below.

Merged at upstream master `1c3d84a`, four commits past the `26.9.3-edge` tag.

## What's in the merge

- `KubeadmVersion` v1.36.0 → **v1.37.0** (the whole point).
- k8s libraries 1.36 → **1.37** (`k8s.io/kubernetes v1.37.0`, `k8s.io/* v0.37.0`,
  `controller-runtime v0.24.1 → v0.25.1`).
- Additive API surface: `konnectivity.agent.resources`, and stricter CEL on
  existing fields — `serviceCidrs`/`podCidrs` may not hold two CIDRs of the same
  IP family; `kubelet.preferredAddressTypes` capped at 5 unique entries
  (`listType` set → atomic).
- `DefaultKubernetesVersion` (e2e only) v1.35.7 → v1.37.0.

## Conflicts (3) and resolution

| File | Resolution |
|---|---|
| `go.mod` | Took upstream's versions wholesale, then `go mod tidy`. |
| `charts/kamaji-crds/hack/…_datastores_spec.yaml` | Regenerated, not hand-merged. |
| `charts/kamaji/crds/…_datastores.yaml` | Regenerated, not hand-merged. |

Both datastore CRDs were rebuilt with `make manifests` from the merged Go types,
so our `managementEndpoints` field and upstream's new IPv6-bracket CEL rule both
survive — and the rule now also covers `managementEndpoints`, which is what we
want (it is the same shape of field).

## Verification performed

- `go build`, `go vet`, full unit suite **including** the `api/v1alpha1` envtest
  specs (59/59). That suite needs `KUBEBUILDER_ASSETS`; without it the only
  failure you will see is `fork/exec …/etcd: no such file or directory`:
  `bin/setup-envtest use 1.31.0 --bin-dir $PWD/bin -p path`.
- **Pre-flight against the new CEL rules before rolling:** every live
  TenantControlPlane on stage, cs/zrh, cs/crk, cs/jed and cs/next (43 of them)
  was checked for same-family CIDR pairs and for oversized/duplicate
  `preferredAddressTypes`. Zero violations. Do this every time upstream tightens
  validation — the HelmRelease uses `crds: CreateReplace`, so a stricter schema
  lands immediately and would start rejecting **updates** to an offending object.
- Per cluster after the roll: live Deployment image (never the HR condition
  alone), pod ready + 0 restarts, CRD carries the new rules, 0 error lines in
  5 minutes of controller log, and a server dry-run probe:
  v1.37.0 create **admitted**, v1.38.0 create **still denied**.

## Release artifacts

| Artifact | Tag |
|---|---|
| `shalb/kamaji` | `edge-26.9.3-v1-kube-dc` |
| chart `shalb/kamaji` | `1.0.12-kube-dc` |
| `shalb/cluster-api-control-plane-provider-kamaji` | `v0.19.0-kube-dc-v3` |

Branches: `kube-dc-merge-1.37` (fork), `kube-dc-kamaji-1.37` (provider).

## Companion provider: bumped, but NOT required for 1.37

The provider fork has **no version ceiling of its own** — it copies
`kcp.Spec.Version` straight into the TenantControlPlane
(`controllers/kamajicontrolplane_controller_tcp.go`). So the kamaji image alone
unblocks 1.37, and `v0.19.0-kube-dc-v3` is library alignment: the provider
builds against the fork through `replace github.com/clastix/kamaji => ../kamaji`,
so its module graph must move to k8s 0.37 / controller-runtime 0.25.1 or it
stops compiling. `sigs.k8s.io/cluster-api` stays at v1.11.6 (CAPI v1beta2 /
provider v0.20.0 remains a separate decision).

The image was built on the host, not through dagger: the in-cluster engine
cannot reach `proxy.golang.org`. Same recipe as the dagger one — static
linux/amd64 binary on `shalb/distroless-static:nonroot` at `/manager`, user
65532 — then `docker push`.

**The fleet IS pinned to v3 (done 2026-09-20).**
`infrastructure/capi/providers/kamaji-controlplane-components-kube-dc.yaml` is
shared by cs/zrh, cs/crk, cs/jed, cs/next, cloudacropolis and stage, so all six
roll together. Regenerating it is **not** a plain image swap, so do it this way:

1. `kustomize build config/default` from the provider worktree.
2. Re-expand the two clusterctl variables to the defaults this file has always
   carried — `--feature-gates=…=false` and an empty
   `--dynamic-infrastructure-clusters=`. A raw build leaves `${CACPPK_*}`
   placeholders that Flux substitution does not fill.
3. Swap the image to the kube-dc tag; keep the three leading kustomize warning
   comments so the diff stays readable.
4. Diff against the committed copy and expect **only** the image line plus
   additive CRD schema. Check the object count is unchanged (11) and that no new
   CEL rules appeared — a rule added to the KamajiControlPlane CRD could
   invalidate live objects the way the kamaji CRDs can.

Because cloudacropolis also consumes this file, it necessarily moved to a v3
provider; its kamaji was bumped to 1.0.12-kube-dc in the same session so the
pair matches. cloud and webdock run no CAPI kamaji provider and stay on
edge-26.8.5-v2 deliberately.

`sigs.k8s.io/cluster-api` stays at v1.11.6 — CAPI v1beta2 / provider v0.20.0
remains a separate decision.

## The failure this fixes (worth recognising again)

`cs-m-yassin-0a5cae54/tst-cls2-myassin` in jed sat wrong for three days and
nothing alerted:

- KdcCluster `spec.version: v1.37.0`, **`status.phase: Ready`**
- KamajiControlPlane spec v1.37.0, status v1.36.2, with the real reason buried in
  `TenantControlPlaneCreated=False … admission webhook denied`
- TenantControlPlane still v1.36.2, node kubelet still v1.36.2
- a v1.37.0 MachineSet created 09-17 stuck at 0 replicas, held by CAPI's
  `ControlPlaneIsStable` preflight (spec ≠ status) — which is the one thing that
  kept it safe: no worker ever ran ahead of its control plane.

`status.controlPlane.version` did report v1.36.2 truthfully. What is missing is
a KdcCluster **condition** carrying the KCP's rejection, and a `phase` that is
not `Ready` while the control plane has refused the requested version. Until
that exists, a version the fork does not support fails quietly.

The moment the new image rolled in jed the whole chain unblocked itself with no
manual step: TCP → v1.37.0 Ready, KCP status caught up, the blocked v1.37.0
MachineSet scaled to 1, the old v1.36.2 set went to 0, and the tenant came back
healthy (Cilium v1.16.5, CoreDNS, konnectivity, CSI all Running; DNS + a
CloudSigma NVMe PVC smoke-tested green).

## Known gap carried forward

No cluster-autoscaler exists for 1.37 (every `v1.37.x` 404s on registry.k8s.io
as of 2026-09-20). An autoscaled 1.37 cluster runs CA `v1.36.1` through
k8-manager's nearest-lower fallback and emits a `ClusterAutoscalerVersionSkew`
warning event. That is intended — do not silence it by pinning 1.37 to a 1.36
tag.
