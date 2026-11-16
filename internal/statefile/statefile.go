// Package statefile persists small in-memory caches to a local file so they
// survive a graceful restart. It is a local addition and is intentionally
// independent from the Home key-value backend.
package statefile

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/MoeForge/moego/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultFlushDelay bounds how long the first unsaved change waits before a
// write starts. Later changes never postpone an already scheduled write.
const DefaultFlushDelay = 5 * time.Second

const warnInterval = time.Minute

// ResolvePath returns path made absolute relative to baseDir. An empty path
// disables persistence and is returned unchanged.
func ResolvePath(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(baseDir, path)
}

// LoadStatus describes the outcome of reading a state file.
type LoadStatus int

const (
	// LoadMissing means no file exists; persistence stays active.
	LoadMissing LoadStatus = iota
	// LoadOK means the envelope was parsed; persistence stays active.
	LoadOK
	// LoadQuarantined means an unparseable file was renamed aside; persistence stays active.
	LoadQuarantined
	// LoadDisabled means the file was left untouched and persistence must stay off until restart.
	LoadDisabled
)

// Active reports whether persistence may write the file after this load.
func (s LoadStatus) Active() bool { return s != LoadDisabled }

// Load reads a versioned state file and returns its raw entries.
//
// An unparseable file is moved to <path>.corrupt-<timestamp> and loading starts
// empty. A parseable file with another version is left untouched and disables
// persistence. Any I/O failure other than a missing file also disables
// persistence so the original is never overwritten blindly.
func Load(path string, version int, now time.Time) ([]json.RawMessage, LoadStatus) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, LoadMissing
		}
		log.Warnf("state file %s: read failed, persistence disabled until restart: %v", path, errRead)
		return nil, LoadDisabled
	}
	// The version is read on its own first, so a newer format with a different
	// entries shape is left untouched instead of being quarantined.
	var header struct {
		Version *int `json:"version"`
	}
	if errDecode := json.Unmarshal(data, &header); errDecode != nil || header.Version == nil {
		return nil, quarantine(path, now)
	}
	if *header.Version != version {
		log.Warnf("state file %s: unsupported version %d (want %d), left untouched; persistence disabled until restart", path, *header.Version, version)
		return nil, LoadDisabled
	}
	var env struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if errDecode := json.Unmarshal(data, &env); errDecode != nil {
		return nil, quarantine(path, now)
	}
	return env.Entries, LoadOK
}

func quarantine(path string, now time.Time) LoadStatus {
	target := fmt.Sprintf("%s.corrupt-%s", path, now.UTC().Format("20060102T150405Z"))
	if errRename := os.Rename(path, target); errRename != nil {
		log.Warnf("state file %s: unparseable and quarantine failed, persistence disabled until restart: %v", path, errRename)
		return LoadDisabled
	}
	log.Warnf("state file %s: unparseable, moved to %s; starting empty", path, target)
	return LoadQuarantined
}

// Encode builds the versioned envelope for entries.
func Encode(version int, entries any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if errEncode := enc.Encode(struct {
		Version int `json:"version"`
		Entries any `json:"entries"`
	}{Version: version, Entries: entries}); errEncode != nil {
		return nil, errEncode
	}
	return buf.Bytes(), nil
}

// WriteAtomic writes data to path through a synced temporary file and rename.
// The directory is created with mode 0700 and the file ends with mode 0600.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	tmp, errTemp := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if errTemp != nil {
		return errTemp
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		cleanup()
		return errChmod
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		cleanup()
		return errWrite
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		cleanup()
		return errSync
	}
	if errClose := tmp.Close(); errClose != nil {
		cleanup()
		return errClose
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		cleanup()
		return errRename
	}
	if dirHandle, errOpen := os.Open(dir); errOpen == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

