# Contract: JetStream store layout handling (WI-C2)

**Feature**: `005-scale-out-1-to-3` | FR-009..FR-012 | research R-004, R-005

## API (internal/embedded/store_layout.go)

```go
// StoreLayout is what an on-disk JetStream store looks like.
type StoreLayout string

const (
	StoreLayoutNone       StoreLayout = "none"
	StoreLayoutStandalone StoreLayout = "standalone"
	StoreLayoutClustered  StoreLayout = "clustered"
)

// DetectStoreLayout inspects <dir>/jetstream. dir == "" → StoreLayoutNone.
// Any stat error other than not-exist is returned (fail-closed).
func DetectStoreLayout(dir string) (StoreLayout, error)

// ArchiveIncompatibleStore moves <dir>/jetstream aside when its layout does
// not match the mode the server is about to start in. Returns the new path
// and the layout that was archived, or ("", StoreLayoutNone, nil) when
// nothing was moved.
func ArchiveIncompatibleStore(dir string, clustered bool, now time.Time) (to string, archived StoreLayout, err error)
```

## Rules

1. **Detection**:
   - `<dir>/jetstream/$SYS/_js_/_meta_` exists → `clustered`;
   - else `<dir>/jetstream` exists → `standalone`;
   - else → `none`.
2. **Mismatch**: `(clustered && layout == standalone) || (!clustered && layout == clustered)`.
3. **Target name**: `<dir>/jetstream.<layout>-<now.UTC().Format("20060102T150405Z")>`. If it exists, append `-1`..`-100`. Past `-100`, return an error.
4. **Operation**: one `os.Rename` within `<dir>`. Never rename, delete or chmod `<dir>` itself.
5. **Idempotence**: once archived, the layout is `none`, so a second call is a no-op.
6. **Errors**: a detection or rename error is returned. The caller maps it to `StartupError{Code: ExitConfig}` naming the path.

## Call site

`internal/runtime/start.go` `bootEmbeddedNATS`:
- after `embedded.BuildOptions`, before `embedded.NewServer`;
- with `clustered := cc.RoutesListen.Enabled && cc.DeclaredSize > 1`.

On a non-empty `to`, emit `embedded_nats.store_archived` ([observability.md](observability.md)).

## Tests

**Unit** (`store_layout_test.go`, temp dirs):
- detection of the three layouts;
- archive standalone → clustered;
- archive clustered → standalone;
- no-op when matching;
- no-op when `dir == ""` or no `jetstream/`;
- collision suffix;
- `<dir>` inode and mode unchanged;
- second call is a no-op;
- an unreadable `jetstream/` returns an error.

**Real server**:
- `tests/scaleout` S1 asserts that node-a's archived standalone store exists after expansion;
- S3 asserts that the clustered store is archived after rollback.
