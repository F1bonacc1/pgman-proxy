// Tar archive writer for `pgmctl dump` (T065 / FR-034).
//
// Single-pass streaming: every slice + the manifest is written
// directly to the underlying io.Writer in one go. An in-flight failure
// leaves the partial archive in a state operators can still extract
// (the tar format is forgiving — gtar / bsdtar will extract whatever
// completed entries the stream contains before truncation).

package dump

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Writer streams a dump artifact to the underlying io.Writer.
//   - When gz is true (caller-determined from output path having a
//     .tar.gz / .tgz suffix, or — equivalently — anytime the operator
//     specified --output <file>), the bytes are gzipped.
//   - When the destination is "-" (stdout, FR-034), gz must be false.
//
// Construct via NewWriter; call WriteSlice for each captured slice in
// any order; call Close to flush the manifest + close the gzip layer.
type Writer struct {
	tw       *tar.Writer
	gzw      *gzip.Writer
	manifest *Manifest
	closed   bool
}

// NewWriter returns a Writer that emits to w. If gz is true, a gzip
// stream wraps the tar stream. The manifest pointer is retained so
// finalization can append it as the last entry.
func NewWriter(w io.Writer, gz bool, m *Manifest) (*Writer, error) {
	if w == nil {
		return nil, errors.New("dump.NewWriter: nil io.Writer")
	}
	if m == nil {
		return nil, errors.New("dump.NewWriter: nil manifest")
	}
	dest := w
	var gzw *gzip.Writer
	if gz {
		gzw = gzip.NewWriter(w)
		dest = gzw
	}
	return &Writer{
		tw:       tar.NewWriter(dest),
		gzw:      gzw,
		manifest: m,
	}, nil
}

// WriteSlice serialises a SliceResult into the archive. Successful
// slices are written as `slices/<name>.json` and recorded in the
// manifest as `outcome=ok`. Failed slices write a `_error` placeholder
// per fanout-protocol.md § Aggregation rules so consumers see the gap
// without parsing two different shapes.
//
// Side effect: appends a SliceEntry to the manifest.
func (w *Writer) WriteSlice(r SliceResult) error {
	if w.closed {
		return errors.New("dump.Writer: closed")
	}
	entry := SliceEntry{
		Name:       r.Name,
		Outcome:    r.Outcome,
		DurationMS: r.Duration.Milliseconds(),
		Path:       "slices/" + r.Name + ".json",
	}
	var payload []byte
	switch r.Outcome {
	case OutcomeOK:
		buf, err := json.MarshalIndent(r.Data, "", "  ")
		if err != nil {
			return fmt.Errorf("encode slice %s: %w", r.Name, err)
		}
		payload = buf
	default:
		entry.Error = &ErrorInfo{
			Code:    "slice_failed",
			Message: errMessage(r.Err),
		}
		placeholder := map[string]any{
			"_error": entry.Error,
		}
		buf, err := json.MarshalIndent(placeholder, "", "  ")
		if err != nil {
			return fmt.Errorf("encode placeholder for %s: %w", r.Name, err)
		}
		payload = buf
	}
	if err := w.writeFile(entry.Path, payload); err != nil {
		return err
	}
	w.manifest.AddSlice(entry)
	return nil
}

// WriteRaw appends a raw file (not a slice) at the supplied path.
// Used by the dump command to land the strict-mode correlation table
// (correlation.json) alongside the slices.
func (w *Writer) WriteRaw(path string, content []byte) error {
	if w.closed {
		return errors.New("dump.Writer: closed")
	}
	return w.writeFile(path, content)
}

// Close finalises the archive: appends the manifest, flushes the tar
// trailer, and (when wrapped) the gzip footer. Idempotent.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.manifest.FinishedAt(time.Now())
	mBuf, err := json.MarshalIndent(w.manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := w.writeFile("manifest.json", mBuf); err != nil {
		return err
	}
	if err := w.tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	if w.gzw != nil {
		if err := w.gzw.Close(); err != nil {
			return fmt.Errorf("close gzip writer: %w", err)
		}
	}
	return nil
}

func (w *Writer) writeFile(name string, data []byte) error {
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(data)),
		ModTime:  time.Now().UTC(),
		Typeflag: tar.TypeReg,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := w.tw.Write(data); err != nil {
		return fmt.Errorf("tar body %s: %w", name, err)
	}
	return nil
}

func errMessage(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