// Writer serializes debounced snapshots of one cache to one file.
type Writer struct {
	name     string
	path     string
	delay    time.Duration
	locker   sync.Locker
	snapshot func() ([]byte, error)
	write    func(path string, data []byte) error
	now      func() time.Time
	after    func(time.Duration, func()) stopper

	mu            sync.Mutex
	dirty         bool
	timer         stopper
	sealed        bool
	sealNotified  bool
	lastWarnAt    time.Time
	writeMu       sync.Mutex
	successWrites int
}

type stopper interface{ Stop() bool }

// NewWriter creates a writer for path. locker is the cache lock; snapshot
// returns the full encoded file content and is always called with locker held.
// The cache may call MarkDirty while holding locker (lock order: locker, then
// the Writer's own lock).
func NewWriter(name, path string, locker sync.Locker, snapshot func() ([]byte, error)) *Writer {
	return &Writer{
		name:     name,
		path:     path,
		delay:    DefaultFlushDelay,
		locker:   locker,
		snapshot: snapshot,
		write:    WriteAtomic,
		now:      time.Now,
		after: func(d time.Duration, fn func()) stopper {
			return time.AfterFunc(d, fn)
		},
	}
}

// Path returns the target file path.
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// Dirty reports whether a change is waiting to be written.
func (w *Writer) Dirty() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dirty
}

// MarkDirty records an unsaved change and schedules a write if none is pending.
// It never blocks on I/O and is safe to call while holding the cache lock.
func (w *Writer) MarkDirty() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealed {
		if !w.sealNotified {
			w.sealNotified = true
			log.Debugf("state file %s (%s): change after shutdown seal is not persisted", w.path, w.name)
		}
		return
	}
	if w.dirty {
		return
	}
	w.dirty = true
	w.timer = w.after(w.delay, w.flushScheduled)
}

func (w *Writer) flushScheduled() {
	w.mu.Lock()
	if w.sealed {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.flush()
}

// flush writes one snapshot. The dirty flag is cleared before the snapshot is
// taken, so changes made during I/O schedule another write.
func (w *Writer) flush() bool {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	w.mu.Lock()
	// A scheduled flush may have been waiting for writeMu while the writer was
	// sealed; after the seal only SealAndFlush writes, and only its own snapshot.
	if w.sealed || !w.dirty {
		clean := !w.dirty
		w.mu.Unlock()
		return clean
	}
	w.dirty = false
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()

	w.locker.Lock()
	data, errSnapshot := w.snapshot()
	w.locker.Unlock()
	return w.writeSnapshot(data, errSnapshot)
}

func (w *Writer) writeSnapshot(data []byte, errSnapshot error) bool {
	if errSnapshot == nil {
		errSnapshot = w.write(w.path, data)
	}
	if errSnapshot != nil {
		w.mu.Lock()
		now := w.now()
		if w.lastWarnAt.IsZero() || now.Sub(w.lastWarnAt) >= warnInterval {
			w.lastWarnAt = now
			log.Warnf("state file %s (%s): write failed, keeping data in memory and retrying: %v", w.path, w.name, errSnapshot)
		}
		sealed := w.sealed
		if !w.dirty {
			w.dirty = true
			if !sealed {
				w.timer = w.after(w.delay, w.flushScheduled)
			}
		}
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	w.successWrites++
	w.mu.Unlock()
	return true
}

// SealAndFlush stops accepting changes and writes any pending snapshot. The
// seal and the final snapshot happen under the cache lock, so the file holds
// exactly the state at the seal; later mutations are not persisted.
func (w *Writer) SealAndFlush() error {
	if w == nil {
		return nil
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	w.locker.Lock()
	w.mu.Lock()
	w.sealed = true
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	dirty := w.dirty
	w.dirty = false
	w.mu.Unlock()
	var data []byte
	var errSnapshot error
	if dirty {
		data, errSnapshot = w.snapshot()
	}
	w.locker.Unlock()
	if !dirty {
		return nil
	}
	if !w.writeSnapshot(data, errSnapshot) {
		return fmt.Errorf("state file %s (%s): final write failed", w.path, w.name)
	}
	return nil
}
