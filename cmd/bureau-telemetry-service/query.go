// Copyright 2026 The Bureau Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"github.com/bureau-foundation/bureau/lib/codec"
	"github.com/bureau-foundation/bureau/lib/schema/telemetry"
	"github.com/bureau-foundation/bureau/lib/servicetoken"
)

// handleTraces returns spans matching the request filters.
// Grant: telemetry/traces.
func (s *TelemetryService) handleTraces(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/traces", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/traces")
	}

	var request telemetry.TracesRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding traces request: %w", err)
	}

	spans, err := s.store.QuerySpans(ctx, SpanFilter{
		TraceID:     request.TraceID,
		Machine:     request.Machine,
		Source:      request.Source,
		Operation:   request.Operation,
		MinDuration: request.MinDuration,
		Status:      request.Status,
		Start:       request.Start,
		End:         request.End,
		Limit:       request.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("querying spans: %w", err)
	}

	if spans == nil {
		spans = []telemetry.Span{}
	}

	return telemetry.TracesResponse{Spans: spans}, nil
}

// handleMetrics returns metric points matching the request filters.
// Name is required.
// Grant: telemetry/metrics.
func (s *TelemetryService) handleMetrics(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/metrics", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/metrics")
	}

	var request telemetry.MetricsRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding metrics request: %w", err)
	}

	if request.Name == "" {
		return nil, fmt.Errorf("metric name is required")
	}

	metrics, err := s.store.QueryMetrics(ctx, MetricFilter{
		Name:    request.Name,
		Machine: request.Machine,
		Source:  request.Source,
		Labels:  request.Labels,
		Start:   request.Start,
		End:     request.End,
		Limit:   request.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("querying metrics: %w", err)
	}

	if metrics == nil {
		metrics = []telemetry.MetricPoint{}
	}

	return telemetry.MetricsResponse{Metrics: metrics}, nil
}

// handleLogs returns log records matching the request filters.
// Grant: telemetry/logs.
func (s *TelemetryService) handleLogs(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/logs", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/logs")
	}

	var request telemetry.LogsRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding logs request: %w", err)
	}

	logs, err := s.store.QueryLogs(ctx, LogFilter{
		Machine:     request.Machine,
		Source:      request.Source,
		MinSeverity: request.MinSeverity,
		TraceID:     request.TraceID,
		Search:      request.Search,
		Start:       request.Start,
		End:         request.End,
		Limit:       request.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("querying logs: %w", err)
	}

	if logs == nil {
		logs = []telemetry.LogRecord{}
	}

	return telemetry.LogsResponse{Logs: logs}, nil
}

// handleTop returns an aggregated operational overview for the
// requested time window. Window is required.
// Grant: telemetry/top.
func (s *TelemetryService) handleTop(ctx context.Context, token *servicetoken.Token, raw []byte) (any, error) {
	if !servicetoken.GrantsAllow(token.Grants, "telemetry/top", "") {
		return nil, fmt.Errorf("access denied: missing grant for telemetry/top")
	}

	var request telemetry.TopRequest
	if err := codec.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decoding top request: %w", err)
	}

	if request.Window <= 0 {
		return nil, fmt.Errorf("window is required and must be positive")
	}

	response, err := s.store.QueryTop(ctx, TopFilter{
		Window:  request.Window,
		Machine: request.Machine,
	})
	if err != nil {
		return nil, fmt.Errorf("querying top: %w", err)
	}

	return response, nil
}
