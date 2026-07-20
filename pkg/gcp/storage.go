package gcp

import (
	"context"
	"fmt"

	"google.golang.org/api/storage/v1"
)

// KubernetesSubject returns the OIDC subject for a Kubernetes service account.
func KubernetesSubject(namespace, serviceAccount string) string {
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount)
}

// PrincipalForSubject returns the IAM principal identifier for a single
// subject in a workload identity pool. Note that IAM principal identifiers
// use the project number, not the project ID.
func PrincipalForSubject(projectNumber int64, poolID, subject string) string {
	return fmt.Sprintf("principal://iam.googleapis.com/projects/%d/locations/global/workloadIdentityPools/%s/subject/%s",
		projectNumber, poolID, subject)
}

// EnsureBucketBinding grants role to member on the bucket, if not already granted.
// Returns true if the policy was modified.
func (c *Client) EnsureBucketBinding(ctx context.Context, bucket, member, role string) (bool, error) {
	storageService, err := storage.NewService(ctx)
	if err != nil {
		return false, fmt.Errorf("building storage client: %w", err)
	}

	policy, err := storageService.Buckets.GetIamPolicy(bucket).Context(ctx).Do()
	if err != nil {
		return false, fmt.Errorf("getting IAM policy for bucket %q: %w", bucket, err)
	}

	for _, binding := range policy.Bindings {
		if binding.Role != role {
			continue
		}
		for _, existingMember := range binding.Members {
			if existingMember == member {
				return false, nil
			}
		}
		binding.Members = append(binding.Members, member)
		if _, err := storageService.Buckets.SetIamPolicy(bucket, policy).Context(ctx).Do(); err != nil {
			return false, fmt.Errorf("setting IAM policy for bucket %q: %w", bucket, err)
		}
		return true, nil
	}

	policy.Bindings = append(policy.Bindings, &storage.PolicyBindings{
		Role:    role,
		Members: []string{member},
	})
	if _, err := storageService.Buckets.SetIamPolicy(bucket, policy).Context(ctx).Do(); err != nil {
		return false, fmt.Errorf("setting IAM policy for bucket %q: %w", bucket, err)
	}
	return true, nil
}
