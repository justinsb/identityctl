package commands

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/justinsb/identityctl/pkg/gcp"
	"github.com/justinsb/identityctl/pkg/kube"
	"github.com/justinsb/identityctl/pkg/workloadidentity"
)

type gcpOptions struct {
	Project     string
	Pool        string
	Provider    string
	KubeContext string
}

func (o *gcpOptions) AddFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.Project, "project", "", "GCP project ID")
	cmd.Flags().StringVar(&o.Pool, "pool", "kubernetes", "workload identity pool ID")
	cmd.Flags().StringVar(&o.Provider, "provider", "", "workload identity pool provider ID (default: derived from the kubeconfig context name)")
	cmd.Flags().StringVar(&o.KubeContext, "kubecontext", "", "kubeconfig context to use (default: current context)")
	_ = cmd.MarkFlagRequired("project")
}

// resolveProvider fills in the provider ID from the kube context name if unset.
func (o *gcpOptions) resolveProvider(kubeClient *kube.Client) error {
	if o.Provider != "" {
		return nil
	}
	o.Provider = sanitizeResourceID(kubeClient.ContextName())
	if o.Provider == "" {
		return fmt.Errorf("could not derive a provider ID from kube context %q; pass --provider", kubeClient.ContextName())
	}
	return nil
}

var invalidResourceIDChars = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeResourceID converts a string into a valid workload identity pool
// provider ID (4-32 chars, lowercase letters, digits, hyphens).
func sanitizeResourceID(s string) string {
	s = invalidResourceIDChars.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
		s = strings.Trim(s, "-")
	}
	for len(s) != 0 && len(s) < 4 {
		s = s + "-x"
	}
	return s
}

// BuildGCPCommand returns the "gcp" command group.
func BuildGCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gcp",
		Short: "Manage OIDC federation with Google Cloud",
	}
	cmd.AddCommand(buildGCPInitCommand())
	cmd.AddCommand(buildGCPGrantCommand())
	cmd.AddCommand(buildGCPPodSpecCommand())
	return cmd
}

func buildGCPInitCommand() *cobra.Command {
	options := &gcpOptions{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a workload identity pool and OIDC provider for the cluster",
		Long: `Reads the cluster's OIDC discovery document and signing keys (JWKS) through
the Kubernetes API, then creates (or updates) a GCP workload identity pool and
OIDC provider. The JWKS is uploaded to GCP, so the cluster's issuer URL does
not need to be publicly reachable.

Re-running is safe, and refreshes the uploaded signing keys.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGCPInit(cmd.Context(), options)
		},
	}
	options.AddFlags(cmd)
	return cmd
}

func runGCPInit(ctx context.Context, options *gcpOptions) error {
	kubeClient, err := kube.NewClient(options.KubeContext)
	if err != nil {
		return err
	}
	if err := options.resolveProvider(kubeClient); err != nil {
		return err
	}

	discovery, err := kubeClient.Discover(ctx)
	if err != nil {
		return err
	}
	jwks, err := kubeClient.JWKS(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("cluster %q: issuer %s\n", kubeClient.ContextName(), discovery.Issuer)

	gcpClient, err := gcp.NewClient(ctx)
	if err != nil {
		return err
	}
	if err := gcpClient.EnsureServices(ctx, options.Project, gcp.RequiredServices); err != nil {
		return err
	}
	if err := gcpClient.EnsurePool(ctx, options.Project, options.Pool); err != nil {
		return err
	}
	fmt.Printf("workload identity pool %q ready\n", gcp.PoolName(options.Project, options.Pool))

	if err := gcpClient.EnsureProvider(ctx, options.Project, options.Pool, options.Provider, discovery.Issuer, jwks); err != nil {
		return err
	}
	fmt.Printf("provider %q ready (JWKS uploaded)\n", gcp.ProviderName(options.Project, options.Pool, options.Provider))
	return nil
}

func buildGCPGrantCommand() *cobra.Command {
	options := &gcpOptions{}
	var namespace, serviceAccount, bucket, role string
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Grant a Kubernetes service account a role on a GCS bucket",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			gcpClient, err := gcp.NewClient(ctx)
			if err != nil {
				return err
			}
			projectNumber, err := gcpClient.ProjectNumber(ctx, options.Project)
			if err != nil {
				return err
			}
			subject := gcp.KubernetesSubject(namespace, serviceAccount)
			member := gcp.PrincipalForSubject(projectNumber, options.Pool, subject)
			modified, err := gcpClient.EnsureBucketBinding(ctx, bucket, member, role)
			if err != nil {
				return err
			}
			if modified {
				fmt.Printf("granted %s on bucket %q to %s\n", role, bucket, member)
			} else {
				fmt.Printf("%s already has %s on bucket %q\n", member, role, bucket)
			}
			return nil
		},
	}
	options.AddFlags(cmd)
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace of the service account")
	cmd.Flags().StringVar(&serviceAccount, "serviceaccount", "", "Kubernetes service account name")
	cmd.Flags().StringVar(&bucket, "bucket", "", "GCS bucket name")
	cmd.Flags().StringVar(&role, "role", "roles/storage.objectAdmin", "IAM role to grant on the bucket")
	_ = cmd.MarkFlagRequired("namespace")
	_ = cmd.MarkFlagRequired("serviceaccount")
	_ = cmd.MarkFlagRequired("bucket")
	return cmd
}

func buildGCPPodSpecCommand() *cobra.Command {
	options := &gcpOptions{}
	cmd := &cobra.Command{
		Use:   "podspec",
		Short: "Print the pod spec additions a workload needs for GCP access",
		Long: `Prints the projected service account token volume a pod needs to
authenticate to GCP. The token's audience identifies the workload identity
provider, so workloads using the identityctl workloadidentity library need no
other configuration.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			kubeClient, err := kube.NewClient(options.KubeContext)
			if err != nil {
				return err
			}
			if err := options.resolveProvider(kubeClient); err != nil {
				return err
			}
			gcpClient, err := gcp.NewClient(ctx)
			if err != nil {
				return err
			}
			projectNumber, err := gcpClient.ProjectNumber(ctx, options.Project)
			if err != nil {
				return err
			}
			providerName := gcp.ProviderName(fmt.Sprintf("%d", projectNumber), options.Pool, options.Provider)
			fmt.Printf("Add the following to your pod spec:\n\n%s", podSpecSnippet(providerName))
			return nil
		},
	}
	options.AddFlags(cmd)
	return cmd
}

// TokenAudience returns the audience workloads must request in their projected
// service account tokens; it is the provider's default allowed audience.
func TokenAudience(providerName string) string {
	return "https://iam.googleapis.com/" + providerName
}

func podSpecSnippet(providerName string) string {
	return fmt.Sprintf(`  volumes:
  - name: gcp-token
    projected:
      sources:
      - serviceAccountToken:
          audience: %s
          path: token
  containers:
  - # your container:
    volumeMounts:
    - name: gcp-token
      mountPath: %s
      readOnly: true
`, TokenAudience(providerName), workloadidentity.TokenDir)
}
