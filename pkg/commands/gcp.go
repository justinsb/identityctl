package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/justinsb/identityctl/pkg/gcp"
	"github.com/justinsb/identityctl/pkg/kube"
)

// TokenMountPath is where the projected service account token is mounted in pods.
const TokenMountPath = "/var/run/secrets/identityctl"

// CredentialsMountPath is where the credential configuration is mounted in pods.
const CredentialsMountPath = "/etc/identityctl"

// CredentialsKey is the ConfigMap key (and file name) for the credential configuration.
const CredentialsKey = "credential-configuration.json"

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
	cmd.AddCommand(buildGCPCredentialsCommand())
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

func buildGCPCredentialsCommand() *cobra.Command {
	options := &gcpOptions{}
	var namespace, configMapName string
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Write a ConfigMap with the GCP credential configuration for workloads",
		Long: `Generates an external-account credential configuration for the cluster's
workload identity provider and stores it in a ConfigMap in the given
namespace. Prints the pod spec additions needed to use it.`,
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

			credentialConfiguration, err := buildCredentialConfiguration(providerName)
			if err != nil {
				return err
			}
			data := map[string]string{CredentialsKey: string(credentialConfiguration)}
			if err := kubeClient.ApplyConfigMap(ctx, namespace, configMapName, data); err != nil {
				return err
			}
			fmt.Printf("configmap %s/%s ready\n", namespace, configMapName)
			fmt.Printf("\nAdd the following to your pod spec:\n\n%s", podSpecSnippet(providerName, configMapName))
			return nil
		},
	}
	options.AddFlags(cmd)
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace to write the ConfigMap to")
	cmd.Flags().StringVar(&configMapName, "name", "gcp-credentials", "name of the ConfigMap to create")
	_ = cmd.MarkFlagRequired("namespace")
	return cmd
}

// buildCredentialConfiguration renders the external_account credential
// configuration that Google client libraries consume via
// GOOGLE_APPLICATION_CREDENTIALS. providerName uses the project number.
func buildCredentialConfiguration(providerName string) ([]byte, error) {
	type credentialSourceFormat struct {
		Type string `json:"type"`
	}
	type credentialSource struct {
		File   string                 `json:"file"`
		Format credentialSourceFormat `json:"format"`
	}
	type credentialConfiguration struct {
		Type             string           `json:"type"`
		Audience         string           `json:"audience"`
		SubjectTokenType string           `json:"subject_token_type"`
		TokenURL         string           `json:"token_url"`
		CredentialSource credentialSource `json:"credential_source"`
	}
	configuration := credentialConfiguration{
		Type:             "external_account",
		Audience:         "//iam.googleapis.com/" + providerName,
		SubjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
		TokenURL:         "https://sts.googleapis.com/v1/token",
		CredentialSource: credentialSource{
			File:   TokenMountPath + "/token",
			Format: credentialSourceFormat{Type: "text"},
		},
	}
	return json.MarshalIndent(configuration, "", "  ")
}

// TokenAudience returns the audience workloads must request in their projected
// service account tokens; it is the provider's default allowed audience.
func TokenAudience(providerName string) string {
	return "https://iam.googleapis.com/" + providerName
}

func podSpecSnippet(providerName, configMapName string) string {
	return fmt.Sprintf(`  volumes:
  - name: gcp-token
    projected:
      sources:
      - serviceAccountToken:
          audience: %s
          expirationSeconds: 3600
          path: token
  - name: gcp-credentials
    configMap:
      name: %s
  containers:
  - # your container...
    env:
    - name: GOOGLE_APPLICATION_CREDENTIALS
      value: %s/%s
    volumeMounts:
    - name: gcp-token
      mountPath: %s
      readOnly: true
    - name: gcp-credentials
      mountPath: %s
      readOnly: true
`, TokenAudience(providerName), configMapName, CredentialsMountPath, CredentialsKey, TokenMountPath, CredentialsMountPath)
}
