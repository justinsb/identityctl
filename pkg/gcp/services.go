package gcp

import (
	"context"
	"fmt"

	"google.golang.org/api/serviceusage/v1"
	"k8s.io/klog/v2"
)

// RequiredServices are the APIs needed for workload identity federation and
// GCS access.
var RequiredServices = []string{
	"iam.googleapis.com",
	"sts.googleapis.com",
	"iamcredentials.googleapis.com",
	"cloudresourcemanager.googleapis.com",
	"storage.googleapis.com",
}

// EnsureServices enables the given services on the project if they are not
// already enabled.
func (c *Client) EnsureServices(ctx context.Context, projectID string, services []string) error {
	log := klog.FromContext(ctx)

	serviceUsage, err := serviceusage.NewService(ctx)
	if err != nil {
		return fmt.Errorf("building serviceusage client: %w", err)
	}

	parent := "projects/" + projectID
	var toEnable []string
	for _, service := range services {
		log.Info("checking if service is enabled", "service", parent+"/services/"+service)
		state, err := serviceUsage.Services.Get(parent + "/services/" + service).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("getting state of service %q: %w", service, err)
		}
		if state.State != "ENABLED" {
			log.Info("service is not enabled, adding to enable list", "service", service)
			toEnable = append(toEnable, service)
		}
	}
	if len(toEnable) == 0 {
		return nil
	}

	log.Info("enabling services %v on project %s", toEnable, projectID)
	operation, err := serviceUsage.Services.BatchEnable(parent, &serviceusage.BatchEnableServicesRequest{
		ServiceIds: toEnable,
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("enabling services %v: %w", toEnable, err)
	}

	return waitFor(ctx, fmt.Sprintf("services %v to be enabled", toEnable), func() (bool, error) {
		latest, err := serviceUsage.Operations.Get(operation.Name).Context(ctx).Do()
		if err != nil {
			return false, err
		}
		if latest.Done && latest.Error != nil {
			return false, fmt.Errorf("enable operation failed: %s", latest.Error.Message)
		}
		return latest.Done, nil
	})
}
