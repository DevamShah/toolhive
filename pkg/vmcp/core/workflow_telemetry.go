// SPDX-FileCopyrightText: Copyright 2025 Stacklok, Inc.
// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/stacklok/toolhive/pkg/telemetry"
	"github.com/stacklok/toolhive/pkg/vmcp"
)

// workflowInstrumentationName is the OTEL instrumentation scope for the core's workflow
// metrics. It matches the session layer's scope so the metrics are indistinguishable from
// the pre-refactor ones.
const workflowInstrumentationName = "github.com/stacklok/toolhive/pkg/vmcp"

// workflowTelemetry records composite-tool (workflow) execution metrics and traces on the
// core call path.
//
// Composite tools now execute in the core (CallTool → executeComposite), not in the
// session-layer composite-tools decorator. This reproduces the workflow instrumentation
// that decorator carried (sessionmanager.telemetryWorkflowExecutor) — same metric names,
// unit, and the workflow.name attribute — so existing dashboards are unaffected by the
// relocation. A nil *workflowTelemetry is the disabled case (no telemetry provider):
// record invokes the execution directly.
type workflowTelemetry struct {
	tracer            trace.Tracer
	executionsTotal   metric.Int64Counter
	errorsTotal       metric.Int64Counter
	executionDuration metric.Float64Histogram
}

// newWorkflowTelemetry builds the workflow instruments from provider, or returns (nil, nil)
// when provider is nil (telemetry disabled). The instrument names/unit mirror
// sessionmanager.newWorkflowExecutorInstruments so the Prometheus series are unchanged.
func newWorkflowTelemetry(provider *telemetry.Provider) (*workflowTelemetry, error) {
	if provider == nil {
		return nil, nil
	}

	meter := provider.MeterProvider().Meter(workflowInstrumentationName)

	executionsTotal, err := meter.Int64Counter(
		"toolhive_vmcp_workflow_executions",
		metric.WithDescription("Total number of workflow executions"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow executions counter: %w", err)
	}

	errorsTotal, err := meter.Int64Counter(
		"toolhive_vmcp_workflow_errors",
		metric.WithDescription("Total number of workflow execution errors"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow errors counter: %w", err)
	}

	executionDuration, err := meter.Float64Histogram(
		"toolhive_vmcp_workflow_duration",
		metric.WithDescription("Duration of workflow executions in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(telemetry.MCPHistogramBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflow duration histogram: %w", err)
	}

	return &workflowTelemetry{
		tracer:            provider.TracerProvider().Tracer(workflowInstrumentationName),
		executionsTotal:   executionsTotal,
		errorsTotal:       errorsTotal,
		executionDuration: executionDuration,
	}, nil
}

// record wraps a composite-tool execution with a span and the workflow metrics, labeled by
// workflowName. A nil receiver (telemetry disabled) invokes fn directly. The execution is
// counted as an error when fn returns a Go error OR a tool-level error result
// (executeComposite converts workflow failures to an IsError result rather than an error).
func (wt *workflowTelemetry) record(
	ctx context.Context,
	workflowName string,
	fn func(context.Context) (*vmcp.ToolCallResult, error),
) (*vmcp.ToolCallResult, error) {
	if wt == nil {
		return fn(ctx)
	}

	attrs := []attribute.KeyValue{attribute.String("workflow.name", workflowName)}
	ctx, span := wt.tracer.Start(ctx, "core.executeComposite", trace.WithAttributes(attrs...))
	defer span.End()

	metricAttrs := metric.WithAttributes(attrs...)
	start := time.Now()
	wt.executionsTotal.Add(ctx, 1, metricAttrs)

	result, err := fn(ctx)

	wt.executionDuration.Record(ctx, time.Since(start).Seconds(), metricAttrs)
	if err != nil || (result != nil && result.IsError) {
		wt.errorsTotal.Add(ctx, 1, metricAttrs)
		span.SetStatus(codes.Error, "workflow execution failed")
		if err != nil {
			span.RecordError(err)
		}
	}
	return result, err
}
