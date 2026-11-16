package statefile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeTimer struct {
	mu      sync.Mutex
	delays  []time.Duration
	pending []func()
}

func (f *fakeTimer) after(d time.Duration, fn func()) stopper {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delays = append(f.delays, d)
	f.pending = append(f.pending, fn)
	return fakeStop{}
}

func (f *fakeTimer) fire() {
	f.mu.Lock()
	pending := f.pending
	f.pending = nil
	f.mu.Unlock()
	for _, fn := range pending {
		fn()
	}
}

type fakeStop struct{}

func (fakeStop) Stop() bool { return true }

func newTestWriter(t *testing.T, snapshot func() ([]byte, error)) (*Writer, *fakeTimer, *[]string) {
	t.Helper()
	timer := &fakeTimer{}
	writes := &[]string{}
	w := NewWriter("test", filepath.Join(t.TempDir(), "x.state"), &sync.Mutex{}, snapshot)
	w.after = timer.after
	w.write = func(path string, data []byte) error {
		*writes = append(*writes, string(data))
		return nil
	}
	return w, timer, writes
}

func TestResolvePath(t *testing.T) {
	if got := ResolvePath("/auth", ""); got != "" {
		t.Fatalf("empty path = %q", got)
	}
	if got := ResolvePath("/auth", ".cliproxy-state/a.state"); got != "/auth/.cliproxy-state/a.state" {
		t.Fatalf("relative path = %q", got)
	}
	if got := ResolvePath("/auth", "/abs/a.state"); got != "/abs/a.state" {
		t.Fatalf("absolute path = %q", got)
	}
}

