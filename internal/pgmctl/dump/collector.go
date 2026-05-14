// Parallel slice fetcher for `pgmctl dump` (T063 / FR-032).
//
// One go-routine per slice; per-slice timeout caps each request
// (FR-032 default 10s). The collector NEVER fails the whole dump on a
// single-slice error — failed slices land in the manifest with a
// structured error per fanout-protocol.md § Aggregation rules so the
// operator sees the gap and reason.

package dump

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Fetcher is the narrow contract the collector needs against the
// pgman-proxy HTTP control plane. Carved into an interface so unit
// tests can drive the collector without a running server.
type Fetcher interface {
	GetJSON(ctx context.Context, path string) (json.RawMessage, error)
}

// SliceSpec is one work unit: a slice name + the closure that fetches
// it. The closure receives a context bounded by the per-slice timeout.
type SliceSpec struct {
	Name  string
	Fetch func(ctx context.Context) (any, error)
}

// SliceResult is what the collector returns to the writer per slice.
// Data is JSON-marshalled by the caller; we don't pre-encode here so
// the redactor can operate on the typed shape.
type SliceResult struct {
	Name     string
	Outcome  string
	Data     any
	Err      error
	Duration time.Duration
}

// Collector runs every spec in parallel and returns the results in
// the input order. Per-slice failures are absorbed (Result.Err set;
// Outcome=OutcomeFailed). The whole call only returns an error on
// context cancellation.
type Collector struct {
	perSliceTimeout time.Duration
}

// NewCollector constructs a Collector with the given per-slice timeout.
// Defaults to 10s when timeout <= 0 (FR-032).
func NewCollector(perSliceTimeout time.Duration) *Collector {
	if perSliceTimeout <= 0 {
		perSliceTimeout = 10 * time.Second
	}
	return &Collector{perSliceTimeout: perSliceTimeout}
}

// Run dispatches every spec in parallel under the outer ctx. Each
// spec's closure runs under a derived context bounded by the
// per-slice timeout. Returns slice results in the same order as the
// input specs.
func (c *Collector) Run(ctx context.Context, specs []SliceSpec) []SliceResult {
	results := make([]SliceResult, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(idx int, s SliceSpec) {
			defer wg.Done()
			results[idx] = c.runOne(ctx, s)
		}(i, spec)
	}
	wg.Wait()
	return results
}

func (c *Collector) runOne(ctx context.Context, s SliceSpec) SliceResult {
	start := time.Now()
	sliceCtx, cancel := context.WithTimeout(ctx, c.perSliceTimeout)
	defer cancel()
	data, err := s.Fetch(sliceCtx)
	dur := time.Since(start)
	if err != nil {
		return SliceResult{Name: s.Name, Outcome: OutcomeFailed, Err: err, Duration: dur}
	}
	return SliceResult{Name: s.Name, Outcome: OutcomeOK, Data: data, Duration: dur}
}

// HTTPSliceFetcher is the canonical SliceSpec.Fetch builder for slices
// that map 1:1 to a pgman-proxy HTTP GET. Embeds the raw envelope's
// engine_result so downstream JSON marshalling produces the same shape
// the operator sees with `pgmctl <slice> -o json`.
func HTTPSliceFetcher(f Fetcher, path string) func(ctx context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		raw, err := f.GetJSON(ctx, path)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, errors.New("empty response")
		}
		// Re-unmarshal into a generic value so the result is
		// re-marshalable into JSON / YAML without leaking
		// json.RawMessage semantics through to redaction.
		var v any
		if jerr := json.Unmarshal(raw, &v); jerr != nil {
			return nil, fmt.Errorf("decode response: %w", jerr)
		}
		return v, nil
	}
}

// DefaultSpecs returns the v1 slice set the dump CLI captures by
// default (T063). The since parameter feeds the history-events /
// history-audit queries; pass zero to use server-side defaults.
//
// Slices not yet wired by the server (doctor, clock-skew) are
// included as "advertise & fail" so the manifest documents the gap.
func DefaultSpecs(f Fetcher, since time.Duration) []SliceSpec {
	sinceQuery := ""
	if since > 0 {
		sinceQuery = "&since=" + since.String()
	}
	return []SliceSpec{
		{Name: "status", Fetch: HTTPSliceFetcher(f, "/v1/status")},
		{Name: "topology", Fetch: HTTPSliceFetcher(f, "/v1/status")}, // derived client-side
		{Name: "history-events", Fetch: HTTPSliceFetcher(f, "/v1/history?category=event"+sinceQuery)},
		{Name: "history-audit", Fetch: HTTPSliceFetcher(f, "/v1/history?category=audit"+sinceQuery)},
		// Not yet implemented server-side; the failure outcome
		// surfaces in the manifest so the operator sees the gap.
		{Name: "doctor", Fetch: notImplementedFetch("/v1/doctor/run (US3)")},
		{Name: "clock-skew", Fetch: notImplementedFetch("/v1/clock-skew (US3)")},
		{Name: "config", Fetch: notImplementedFetch("/v1/config (US6)")},
	}
}

func notImplementedFetch(label string) func(ctx context.Context) (any, error) {
	msg := label + " not yet implemented server-side"
	return func(_ context.Context) (any, error) {
		return nil, errors.New(msg)
	}
}
