// Package defiruntime captures and archives goroutine stack traces so that a
// process can be inspected after the fact.
//
// The design goal is a small, dependency-free dumper that can be armed at
// process start and then left alone. It writes one file per capture into a
// directory, and prunes the oldest captures once a configured limit is
// exceeded. There is no background goroutine unless Run is called; the caller
// decides whether captures happen on a signal, on an interval, or manually.
package defiruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Clock is the source of the current time. Tests supply a fake so that file
// names and retention decisions are deterministic; production code passes
// time.Now.
type Clock func() time.Time

// Dumper captures goroutine stack traces into a directory.
//
// A Dumper is safe for concurrent use. Captures are serialized by an internal
// mutex so that two goroutines writing at the same moment cannot interleave
// the prune step with a write.
type Dumper struct {
	dir     string
	keep    int
	clock   Clock
	mu      sync.Mutex
	counter uint64
}

// NewDumper returns a Dumper that writes captures into dir and retains at most
// keep files. A non-positive keep means "retain everything".
//
// The directory is created lazily on the first capture rather than here, so
// that constructing a Dumper cannot fail for a path the caller may not have
// finished preparing. clock may be nil, in which case time.Now is used.
func NewDumper(dir string, keep int, clock Clock) *Dumper {
	if clock == nil {
		clock = time.Now
	}
	return &Dumper{dir: dir, keep: keep, clock: clock}
}

// Capture writes the current goroutine stacks to a new file and returns its
// path. The file name encodes the capture time and a monotonic counter so that
// multiple captures within the same clock tick do not collide.
//
// The counter is incremented under the same mutex that guards pruning, which
// guarantees that two concurrent captures never choose the same name even when
// the clock is frozen by a test.
func (d *Dumper) Capture() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return "", fmt.Errorf("defiruntime: create dir: %w", err)
	}

	d.counter++
	name := fmt.Sprintf("goroutines-%s-%06d.txt", d.clock().UTC().Format("20060102T150405.000000000"), d.counter)
	path := filepath.Join(d.dir, name)

	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		// The buffer filled exactly; grow once and retry so we do not silently
		// truncate a large process's stack dump.
		buf = make([]byte, 4<<20)
		n = runtime.Stack(buf, true)
	}

	if err := os.WriteFile(path, buf[:n], 0o644); err != nil {
		return "", fmt.Errorf("defiruntime: write capture: %w", err)
	}

	if err := d.pruneLocked(); err != nil {
		return path, err
	}
	return path, nil
}

// pruneLocked removes the oldest captures until at most keep remain. Callers
// must hold d.mu.
//
// Only files matching the "goroutines-" prefix are considered, so unrelated
// files placed in the directory by the operator are never deleted.
func (d *Dumper) pruneLocked() error {
	if d.keep <= 0 {
		return nil
	}

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return fmt.Errorf("defiruntime: read dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "goroutines-") {
			names = append(names, e.Name())
		}
	}

	if len(names) <= d.keep {
		return nil
	}

	// The names embed a zero-padded timestamp and counter, so lexical order is
	// chronological order. Sorting avoids a stat per file and keeps pruning
	// cheap on directories with many captures.
	sort.Strings(names)

	for _, name := range names[:len(names)-d.keep] {
		if err := os.Remove(filepath.Join(d.dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("defiruntime: prune %s: %w", name, err)
		}
	}
	return nil
}

// List returns the capture file names currently present, oldest first. It is
// useful for tests and for operators inspecting the archive.
func (d *Dumper) List() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := os.ReadDir(d.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("defiruntime: read dir: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "goroutines-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Run captures immediately and then on every tick until stop is closed. It
// blocks until stop is closed, so callers typically invoke it in its own
// goroutine. Errors from individual captures are reported through onErr; a nil
// onErr discards them.
//
// Run does not own stop and never closes it. That keeps ownership with the
// caller, which is the only party that knows when the process is shutting
// down.
func (d *Dumper) Run(stop <-chan struct{}, interval time.Duration, onErr func(error)) {
	if _, err := d.Capture(); err != nil && onErr != nil {
		onErr(err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if _, err := d.Capture(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
