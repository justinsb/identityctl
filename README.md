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

The only thing a pod needs is a projected service account token whose
audience is the workload identity provider; `identityctl gcp podspec
--project my-project` prints the exact snippet:

```yaml
  volumes:
  - name: gcp-token
    projected:
      sources:
      - serviceAccountToken:
          audience: https://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/kubernetes/providers/<cluster>
          path: token
  containers:
  - # your container:
    volumeMounts:
    - name: gcp-token
      mountPath: /var/run/secrets/identityctl
      readOnly: true
```

In the workload, use the `workloadidentity` package to get a token source:

```go
import (
    "cloud.google.com/go/storage"
    "google.golang.org/api/option"

    "github.com/justinsb/identityctl/pkg/workloadidentity"
)

tokenSource, err := workloadidentity.TokenSource(ctx)
client, err := storage.NewClient(ctx, option.WithTokenSource(tokenSource))
```

There is no other configuration: the token's own audience identifies the
workload identity provider, so the library derives the STS exchange from it.
If `GOOGLE_APPLICATION_CREDENTIALS` is set it takes precedence, and if
neither is present the library falls back to the normal application default
credentials chain — so the same binary runs unchanged on GKE, on GCE, or on
a developer machine.

## How it works

1. `init` fetches `/.well-known/openid-configuration` and `/openid/v1/jwks`
   from the Kubernetes API server, then creates a GCP workload identity pool
   and OIDC provider with the issuer URL and uploaded JWKS. Subjects are
   mapped from the token's `sub` claim
   (`system:serviceaccount:<namespace>:<name>`).
2. `grant` adds an IAM binding on a GCS bucket for
   `principal://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/subject/<subject>`.
3. At runtime, the pod's projected token (audience =
   `https://iam.googleapis.com/<provider>`, the provider's default allowed
   audience) is exchanged at `sts.googleapis.com` for a federated access
   token, which GCS accepts directly. The kubelet rotates the projected
   token automatically, and the library re-reads it on each refresh.
