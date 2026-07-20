package gcp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iam/v1"
)

// Client wraps the GCP services needed for workload identity federation.
type Client struct {
	iam *iam.Service
	crm *cloudresourcemanager.Service
}

// NewClient builds a GCP client using application default credentials.
func NewClient(ctx context.Context) (*Client, error) {
	iamService, err := iam.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("building IAM client: %w", err)
	}
	crmService, err := cloudresourcemanager.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("building cloudresourcemanager client: %w", err)
	}
	return &Client{iam: iamService, crm: crmService}, nil
}

// ProjectNumber looks up the numeric project number for a project ID.
func (c *Client) ProjectNumber(ctx context.Context, projectID string) (int64, error) {
	project, err := c.crm.Projects.Get("projects/" + projectID).Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("getting project %q: %w", projectID, err)
	}
	// project.Name is "projects/<number>"
	numberString := strings.TrimPrefix(project.Name, "projects/")
	number, err := strconv.ParseInt(numberString, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing project number from %q: %w", project.Name, err)
	}
	return number, nil
}

// PoolName returns the full resource name of a workload identity pool.
func PoolName(projectID, poolID string) string {
	return fmt.Sprintf("projects/%s/locations/global/workloadIdentityPools/%s", projectID, poolID)
}

// ProviderName returns the full resource name of a workload identity pool provider.
func ProviderName(projectID, poolID, providerID string) string {
	return PoolName(projectID, poolID) + "/providers/" + providerID
}

// EnsurePool creates the workload identity pool if it does not exist,
// undeleting it if it was soft-deleted.
func (c *Client) EnsurePool(ctx context.Context, projectID, poolID string) error {
	name := PoolName(projectID, poolID)
	pool, err := c.iam.Projects.Locations.WorkloadIdentityPools.Get(name).Context(ctx).Do()
	if err == nil {
		if pool.State == "DELETED" {
			if _, err := c.iam.Projects.Locations.WorkloadIdentityPools.Undelete(name,
				&iam.UndeleteWorkloadIdentityPoolRequest{}).Context(ctx).Do(); err != nil {
				return fmt.Errorf("undeleting workload identity pool %q: %w", name, err)
			}
			return c.waitForPool(ctx, name)
		}
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("getting workload identity pool %q: %w", name, err)
	}

	parent := fmt.Sprintf("projects/%s/locations/global", projectID)
	_, err = c.iam.Projects.Locations.WorkloadIdentityPools.Create(parent, &iam.WorkloadIdentityPool{
		DisplayName: poolID,
		Description: "Created by identityctl",
	}).WorkloadIdentityPoolId(poolID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("creating workload identity pool %q: %w", name, err)
	}
	return c.waitForPool(ctx, name)
}

func (c *Client) waitForPool(ctx context.Context, name string) error {
	return waitFor(ctx, fmt.Sprintf("workload identity pool %q", name), func() (bool, error) {
		pool, err := c.iam.Projects.Locations.WorkloadIdentityPools.Get(name).Context(ctx).Do()
		if isNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return pool.State == "ACTIVE", nil
	})
}

// EnsureProvider creates or updates an OIDC provider in the pool. The JWKS is
// uploaded so the issuer does not need to be publicly reachable; re-running
// refreshes the uploaded keys.
func (c *Client) EnsureProvider(ctx context.Context, projectID, poolID, providerID, issuer string, jwks []byte) error {
	name := ProviderName(projectID, poolID, providerID)
	desired := &iam.WorkloadIdentityPoolProvider{
		DisplayName: providerID,
		Description: "Created by identityctl",
		Oidc: &iam.Oidc{
			IssuerUri: issuer,
			JwksJson:  string(jwks),
		},
		AttributeMapping: map[string]string{
			"google.subject": "assertion.sub",
		},
	}

	existing, err := c.iam.Projects.Locations.WorkloadIdentityPools.Providers.Get(name).Context(ctx).Do()
	if err == nil {
		if existing.State == "DELETED" {
			if _, err := c.iam.Projects.Locations.WorkloadIdentityPools.Providers.Undelete(name,
				&iam.UndeleteWorkloadIdentityPoolProviderRequest{}).Context(ctx).Do(); err != nil {
				return fmt.Errorf("undeleting workload identity pool provider %q: %w", name, err)
			}
			if err := c.waitForProvider(ctx, name); err != nil {
				return err
			}
		}
		_, err := c.iam.Projects.Locations.WorkloadIdentityPools.Providers.Patch(name, desired).
			UpdateMask("oidc,attributeMapping").Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("updating workload identity pool provider %q: %w", name, err)
		}
		return c.waitForProvider(ctx, name)
	}
	if !isNotFound(err) {
		return fmt.Errorf("getting workload identity pool provider %q: %w", name, err)
	}

	_, err = c.iam.Projects.Locations.WorkloadIdentityPools.Providers.Create(PoolName(projectID, poolID), desired).
		WorkloadIdentityPoolProviderId(providerID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("creating workload identity pool provider %q: %w", name, err)
	}
	return c.waitForProvider(ctx, name)
}

func (c *Client) waitForProvider(ctx context.Context, name string) error {
	return waitFor(ctx, fmt.Sprintf("workload identity pool provider %q", name), func() (bool, error) {
		provider, err := c.iam.Projects.Locations.WorkloadIdentityPools.Providers.Get(name).Context(ctx).Do()
		if isNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return provider.State == "ACTIVE", nil
	})
}

func waitFor(ctx context.Context, description string, poll func() (bool, error)) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		done, err := poll()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", description, err)
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to become active", description)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func isNotFound(err error) bool {
	var apiError *googleapi.Error
	if errors.As(err, &apiError) {
		return apiError.Code == 404
	}
	return false
}
