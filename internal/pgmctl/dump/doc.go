// Package dump assembles the single-artifact full-state dump
// (FR-032..FR-035) consumed by post-mortem authors.
//
// Slices are fetched in parallel with a per-slice timeout; unreachable
// slices are recorded as missing entries with their underlying error;
// the dump never blocks indefinitely on a stalled slice.
package dump
