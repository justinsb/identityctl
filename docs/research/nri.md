# NRI-based credential injection

**Status: research — not implemented.** Captured 2026-07 as a candidate
future feature.

## Problem

Today, a workload using identityctl needs one pod spec change: a projected
`serviceAccountToken` volume whose audience is the workload identity
provider. That is already a small surface (no ConfigMaps, no env vars), but
it is per-installation — the audience embeds the project number and provider
name — so generic upstream manifests can't ship it verbatim.

The conventional way to remove it is a mutating admission webhook that
injects the volume (as EKS Pod Identity and Azure Workload Identity do).
This doc explores the alternative: injecting credentials at the node level
with NRI, so that a pod opts in with a single annotation and the pod spec
needs no cluster-specific content at all.

## Background: NRI

NRI (Node Resource Interface, `github.com/containerd/nri`) is a plugin
framework in the container runtime. Plugins connect to the runtime over
`/var/run/nri/nri.sock` (Go SDK: `github.com/containerd/nri/pkg/stub`) and
receive pod/container lifecycle callbacks. At `CreateContainer` a plugin can
adjust the container before it starts — including its **mounts** and
**environment** — and it receives full pod context: namespace, name, UID,
annotations, and the pod sandbox.

Availability: containerd 1.7 (experimental, disabled by default), enabled by
default since containerd 2.0; also supported by CRI-O. On plugin
(re)connection, NRI synchronizes the plugin with all existing pods and
containers, so a restarted plugin can reconcile its state.

## What NRI cannot do

The projected `serviceAccountToken` volume is a **kubelet** feature: kubelet
mints the token via the TokenRequest API and rotates it. NRI sits below
kubelet, so a plugin cannot request that projection. If NRI injects the
mount, it must also mint and rotate the token itself.

## Proposed design

A DaemonSet (`identityctl-nri`) running an NRI plugin:

1. **Opt-in**: pods carry an annotation, e.g. `identityctl.io/gcp: "true"`.
   No other pod spec changes.
2. **Token minting**: on `CreateContainer` for an opted-in pod, the plugin
   calls TokenRequest for the pod's service account with the provider
   audience, writes the token into a per-pod tmpfs directory on the node
   (e.g. `/run/identityctl/tokens/<pod-uid>/`), and adds a read-only bind
   mount of that directory at `/var/run/secrets/identityctl`.
3. **Rotation**: a background refresher re-mints each token at ~half
   lifetime, writing atomically (write to temp file, rename — the kubelet
   pattern) so the file updates in place under the bind mount. The
   `workloadidentity` library re-reads the file on every refresh, so
   rotation needs no cooperation from the workload.
4. **Bound tokens**: each TokenRequest sets `boundObjectRef` to the pod, so
   tokens are invalidated automatically when the pod is deleted — strictly
   better revocation than kubelet's own projection default.
5. **Cleanup**: on pod removal, delete the per-pod directory and stop the
   refresher. On plugin restart, rebuild refresher state from the NRI
   synchronization callback.
6. **Configuration**: the audience is node-level, not per-pod — a small
   config file (or the plugin's ConfigMap) holding the provider resource
   name, which `identityctl gcp init` (or an `identityctl gcp nri install`
   command that deploys the DaemonSet) writes. A per-pod annotation override
   is possible but probably unnecessary.

### Security

- **RBAC**: a naive plugin service account would need `create` on
  `serviceaccounts/token` cluster-wide; a compromised node could then mint
  tokens for any service account. Instead, the plugin should authenticate
  with the **kubelet's credentials** (it already runs privileged on the
  node): the NodeRestriction admission plugin limits a node identity to
  creating tokens only for pods scheduled on that node. Same trust domain
  as kubelet's own projection, no new powers.
- **Token storage**: per-pod directories must be tmpfs (never node disk),
  mode 0600-ish, cleaned up with the pod.
- **Audience separation** is preserved: injected tokens carry the provider
  audience, not the API server audience.

### Failure modes and observability

- NRI injection **fails open**: if the plugin is down, opted-in pods start
  without the mount, the `workloadidentity` library falls back to the ADC
  chain, and the workload fails at its first cloud call. Debuggable, but
  silent at pod start. (A webhook can be configured fail-closed.)
- The injected mount is not visible in the pod spec — `kubectl describe`
  shows nothing; inspection requires `crictl inspect` on the node. This is
  the price of bypassing the API server.

## Trade-offs versus a mutating admission webhook

| | Webhook | NRI |
|---|---|---|
| Token mint + rotation | kubelet, for free | reimplemented in plugin |
| Pod spec visibility | volume visible in spec | invisible; node-level only |
| Infrastructure | certs, admission config, availability | DaemonSet + NRI socket |
| Failure mode | configurable (fail closed/open) | fail open |
| Admission latency | on every pod create | none (node-local) |
| Runtime requirements | none | containerd ≥2.0 (or 1.7 opt-in) / CRI-O |
| RBAC | none beyond webhook | kubelet creds (NodeRestriction-scoped) |

The webhook is the conventional answer for general workloads. NRI's
distinctive win is having **no API-server admission dependency**, which
matters for workloads low in the stack — notably cloudetcd running as the
datastore *under* kube-apiserver, where a mutating webhook is a
chicken-and-egg deadlock.

## Bootstrap considerations

NRI removes the admission-path dependency, but TokenRequest still needs a
live API server. For the true bootstrap case — cloudetcd backing the only
API server, static pods, nothing serving TokenRequest yet — neither webhook,
kubelet projection, nor this design produces a token. That case needs
node-local identity (pre-provisioned credentials, or cloud instance
identity — e.g. federating the node's GCE/EC2 instance identity token) and
is out of scope here; worth its own research note if self-hosted control
planes become a target.

## Rejected alternative: federating the default token audience

Adding the cluster's default API-server audience to the provider's
`allowedAudiences` would make every pod's existing
`/var/run/secrets/kubernetes.io/serviceaccount/token` federate with zero
injection of any kind. Rejected: it breaks the audience/replay boundary —
every party that ever sees such a token (GCP STS included) holds a
credential valid against the Kubernetes API. Audience separation is the one
property worth keeping sacred.

## Open questions

- Multi-cloud: one plugin could inject several tokens (distinct audiences
  for GCP/AWS/...) into one pod, keyed by annotations, at marginal cost.
- Should `CreateContainer` block on the first TokenRequest (adds one API
  round-trip to pod start, guarantees the token is present) or inject the
  mount and let the refresher fill it (faster, racy first read)? Blocking
  seems right; NRI plugin timeouts bound the damage.
- Annotation schema: `identityctl.io/gcp: "true"` vs a generic
  `identityctl.io/inject: gcp,aws` list.
- Whether to surface injection state (e.g. plugin writes a pod annotation
  or event via its node identity) to recover some observability.

## References

- NRI: https://github.com/containerd/nri
- TokenRequest bound object refs:
  https://kubernetes.io/docs/reference/kubernetes-api/authentication-resources/token-request-v1/
- NodeRestriction:
  https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/#noderestriction
- Prior art (webhook-based): Azure Workload Identity, EKS Pod Identity
  webhook.