func TestLoadStatuses(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

	if _, status := Load(filepath.Join(dir, "missing.state"), 1, now); status != LoadMissing || !status.Active() {
		t.Fatalf("missing status = %v", status)
	}

	ok := filepath.Join(dir, "ok.state")
	data, _ := Encode(1, []map[string]int{{"a": 1}, {"b": 2}})
	if err := os.WriteFile(ok, data, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, status := Load(ok, 1, now)
	if status != LoadOK || len(entries) != 2 {
		t.Fatalf("ok load = %v entries=%d", status, len(entries))
	}

	wrongVersion := filepath.Join(dir, "v2.state")
	data2, _ := Encode(2, []int{})
	if err := os.WriteFile(wrongVersion, data2, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, status := Load(wrongVersion, 1, now); status != LoadDisabled || status.Active() {
		t.Fatalf("unknown version status = %v", status)
	}
	if got, _ := os.ReadFile(wrongVersion); string(got) != string(data2) {
		t.Fatal("unknown version file was modified")
	}

	// A newer version may use a different entries shape; it must still be left alone.
	newerShape := filepath.Join(dir, "v2-object.state")
	if err := os.WriteFile(newerShape, []byte(`{"version":2,"entries":{"groups":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, status := Load(newerShape, 1, now); status != LoadDisabled {
		t.Fatalf("newer shape status = %v, want disabled", status)
	}
	if got, _ := os.ReadFile(newerShape); string(got) != `{"version":2,"entries":{"groups":[]}}` {
		t.Fatal("newer shape file was modified")
	}
	if matches, _ := filepath.Glob(newerShape + ".corrupt-*"); len(matches) != 0 {
		t.Fatalf("newer shape quarantined: %v", matches)
	}

	corrupt := filepath.Join(dir, "bad.state")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, status := Load(corrupt, 1, now); status != LoadQuarantined || !status.Active() {
		t.Fatalf("corrupt status = %v", status)
	}
	if _, err := os.Stat(corrupt); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("corrupt file not moved")
	}
	if _, err := os.Stat(corrupt + ".corrupt-20261020T080000Z"); err != nil {
		t.Fatalf("quarantine file missing: %v", err)
	}

	unreadable := filepath.Join(dir, "dir.state")
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, status := Load(unreadable, 1, now); status != LoadDisabled {
		t.Fatalf("unreadable status = %v", status)
	}
}

func TestWriteAtomicModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "a.state")
	if err := WriteAtomic(path, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("two")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v", info.Mode().Perm())
	}
	dirInfo, _ := os.Stat(dir)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", dirInfo.Mode().Perm())
	}
	if got, _ := os.ReadFile(path); string(got) != "two" {
		t.Fatalf("content = %q", got)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("leftover temp files: %d", len(files))
	}
}

func TestWriterSchedulesOnceFromFirstChange(t *testing.T) {
	n := 0
	w, timer, writes := newTestWriter(t, func() ([]byte, error) {
		n++
		return []byte(strings.Repeat("x", n)), nil
	})
	w.MarkDirty()
	w.MarkDirty()
	w.MarkDirty()
	if len(timer.delays) != 1 || timer.delays[0] != DefaultFlushDelay {
		t.Fatalf("timers = %v, want one %v", timer.delays, DefaultFlushDelay)
	}
	timer.fire()
	if len(*writes) != 1 {
		t.Fatalf("writes = %d", len(*writes))
	}
	w.MarkDirty()
	if len(timer.delays) != 2 {
		t.Fatalf("second change did not schedule a write")
	}
}

func TestWriterRetriesAfterFailureAndKeepsChangesDuringIO(t *testing.T) {
	var w *Writer
	fail := true
	timer := &fakeTimer{}
	writes := 0
	w = NewWriter("test", filepath.Join(t.TempDir(), "x.state"), &sync.Mutex{}, func() ([]byte, error) {
		return []byte("snap"), nil
	})
	w.after = timer.after
	w.write = func(string, []byte) error {
		if fail {
			return errors.New("disk full")
		}
		writes++
		// A change made while the file is being written must schedule another write.
		if writes == 1 {
			w.MarkDirty()
		}
		return nil
	}
	w.MarkDirty()
	timer.fire()
	if len(timer.pending) != 1 {
		t.Fatal("failed write was not rescheduled")
	}
	fail = false
	timer.fire()
	if writes != 1 || len(timer.pending) != 1 {
		t.Fatalf("writes=%d pending=%d, want mutation during I/O to reschedule", writes, len(timer.pending))
	}
	timer.fire()
	if writes != 2 {
		t.Fatalf("writes = %d, want 2", writes)
	}
}

func TestWriterSealFlushesPendingAndIgnoresLaterChanges(t *testing.T) {
	w, timer, writes := newTestWriter(t, func() ([]byte, error) { return []byte("s"), nil })
	w.MarkDirty()
	if err := w.SealAndFlush(); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 1 {
		t.Fatalf("seal writes = %d", len(*writes))
	}
	w.MarkDirty()
	timer.fire()
	if err := w.SealAndFlush(); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 1 {
		t.Fatalf("change after seal was written: %d", len(*writes))
	}
}

// A scheduled flush that passed its sealed check before the seal, and resumes
// after a failed final write, must not persist changes made after the seal.
func TestWriterResumedFlushAfterFailedSealWritesNothing(t *testing.T) {
	var cacheMu sync.Mutex
	state := "before-seal"
	w := NewWriter("test", filepath.Join(t.TempDir(), "x.state"), &cacheMu, func() ([]byte, error) {
		return []byte(state), nil
	})
	timer := &fakeTimer{}
	w.after = timer.after
	fail := true
	var written []string
	w.write = func(_ string, data []byte) error {
		if fail {
			return errors.New("disk full")
		}
		written = append(written, string(data))
		return nil
	}
	w.MarkDirty()
	if err := w.SealAndFlush(); err == nil {
		t.Fatal("final write failure not reported")
	}
	fail = false
	cacheMu.Lock()
	state = "after-seal"
	w.MarkDirty()
	cacheMu.Unlock()
	// Equivalent of the timer callback resuming past its sealed check.
	w.flush()
	timer.fire()
	if len(written) != 0 {
		t.Fatalf("changes after the seal were written: %v", written)
	}
}

func TestWriterSealWithoutChangesDoesNotWrite(t *testing.T) {
	w, _, writes := newTestWriter(t, func() ([]byte, error) { return []byte("s"), nil })
	if err := w.SealAndFlush(); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 0 {
		t.Fatal("clean writer wrote on seal")
	}
}

// The final snapshot must be taken under the cache lock right at the seal, so
// a cache mutation can land only before it (persisted) or after it (dropped).
func TestWriterSealSnapshotsUnderCacheLock(t *testing.T) {
	var cacheMu sync.Mutex
	state := "before-seal"
	var w *Writer
	snapshots := 0
	w = NewWriter("test", filepath.Join(t.TempDir(), "x.state"), &cacheMu, func() ([]byte, error) {
		snapshots++
		if cacheMu.TryLock() {
			cacheMu.Unlock()
			t.Error("snapshot ran without the cache lock")
		}
		// A change reported while the seal snapshot is taken must be ignored.
		w.MarkDirty()
		return []byte(state), nil
	})
	timer := &fakeTimer{}
	w.after = timer.after
	var written []string
	w.write = func(_ string, data []byte) error {
		written = append(written, string(data))
		return nil
	}

	cacheMu.Lock()
	w.MarkDirty()
	cacheMu.Unlock()

	// Hold the cache lock while the seal starts: SealAndFlush must wait for it.
	cacheMu.Lock()
	done := make(chan error, 1)
	go func() { done <- w.SealAndFlush() }()
	select {
	case <-done:
		t.Fatal("seal did not wait for the cache lock")
	case <-time.After(50 * time.Millisecond):
	}
	state = "last-change-before-seal"
	cacheMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cacheMu.Lock()
	state = "after-seal"
	w.MarkDirty()
	cacheMu.Unlock()
	timer.fire()

	if len(written) != 1 || written[0] != "last-change-before-seal" {
		t.Fatalf("written = %v, want only the state at the seal", written)
	}
	if w.Dirty() || snapshots != 1 {
		t.Fatalf("dirty=%v snapshots=%d after seal", w.Dirty(), snapshots)
	}
}
