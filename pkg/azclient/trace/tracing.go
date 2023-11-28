/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trace

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.10.0"
	"go.opentelemetry.io/otel/trace"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/trace/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

func init() {
	utils.TracingProvider = tracing.NewProvider(func(name, version string) tracing.Tracer {
		return tracing.NewTracer(NewOtlpSpan, nil)
	}, nil)
}

const (
	instrumentationName    = "otlp"
	instrumentationVersion = "v0.1.0"
	SDKSpanKindKey         = "azsdkTraceKind"
)

var (
	tracer = otel.GetTracerProvider().Tracer(
		instrumentationName,
		trace.WithInstrumentationVersion(instrumentationVersion),
		trace.WithSchemaURL(semconv.SchemaURL),
	)
)

func NewOtlpSpan(ctx context.Context, spanName string, options *tracing.SpanOptions) (context.Context, tracing.Span) {
	var span trace.Span
	var traceAttributes []attribute.KeyValue

	if options != nil {
		for _, entry := range options.Attributes {
			traceAttributes = append(traceAttributes, attribute.String(entry.Key, fmt.Sprintf("%v", entry.Value)))
		}
		traceAttributes = append(traceAttributes, attribute.Int(SDKSpanKindKey, int(options.Kind)))
	}

	ctx, span = tracer.Start(ctx, spanName, trace.WithAttributes(traceAttributes...))

	var (
		requestMethod  = "TODO"
		resourceGroup  = "TODO"
		subscriptionID = "TODO"
		metricsSpan    = metrics.Default().NewSpan(spanName, requestMethod, resourceGroup, subscriptionID)
	)

	return ctx, tracing.NewSpan(
		tracing.SpanImpl{
			End: func() {
				metricsSpan.Observe(context.Background(), nil)
				span.End()
			},
			SetAttributes: func(attributes ...tracing.Attribute) {
				var traceAttributes []attribute.KeyValue
				for _, entry := range attributes {
					traceAttributes = append(traceAttributes, attribute.String(entry.Key, fmt.Sprintf("%v", entry.Value)))
				}
				span.SetAttributes(traceAttributes...)
			},
			AddEvent: func(name string, attributes ...tracing.Attribute) {
				var traceAttributes []attribute.KeyValue
				for _, entry := range attributes {
					traceAttributes = append(traceAttributes, attribute.String(entry.Key, fmt.Sprintf("%v", entry.Value)))
				}
				span.AddEvent(name, trace.WithAttributes(traceAttributes...))
			},
			AddError: func(err error) {
				span.RecordError(err)
			},

			SetStatus: func(status tracing.SpanStatus, description string) {
				switch status {
				case tracing.SpanStatusOK:
					span.SetStatus(codes.Ok, description)
				case tracing.SpanStatusError:
					span.SetStatus(codes.Error, description)
				default:
					span.SetStatus(codes.Unset, description)
				}
			},
		},
	)
}
func NewOtlpSpanFromContext(ctx context.Context) (tracing.Span, bool) {
	var span trace.Span

	_, span = tracer.Start(ctx, "NewSpanFromCtx")

	return tracing.NewSpan(
		tracing.SpanImpl{
			End: func() {
				span.End()
			},
			SetAttributes: func(attributes ...tracing.Attribute) {
				var traceAttributes []attribute.KeyValue
				for _, entry := range attributes {
					traceAttributes = append(traceAttributes, attribute.String(entry.Key, fmt.Sprintf("%v", entry.Value)))
				}
				span.SetAttributes(traceAttributes...)
			},
			AddEvent: func(name string, attributes ...tracing.Attribute) {
				var traceAttributes []attribute.KeyValue
				for _, entry := range attributes {
					traceAttributes = append(traceAttributes, attribute.String(entry.Key, fmt.Sprintf("%v", entry.Value)))
				}
				span.AddEvent(name, trace.WithAttributes(traceAttributes...))
			},
			AddError: func(err error) {
				span.RecordError(err)
			},

			SetStatus: func(status tracing.SpanStatus, description string) {
				switch status {
				case tracing.SpanStatusOK:
					span.SetStatus(codes.Ok, description)
				case tracing.SpanStatusError:
					span.SetStatus(codes.Error, description)
				default:
					span.SetStatus(codes.Unset, description)
				}
			},
		},
	), true
}
