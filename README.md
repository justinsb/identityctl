# identityctl

`identityctl` sets up OIDC federation between a Kubernetes cluster and cloud
providers, so that Kubernetes service accounts can be granted cloud IAM
permissions directly — no long-lived keys, no per-workload cloud service
accounts. GCP is supported today (via [workload identity
federation](https://cloud.google.com/iam/docs/workload-identity-federation));
AWS and friends are planned.

The cluster's OIDC issuer does **not** need to be publicly reachable:
`identityctl` reads the cluster's signing keys (JWKS) through the Kubernetes
API and uploads them to the cloud provider.

## Quick start (GCP)

Requirements: a working kubeconfig, and application default credentials
(`gcloud auth application-default login`) with admin permissions on the
target project.

### 1. Initialize federation for the cluster

```sh
identityctl gcp init --project my-project
```

This enables the required GCP APIs, creates a workload identity pool
(default: `kubernetes`) and an OIDC provider named after your kubeconfig
context, and uploads the cluster's JWKS. Re-running is safe, and refreshes
the uploaded signing keys (do this after the cluster rotates its keys).

### 2. Grant a Kubernetes service account access to a bucket

```sh
identityctl gcp grant --project my-project \
  --namespace cloudetcd --serviceaccount cloudetcd \
  --bucket my-backup-bucket
```

This grants `roles/storage.objectAdmin` (override with `--role`) on the
bucket to the federated principal
`system:serviceaccount:cloudetcd:cloudetcd`. Access is direct — no
intermediate GCP service account or impersonation.

### 3. Give the workload credentials

```sh
identityctl gcp credentials --project my-project --namespace cloudetcd
```

This writes a `gcp-credentials` ConfigMap into the namespace containing an
[external account credential
configuration](https://cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes),
and prints the pod spec additions needed: a projected service account token
volume (with the provider as audience) and
`GOOGLE_APPLICATION_CREDENTIALS` pointing at the mounted configuration.
Google client libraries pick this up automatically and exchange the
Kubernetes token for GCP credentials transparently.

## How it works

1. `init` fetches `/.well-known/openid-configuration` and `/openid/v1/jwks`
   from the Kubernetes API server, then creates a GCP workload identity pool
   and OIDC provider with the issuer URL and uploaded JWKS. Subjects are
   mapped from the token's `sub` claim
   (`system:serviceaccount:<namespace>:<name>`).
2. `grant` adds an IAM binding on a GCS bucket for
   `principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/subject/<subject>`.
3. At runtime, the pod's projected token (audience =
   `https://iam.googleapis.com/<provider>`) is exchanged at
   `sts.googleapis.com` for a federated access token, which GCS accepts
   directly.
