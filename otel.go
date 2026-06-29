// Minimal OTLP/HTTP trace exporter.
//
// Sends one span per barmkin invocation to any OTLP-capable collector
// (e.g. Jaeger, Tempo, Honeycomb). No SDK dependencies - just a raw
// HTTP POST with the OTLP JSON payload.
//
// Configuration (in rules.yaml):
//
//	otel:
//	  endpoint: "localhost:4317"   # OTLP HTTP endpoint
//	  service: "barmkin"           # service.name attribute (optional)
//
// When endpoint is empty, the exporter is nil and all Record calls
// are no-ops. Span name format: "<tool>.<action>" (e.g. "bash.ShellCommand").

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// OTLPExporter sends individual OTLP spans over HTTP.
// Nil when OTel is disabled (no endpoint configured).
type OTLPExporter struct {
	endpoint string
	service  string
	client   *http.Client
}

// newOTLPExporter creates an exporter from config. Returns nil if
// no endpoint is configured.
func newOTLPExporter(cfg OTel) *OTLPExporter {
	if cfg.Endpoint == "" {
		return nil
	}
	svc := cfg.Service
	if svc == "" {
		svc = "barmkin"
	}
	return &OTLPExporter{
		endpoint: cfg.Endpoint,
		service:  svc,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

// Record sends a single span synchronously. The span captures the
// tool call as a trace event with attributes for decision, rule,
// agent, and trajectory_id.
//
// Called once per evaluation. If the HTTP POST fails, the error is
// logged to stderr (only in verbose mode) and swallowed - telemetry
// failures must never block the guardrail.
func (e *OTLPExporter) Record(entry AuditEntry, start time.Time) {
	if e == nil {
		return
	}

	// Build OTLP attributes from the audit entry
	attrs := []map[string]any{
		{"key": "decision", "value": map[string]any{"stringValue": entry.Decision}},
		{"key": "service", "value": map[string]any{"stringValue": e.service}},
	}
	if entry.Tool != "" {
		attrs = append(attrs, map[string]any{"key": "tool", "value": map[string]any{"stringValue": entry.Tool}})
	}
	if entry.Rule != "" {
		attrs = append(attrs, map[string]any{"key": "rule", "value": map[string]any{"stringValue": entry.Rule}})
	}
	if entry.Agent != "" {
		attrs = append(attrs, map[string]any{"key": "agent", "value": map[string]any{"stringValue": entry.Agent}})
	}
	if entry.TrajectoryID != "" {
		attrs = append(attrs, map[string]any{"key": "trajectory_id", "value": map[string]any{"stringValue": entry.TrajectoryID}})
	}

	// Single span per invocation
	span := map[string]any{
		"traceId":           fmt.Sprintf("%032x", time.Now().UnixNano()),
		"name":              fmt.Sprintf("%s.%s", entry.Tool, entry.Action),
		"kind":              0, // UNSPECIFIED
		"startTimeUnixNano": fmt.Sprintf("%d", start.UnixNano()),
		"endTimeUnixNano":   fmt.Sprintf("%d", time.Now().UnixNano()),
		"attributes":        attrs,
		"status":            map[string]any{"code": 1}, // OK
	}

	// Wrap in OTLP resourceSpans envelope
	payload := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": e.service}},
					},
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{"name": "barmkin"},
						"spans": []map[string]any{span},
					},
				},
			},
		},
	}

	body, _ := json.Marshal(payload)
	resp, err := e.client.Post(e.endpoint+"/v1/traces", "application/json", bytes.NewReader(body))
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "[barmkin] otel: %v\n", err)
		}
		return
	}
	resp.Body.Close()
}

// Flush is a no-op - spans are sent synchronously in Record.
// Exists to satisfy the Engine.close() cleanup interface.
func (e *OTLPExporter) Flush() {}
