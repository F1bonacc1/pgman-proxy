// Copyright 2026 The pgman-proxy Authors
// Licensed under the Apache License, Version 2.0.

package embedded

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StoreLayout is the shape of an on-disk JetStream store under
// cluster.jetstream_dir (specs/005-scale-out-1-to-3/contracts/
// jetstream-store-layout.md).
type StoreLayout string

const (
	// StoreLayoutNone: no <dir>/jetstream, or no persistent dir at all.
	StoreLayoutNone StoreLayout = "none"
	// StoreLayoutStandalone: a store written by a non-clustered server.
	StoreLayoutStandalone StoreLayout = "standalone"
	// StoreLayoutClustered: a store written by a clustered server; only
	// these carry the JetStream meta group under $SYS/_js_/_meta_.
	StoreLayoutClustered StoreLayout = "clustered"
)

// storeSubdir is where nats-server keeps JetStream state inside its
// configured StoreDir.
const storeSubdir = "jetstream"

// maxArchiveSuffix bounds the -N suffix search on a name collision.
const maxArchiveSuffix = 100

// DetectStoreLayout inspects <dir>/jetstream. An empty dir (in-memory
// JetStream) is StoreLayoutNone. Stat errors other than not-exist are
// returned so the caller fails closed.
func DetectStoreLayout(dir string) (StoreLayout, error) {
	if dir == "" {
		return StoreLayoutNone, nil
	}
	store := filepath.Join(dir, storeSubdir)
	if _, err := os.Stat(store); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StoreLayoutNone, nil
		}
		return "", fmt.Errorf("stat %s: %w", store, err)
	}
	meta := filepath.Join(store, "$SYS", "_js_", "_meta_")
	if _, err := os.Stat(meta); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StoreLayoutStandalone, nil
		}
		return "", fmt.Errorf("stat %s: %w", meta, err)
	}
	return StoreLayoutClustered, nil
}

// ArchiveIncompatibleStore moves <dir>/jetstream aside when its layout
// does not match the mode the server is about to start in: a clustered
// start over a standalone store (1→3 expansion) or a standalone start
// over a clustered one (rollback). Reusing the other mode's store gives
// undefined results; in the 2026-10-14 rig a clustered server adopted
// the old history stream but started a fresh KV bucket (spec 005 F13).
//
// The store is renamed to <dir>/jetstream.<layout>-<UTC timestamp>,
// with a -N suffix on collision, and kept for forensics. dir itself is
// never renamed: it is often a mount point. Returns the new path and
// the layout that was archived, or ("", StoreLayoutNone, nil) when
// nothing was moved. Idempotent: after a move the layout is none.
func ArchiveIncompatibleStore(dir string, clustered bool, now time.Time) (string, StoreLayout, error) {
	layout, err := DetectStoreLayout(dir)
	if err != nil {
		return "", StoreLayoutNone, err
	}
	mismatch := (clustered && layout == StoreLayoutStandalone) || (!clustered && layout == StoreLayoutClustered)
	if !mismatch {
		return "", StoreLayoutNone, nil
	}
	from := filepath.Join(dir, storeSubdir)
	base := filepath.Join(dir, fmt.Sprintf("%s.%s-%s", storeSubdir, layout, now.UTC().Format("20060102T150405Z")))
	to := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(to); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return "", StoreLayoutNone, fmt.Errorf("stat %s: %w", to, err)
		}
		if i > maxArchiveSuffix {
			return "", StoreLayoutNone, fmt.Errorf("no free archive name for %s after %d attempts", from, maxArchiveSuffix)
		}
		to = fmt.Sprintf("%s-%d", base, i)
	}
	if err := os.Rename(from, to); err != nil {
		return "", StoreLayoutNone, fmt.Errorf("move %s aside: %w", from, err)
	}
	return to, layout, nil
}
