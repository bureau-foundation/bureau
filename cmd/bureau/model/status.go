// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/bureau-foundation/bureau/cmd/bureau/cli"
	modelschema "github.com/bureau-foundation/bureau/lib/schema/model"
)

type statusParams struct {
	ModelConnection
	cli.JSONOutput
}

func statusCommand() *cli.Command {
	var params statusParams

	return &cli.Command{
		Name:    "status",
		Summary: "Show account quota usage",
		Description: `Display quota usage for all model service accounts. Shows current
daily and monthly spend alongside configured limits.

Accounts without quotas show as "unlimited". Spend values reset at
midnight UTC (daily) and the first of the month UTC (monthly).`,
		Usage: "bureau model status [flags]",
		Examples: []cli.Example{
			{
				Description: "Show quota status from the host",
				Command:     "bureau model status --service",
			},
			{
				Description: "JSON output for scripting",
				Command:     "bureau model status --json",
			},
		},
		Params:         func() any { return &params },
		Output:         func() any { return &modelschema.AccountStatusResponse{} },
		Annotations:    cli.ReadOnly(),
		RequiredGrants: []string{"command/model/status"},
		Run: func(ctx context.Context, args []string, logger *slog.Logger) error {
			client, err := params.connect()
			if err != nil {
				return err
			}

			ctx, cancel := callContext(ctx)
			defer cancel()

			var result modelschema.AccountStatusResponse
			if err := client.Call(ctx, modelschema.ActionStatus, nil, &result); err != nil {
				return err
			}

			if done, err := params.EmitJSON(result); done {
				return err
			}

			if len(result.Accounts) == 0 {
				logger.Info("no accounts registered")
				return nil
			}

			// Sort by account name for stable output.
			sort.Slice(result.Accounts, func(i, j int) bool {
				return result.Accounts[i].Name < result.Accounts[j].Name
			})

			writer := tabwriter.NewWriter(os.Stdout, 2, 0, 3, ' ', 0)
			fmt.Fprintf(writer, "ACCOUNT\tPROVIDER\tPROJECTS\tDAILY\tMONTHLY\n")
			for _, account := range result.Accounts {
				daily := formatSpendVsLimit(
					account.DailySpendMicrodollars,
					quotaDaily(account.Quota),
				)
				monthly := formatSpendVsLimit(
					account.MonthlySpendMicrodollars,
					quotaMonthly(account.Quota),
				)
				projects := strings.Join(account.Projects, ", ")
				if len(projects) > 30 {
					projects = projects[:27] + "..."
				}

				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					account.Name,
					account.Provider,
					projects,
					daily,
					monthly,
				)
			}
			return writer.Flush()
		},
	}
}

// formatSpendVsLimit formats "spend / limit" as dollar amounts.
// Zero limit is shown as "unlimited".
func formatSpendVsLimit(spendMicrodollars, limitMicrodollars int64) string {
	spend := fmt.Sprintf("$%.2f", float64(spendMicrodollars)/1_000_000)
	if limitMicrodollars == 0 {
		return spend + " / unlimited"
	}
	limit := fmt.Sprintf("$%.2f", float64(limitMicrodollars)/1_000_000)
	return spend + " / " + limit
}

// quotaDaily extracts the daily limit from a quota, returning 0 for nil.
func quotaDaily(quota *modelschema.Quota) int64 {
	if quota == nil {
		return 0
	}
	return quota.DailyMicrodollars
}

// quotaMonthly extracts the monthly limit from a quota, returning 0 for nil.
func quotaMonthly(quota *modelschema.Quota) int64 {
	if quota == nil {
		return 0
	}
	return quota.MonthlyMicrodollars
}
