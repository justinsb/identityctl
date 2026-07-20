package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/justinsb/identityctl/pkg/commands"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	root := &cobra.Command{
		Use:          "identityctl",
		Short:        "Set up OIDC federation between Kubernetes clusters and cloud providers",
		SilenceUsage: true,
	}
	root.AddCommand(commands.BuildGCPCommand())

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
