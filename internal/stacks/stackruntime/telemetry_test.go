// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package stackruntime

//lint:file-ignore U1000 Some utilities in here are intentionally unused in VCS but are for temporary use while debugging a test.

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// tracesToTestLog arranges for any traces generated by the current test to
// be emitted directly into the test log using the log methods of the given
// [testing.T].
//
// This works by temporarily reassigning the global tracer provider and so
// is not suitable for parallel tests or subtests of tests that have already
// called this function.
//
// The results of this function are pretty chatty, so we should typically not
// leave this in a test checked in to version control, but it can be helpful to
// add temporarily during test debugging if it's unclear exactly how different
// components are interacting with one another.
func tracesToTestLog(t *testing.T) {
	t.Helper()
	oldProvider := otel.GetTracerProvider()
	if _, ok := oldProvider.(*testLogTracerProvider); ok {
		// This suggests that someone's tried to use tracesToTestLog in
		// a parallel test or in a subtest of a test that already called it.
		t.Fatal("overlapping tracesToTestLog")
	}
	t.Cleanup(func() {
		otel.SetTracerProvider(oldProvider)
	})

	provider := testLogTracerProvider{
		t: t,
		spanTracker: &spanTracker{
			names:  make(map[trace.SpanID]string),
			nextID: 1,
		},
	}
	otel.SetTracerProvider(provider)
}

type testLogTracerProvider struct {
	t           *testing.T
	spanTracker *spanTracker

	embedded.TracerProvider
}

type spanTracker struct {
	names  map[trace.SpanID]string
	nextID int
	mu     sync.Mutex
}

func (t *spanTracker) StartNew(name string) trace.SpanID {
	t.mu.Lock()
	idRaw := t.nextID
	t.nextID++
	t.mu.Unlock()

	ret := trace.SpanID{
		0x00, 0x00, 0x00, 0x00,
		byte(idRaw >> 24),
		byte(idRaw >> 16),
		byte(idRaw >> 8),
		byte(idRaw >> 0),
	}
	t.TrackNew(ret, name)
	return ret
}

func (sn *spanTracker) TrackNew(id trace.SpanID, name string) {
	sn.mu.Lock()
	sn.names[id] = name
	sn.mu.Unlock()
}

func (sn *spanTracker) Get(id trace.SpanID) string {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	return sn.names[id]
}

func (sn *spanTracker) SpanDisplay(id trace.SpanID) string {
	// we only use the last 32bits of the ids in this fake tracer, because
	// the others will always be zero. (see testLogTracer.generateSpanID)
	name := sn.Get(id)
	idStr := testingSpanIDString(id)
	if name == "" {
		return idStr
	}
	return fmt.Sprintf("%s(%q)", idStr, name)
}

func (sn *spanTracker) SpanAttrDisplay(kv attribute.KeyValue) string {
	v := kv.Value.AsInterface()
	switch string(kv.Key) {
	case "promise.waiting_for_id", "promise.waiter_id",
		"promising.resolved_by", "promising.resolved_id",
		"promising.delegated_from", "promising.delegated_to",
		"promising.responsible_for":

		// These conventionally contain stringified span IDs, which
		// are 16 hex digits.
		if v, ok := v.(string); ok && len(v) == 16 {
			if bytes, err := hex.DecodeString(v); err == nil {
				var spanID trace.SpanID
				copy(spanID[:], bytes)
				return sn.SpanDisplay(spanID)
			}
		}
	}
	// If all else fails we'll just GoString it
	return fmt.Sprintf("%#v", v)
}

var _ trace.TracerProvider = (*testLogTracerProvider)(nil)

// Tracer implements trace.TracerProvider.
func (p testLogTracerProvider) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	p.t.Helper()
	return &testLogTracer{
		t:           p.t,
		nextSpanID:  1,
		spanTracker: p.spanTracker,
	}
}

type testLogTracer struct {
	t           *testing.T
	spanTracker *spanTracker
	nextSpanID  uint32
	mu          sync.Mutex

	embedded.Tracer
}

var _ trace.Tracer = (*testLogTracer)(nil)

var fakeTraceIDForTesting = trace.TraceID{
	0xfe, 0xed, 0xfa, 0xce,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
}

