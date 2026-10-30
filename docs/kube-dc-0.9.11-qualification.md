# Kube-DC 0.9.11 Kamaji candidate

This candidate retains the accepted Kube-DC Kamaji fork and refreshes its
Go security dependencies, including x/net 0.62.0. The local non-E2E package
suite passes with Kubernetes 1.35 envtest. The E2E suite now skips by default
and requires `KAMAJI_E2E_KUBECONFIG` to name an explicit disposable target;
it does not fall back to the operator's current cluster.

Keep the source revision with any built artifact and qualify it together
with control-plane provider 0.21 and the configured k8-manager API contract.
The local package tests do not qualify live provisioning or a customer
migration. Publication, storage and lifecycle acceptance remain release gates.

## Dependency security follow-up

The candidate selects gRPC 1.83.2, which includes the fix for
[HTTP/2 memory exhaustion](https://github.com/advisories/GHSA-vp52-pcj8-j9qc).
The complete Go package suite passes against explicit Kubernetes 1.35 envtest
assets with no default live kubeconfig. Runtime images must be rebuilt from
these dependency locks and qualified separately.

The etcd API/client modules are patched together to 3.7.1. This fixes
[the TLS listener handshake deadline issue](https://pkg.go.dev/vuln/GO-2026-6107)
reported by the assembled-image scan. The complete isolated Go suite passes
again. Image scanning and live acceptance remain separate gates.
