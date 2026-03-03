// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	"github.com/bureau-foundation/bureau/cmd/bureau/fleet"
)

type serviceInstancesParams struct {
	fleet.FleetConnection
	cli.JSONOutput
}

type serviceInstancesResult struct {
	Localpart string                  `json:"localpart"  desc:"service localpart"`
	Template  string                  `json:"template"   desc:"template reference"`
	Replicas  int                     `json:"replicas"   desc:"minimum replica count"`
	Failover  string                  `json:"failover"   desc:"failover strategy"`
	Instances []serviceInstancesEntry `json:"instances"  desc:"current placement instances"`
}

type serviceInstancesEntry struct {
	Machine   string `json:"machine"    desc:"machine hosting this instance"`
	Template  string `json:"template"   desc:"template on this instance"`
	AutoStart bool   `json:"auto_start" desc:"whether the instance starts automatically"`
}

func instancesCommand() *cli.Command {
	var params serviceInstancesParams

	return &cli.Command{
		Name:    "instances",
		Summary: "Show placement instances of a fleet-managed service",
		Description: `Display the current placement instances of a fleet-managed service.
Shows which machines host the service and the instance configuration.

Requires a reachable fleet controller.`,
		Usage: "bureau service instances <localpart> [flags]",
		Examples: []cli.Example{
			{
				Description: "Show instances of a service",
				Command:     "bureau service instances service/stt/whisper",
			},
			{
				Description: "Show instances as JSON",
				Command:     "bureau service instances service/stt/whisper --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &serviceInstancesResult{} },
		RequiredGrants: []string{"command/service/instances"},
		Annotations:    cli.ReadOnly(),
		Run: requireLocalpart("bureau service instances <localpart> [flags]", func(ctx context.Context, localpart string, logger *slog.Logger) error {
			return runInstances(ctx, localpart, logger, params)
		}),
	}
}

func runInstances(ctx context.Context, localpart string, logger *slog.Logger, params serviceInstancesParams) error {
	client, err := params.Connect()
	if err != nil {
		return err
	}

	ctx, cancel := fleet.CallContext(ctx)
	defer cancel()

	var response fleet.ShowServiceResponse
	if err := client.Call(ctx, "show-service", map[string]any{
		"service": localpart,
	}, &response); err != nil {
		return cli.Transient("querying service %q: %w", localpart, err).
			WithHint("Run 'bureau service list' to see available services.")
	}

	template := ""
	replicas := 0
	failover := ""
	if response.Definition != nil {
		template = response.Definition.Template
		replicas = response.Definition.Replicas.Min
		failover = string(response.Definition.Failover)
	}

	instances := make([]serviceInstancesEntry, len(response.Instances))
	for i, instance := range response.Instances {
		instanceTemplate := ""
		autoStart := false
		if instance.Assignment != nil {
			instanceTemplate = instance.Assignment.Template
			autoStart = instance.Assignment.AutoStart
		}
		instances[i] = serviceInstancesEntry{
			Machine:   instance.Machine,
			Template:  instanceTemplate,
			AutoStart: autoStart,
		}
	}

	result := serviceInstancesResult{
		Localpart: localpart,
		Template:  template,
		Replicas:  replicas,
		Failover:  failover,
		Instances: instances,
	}

	if done, err := params.EmitJSON(result); done {
		return err
	}

	// Text output.
	fmt.Printf("Service:    %s\n", localpart)
	if template != "" {
		fmt.Printf("Template:   %s\n", template)
	}
	if response.Definition != nil {
		fmt.Printf("Replicas:   %d (min) / %d (max)\n",
			response.Definition.Replicas.Min, response.Definition.Replicas.Max)
		fmt.Printf("Failover:   %s\n", response.Definition.Failover)
	}

	if len(instances) == 0 {
		fmt.Printf("\nNo instances running.\n")
		return nil
	}

	fmt.Println()
	writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "MACHINE\tTEMPLATE\tAUTO-START")
	for _, instance := range instances {
		autoStart := "no"
		if instance.AutoStart {
			autoStart = "yes"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\n", instance.Machine, instance.Template, autoStart)
	}
	writer.Flush()
	return nil
}
