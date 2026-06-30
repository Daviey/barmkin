// Minimal OTLP/HTTP trace exporter.
//
// Sends one span per barmkin invocation to any OTLP-capable collector
// (e.g. Jaeger, Tempo, Honeycomb). No SDK dependencies - just a raw
// HTTP POST with the OTLP JSON payload.
//
// Configuration (in rules.yaml):
//
//	otel:
//	  endpoint: "localhost:4318"   # OTLP HTTP/JSON endpoint (4318, not gRPC 4317)
//	  service: "barmkin"           # service.name attribute (optional)
//
// When endpoint is empty, the exporter is nil and all Record calls
// are no-ops. Span name format: "<tool>.<action>" (e.g. "bash.ShellCommand").

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// OTLPExporter sends individual OTLP spans over HTTP.
// Nil when OTel is disabled (no endpoint configured).
type OTLPExporter struct {
	endpoint string
	service  string
	client   *http.Client
	wg       sync.WaitGroup
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
		client:   &http.Client{},
	}
}

// randHex returns n random bytes encoded as a hex string (2n chars).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// Fallback: time-based, still better than the previous single-int approach.
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Record fires a span asynchronously. The HTTP POST runs in a goroutine
// with a 3-second context deadline so a slow or unreachable collector
// never delays the evaluation result. Call Flush() to wait for the
// goroutine before process exit.
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

	// Single span per invocation. traceId: 16 random bytes (128-bit).
	// spanId: 8 random bytes (64-bit). Both required by the OTLP spec.
	span := map[string]any{
		"traceId":           randHex(16),
		"spanId":            randHex(8),
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

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "POST", e.endpoint+"/v1/traces", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.client.Do(req)
		if err != nil {
			if verbose {
				fmt.Fprintf(os.Stderr, "[barmkin] otel: %v\n", err)
			}
			return
		}
		resp.Body.Close()
	}()
}

// Flush waits for any in-flight span export to complete.
// Called by engine.close() before process exit.
func (e *OTLPExporter) Flush() {
	if e == nil {
		return
	}
	e.wg.Wait()
}
