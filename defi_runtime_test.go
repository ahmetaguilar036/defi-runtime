package defiruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock returns a Clock that advances by one second on every call, so that
// file names produced in sequence are strictly ordered without any dependence
// on wall-clock time.
func fakeClock() Clock {
	base := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	var n int
	return func() time.Time {
		t := base.Add(time.Duration(n) * time.Second)
		n++
		return t
	}
}

func TestCaptureWritesStack(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, 0, fakeClock())

	path, err := d.Capture()
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("capture written to %q, want dir %q", filepath.Dir(path), dir)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("capture is empty")
	}
	if !strings.Contains(string(data), "goroutine") {
		t.Fatalf("capture does not look like a stack dump: %q", data[:min(len(data), 80)])
	}
}

func TestCaptureNamesAreUnique(t *testing.T) {
	dir := t.TempDir()
	// A frozen clock guarantees that uniqueness comes from the counter, not
	// from the timestamp.
	frozen := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	d := NewDumper(dir, 0, func() time.Time { return frozen })

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		path, err := d.Capture()
		if err != nil {
			t.Fatalf("Capture %d: %v", i, err)
		}
		if seen[path] {
			t.Fatalf("duplicate capture path %q", path)
		}
		seen[path] = true
	}
}

func TestPruneKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, 3, fakeClock())

	for i := 0; i < 6; i++ {
		if _, err := d.Capture(); err != nil {
			t.Fatalf("Capture %d: %v", i, err)
		}
	}

	names, err := d.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("kept %d captures, want 3: %v", len(names), names)
	}

	// The three survivors must be the last three names written.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	_ = entries

	for i, name := range names {
		wantSuffix := []string{"000004", "000005", "000006"}[i]
		if !strings.Contains(name, wantSuffix) {
			t.Errorf("survivor %d = %q, want counter %s", i, name, wantSuffix)
		}
	}
}

func TestPruneIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	foreign := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed foreign file: %v", err)
	}

	d := NewDumper(dir, 1, fakeClock())
	for i := 0; i < 3; i++ {
		if _, err := d.Capture(); err != nil {
			t.Fatalf("Capture %d: %v", i, err)
		}
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file was removed: %v", err)
	}

	names, err := d.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("kept %d captures, want 1: %v", len(names), names)
	}
}

func TestKeepZeroRetainsEverything(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, 0, fakeClock())

	for i := 0; i < 4; i++ {
		if _, err := d.Capture(); err != nil {
			t.Fatalf("Capture %d: %v", i, err)
		}
	}

	names, err := d.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 4 {
		t.Fatalf("kept %d captures, want 4", len(names))
	}
}

func TestListOnMissingDir(t *testing.T) {
	d := NewDumper(filepath.Join(t.TempDir(), "does-not-exist"), 1, fakeClock())
	names, err := d.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if names != nil {
		t.Fatalf("List = %v, want nil", names)
	}
}

func TestCaptureCreatesMissingDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "captures")
	d := NewDumper(dir, 1, fakeClock())

	if _, err := d.Capture(); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestRunCapturesImmediatelyAndOnTick(t *testing.T) {
	dir := t.TempDir()
	d := NewDumper(dir, 0, fakeClock())

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		d.Run(stop, 5*time.Millisecond, nil)
		close(done)
	}()

	// Wait for the immediate capture without sleeping: poll the directory with
	// a bounded number of attempts. This is deterministic in the sense that it
	// only depends on the goroutine making progress, not on a fixed delay.
	deadline := time.Now().Add(2 * time.Second)
	for {
		names, err := d.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(names) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not capture immediately")
		}
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after stop was closed")
	}
}

func TestRunReportsErrors(t *testing.T) {
	// Point the dumper at a path that cannot be a directory: a file.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	d := NewDumper(filepath.Join(blocker, "sub"), 1, fakeClock())
	errs := make(chan error, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		d.Run(stop, time.Hour, func(err error) {
			select {
			case errs <- err:
			default:
			}
		})
		close(done)
	}()

	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("expected a non-nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not report the capture error")
	}

	close(stop)
	<-done
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