// Start implements trace.Tracer.
func (t *testLogTracer) Start(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	t.t.Helper()

	parentSpanCtx := trace.SpanContextFromContext(ctx)

	dispName := spanName
	switch dispName { // some shorthands for common names
	case "async task":
		dispName = "🗘"
	case "promise":
		dispName = "⋯"
	}
	if parentName := t.spanTracker.Get(parentSpanCtx.SpanID()); parentName != "" {
		dispName = parentName + " ⇨ " + dispName
	}
	spanID := t.spanTracker.StartNew(dispName)

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: fakeTraceIDForTesting,
		SpanID:  spanID,
	})
	span := &testLogTraceSpan{
		name:        spanName,
		context:     &spanCtx,
		t:           t.t,
		spanTracker: t.spanTracker,
	}
	ctx = trace.ContextWithSpan(ctx, span)

	cfg := trace.NewSpanStartConfig(opts...)
	var attrsBuilder strings.Builder
	if parentSpanCtx.HasSpanID() && !cfg.NewRoot() {
		fmt.Fprintf(&attrsBuilder, "\nPARENT: %s", t.spanTracker.SpanDisplay(parentSpanCtx.SpanID()))
	}
	for _, link := range cfg.Links() {
		fmt.Fprintf(&attrsBuilder, "\nLINK: %s", t.spanTracker.SpanDisplay(link.SpanContext.SpanID()))
	}
	for _, kv := range cfg.Attributes() {
		fmt.Fprintf(&attrsBuilder, "\n%s = %s", kv.Key, t.spanTracker.SpanAttrDisplay(kv))
	}
	span.log("START%s", attrsBuilder.String())
	return ctx, span
}

type testLogTraceSpan struct {
	name        string
	context     *trace.SpanContext
	t           *testing.T
	spanTracker *spanTracker

	embedded.Span
}

var _ trace.Span = (*testLogTraceSpan)(nil)

func (s testLogTraceSpan) log(f string, args ...any) {
	s.t.Helper()
	s.t.Logf(
		"[trace:%s] %s\n%s",
		testingSpanIDString(s.context.SpanID()),
		s.spanTracker.Get(s.context.SpanID()),
		fmt.Sprintf(f, args...),
	)
}

func testingSpanIDString(id trace.SpanID) string {
	// we only use the last 32bits of the ids in this fake tracer, because
	// the others will always be zero. (see testLogTracer.generateSpanID)
	return fmt.Sprintf("%x", id[4:])
}

// AddEvent implements trace.Span.
func (s testLogTraceSpan) AddEvent(name string, options ...trace.EventOption) {
	s.t.Helper()
	cfg := trace.NewEventConfig(options...)
	var attrsBuilder strings.Builder
	for _, kv := range cfg.Attributes() {
		fmt.Fprintf(&attrsBuilder, "\n%s = %s", kv.Key, s.spanTracker.SpanAttrDisplay(kv))
	}
	s.log("EVENT %s%s", name, attrsBuilder.String())
}

// End implements trace.Span.
func (s testLogTraceSpan) End(options ...trace.SpanEndOption) {
	s.t.Helper()
	s.log("END")
}

// IsRecording implements trace.Span.
func (s testLogTraceSpan) IsRecording() bool {
	s.t.Helper()
	return true
}

// RecordError implements trace.Span.
func (s testLogTraceSpan) RecordError(err error, options ...trace.EventOption) {
	s.t.Helper()
	s.log("ERROR %s", err)
}

// SetAttributes implements trace.Span.
func (s testLogTraceSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.t.Helper()
}

// SetName implements trace.Span.
func (s *testLogTraceSpan) SetName(name string) {
	s.t.Helper()
	s.log("RENAMED to %s", name)
	s.name = name
}

// SetStatus implements trace.Span.
func (s testLogTraceSpan) SetStatus(code codes.Code, description string) {
	s.t.Helper()
	s.log("STATUS %s: %s", code, description)
}

// SpanContext implements trace.Span.
func (s testLogTraceSpan) SpanContext() trace.SpanContext {
	s.t.Helper()
	return *s.context
}

// TracerProvider implements trace.Span.
func (s testLogTraceSpan) TracerProvider() trace.TracerProvider {
	s.t.Helper()
	return testLogTracerProvider{
		t:           s.t,
		spanTracker: s.spanTracker,
	}
}

// AddLink implements trace.Span.
func (s testLogTraceSpan) AddLink(link trace.Link) {
	// Noop
}
