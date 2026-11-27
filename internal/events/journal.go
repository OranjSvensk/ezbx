// Package events stores the runtime contract's host-side event history.
package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

const (
	maxRecords        = 1024
	maxGatewayRecords = 256
	destroyTTL        = 24 * time.Hour
)

type Record struct {
	ID   string          `json:"id"`
	TS   time.Time       `json:"ts"`
	Type string          `json:"event"`
	Box  string          `json:"box"`
	Data json.RawMessage `json:"data"`
}

type entry struct {
	Sequence uint64 `json:"sequence"`
	Record
}

type history struct {
	Generation   string    `json:"generation,omitempty"`
	Last         uint64    `json:"last"`
	Dropped      uint64    `json:"dropped"`
	DroppedUntil time.Time `json:"droppedUntil"`
	Destroyed    uint64    `json:"destroyed"`
	Entries      []entry   `json:"entries"`
}

// Journal is private host state. Its directory is separate from the box's
// state directory so destroy can replace the history with a removal receipt.
type Journal struct {
	file    string
	box     string
	now     func() time.Time
	gateway bool
}

func Open(state, box string) Journal {
	return Journal{file: filepath.Join(state, ".events", box+".json"), box: box, now: time.Now}
}

// OpenGateway keeps request traffic separate from lifecycle events. Only the
// gateway writes this file. The host follows atomically published snapshots.
func OpenGateway(logDir, box string) Journal {
	return Journal{file: filepath.Join(logDir, "gateway-events.json"), box: box, now: time.Now, gateway: true}
}

func NewID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// transaction serializes both writers and readers across CLI processes. The
// lock file is stable and separate from the file we replace.
func (j Journal) transaction(ctx context.Context, update func(*history) bool) error {
	if err := os.MkdirAll(filepath.Dir(j.file), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(j.file+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ok, err := tryLock(lock)
		if err != nil {
			return err
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	h, err := j.read()
	if err != nil {
		return err
	}
	dirty := h.expireReceipt(j.now())
	if update(&h) || dirty {
		return j.save(h)
	}
	return nil
}

func (j Journal) read() (history, error) {
	var h history
	raw, err := j.readBytes()
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, fmt.Errorf("reading event journal: %w", err)
	}
	return h, nil
}

func (j Journal) readBytes() ([]byte, error) {
	if !j.gateway || runtime.GOOS != "windows" {
		return os.ReadFile(j.file)
	}
	// On Windows, Root opens files with FILE_SHARE_DELETE so a reader
	// doesn't prevent Docker Desktop from replacing the journal.
	root, err := os.OpenRoot(filepath.Dir(j.file))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(filepath.Base(j.file))
}

func (j Journal) save(h history) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	staging := j.file + ".tmp"
	f, err := os.OpenFile(staging, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(staging)
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	if err := errors.Join(writeErr, syncErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(staging, j.file)
}

func (h *history) drop(n int) {
	for _, e := range h.Entries[:n] {
		h.Dropped = e.Sequence
		if e.TS.After(h.DroppedUntil) {
			h.DroppedUntil = e.TS
		}
	}
	h.Entries = h.Entries[n:]
}

func (h *history) expireReceipt(now time.Time) bool {
	if len(h.Entries) == 0 {
		return false
	}
	first := h.Entries[0]
	if first.Type != "box.destroyed" || first.TS.Add(destroyTTL).After(now) {
		return false
	}
	h.drop(1)
	return true
}

// Append records one completed transition or exec attempt. A nonzero after
// pairs an exec exit with its start and suppresses exits from a destroyed box.
func (j Journal) Append(ctx context.Context, event string, data any, after uint64) (uint64, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	var sequence uint64
	err = j.transaction(ctx, func(h *history) bool {
		if after != 0 && after <= h.Destroyed {
			return false
		}
		if event == "box.destroyed" {
			h.drop(len(h.Entries))
			h.Destroyed = h.Last + 1
		}
		h.Last++
		sequence = h.Last
		ts := j.now().UTC()
		if ts.Before(h.DroppedUntil) {
			ts = h.DroppedUntil
		}
		if n := len(h.Entries); n > 0 && ts.Before(h.Entries[n-1].TS) {
			ts = h.Entries[n-1].TS
		}
		h.Entries = append(h.Entries, entry{
			Sequence: sequence,
			Record:   Record{ID: NewID(), TS: ts, Type: event, Box: j.box, Data: raw},
		})
		limit := maxRecords
		if j.gateway {
			limit = maxGatewayRecords
			if h.Generation == "" {
				h.Generation = NewID()
			}
		}
		if excess := len(h.Entries) - limit; excess > 0 {
			h.drop(excess)
		}
		return true
	})
	return sequence, err
}

var ErrHistoryLost = errors.New("event history is no longer available; check ezbx status and reconnect")

// Follow takes the replay and live boundary from the same journal snapshot.
// The cursor is the last sequence already delivered or intentionally skipped.
func (j Journal) Follow(ctx context.Context, since *time.Time, emit func(Record) error) error {
	var cursor uint64
	var generation string
	first := true
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		h, err := j.snapshot(ctx)
		if err != nil {
			return err
		}
		if generation != "" && h.Generation != generation {
			return ErrHistoryLost
		}
		generation = h.Generation
		if first {
			first = false
			if since == nil {
				cursor = h.Last
			} else {
				if !h.DroppedUntil.IsZero() && !since.After(h.DroppedUntil) {
					if len(h.Entries) > 0 && h.Entries[0].Type == "box.destroyed" {
						if err := emit(h.Entries[0].Record); err != nil {
							return err
						}
					}
					return ErrHistoryLost
				}
				for _, e := range h.Entries {
					if !e.TS.Before(*since) {
						if err := emit(e.Record); err != nil {
							return err
						}
					}
				}
				cursor = h.Last
			}
		}
		if cursor < h.Dropped {
			// A live reader can still see destroy when the same write
			// purged unread records. Other retention gaps require resync.
			if len(h.Entries) == 0 || h.Entries[0].Type != "box.destroyed" {
				return ErrHistoryLost
			}
			if err := emit(h.Entries[0].Record); err != nil {
				return err
			}
			return ErrHistoryLost
		}
		for _, e := range h.Entries {
			if e.Sequence > cursor {
				if err := emit(e.Record); err != nil {
					return err
				}
				cursor = e.Sequence
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (j Journal) snapshot(ctx context.Context) (history, error) {
	if err := ctx.Err(); err != nil {
		return history{}, err
	}
	if j.gateway {
		// Docker Desktop may briefly hold the journal exclusively during
		// replacement. Retain the cursor and retry only that Windows error.
		const sharingViolation = syscall.Errno(32)
		for retries := 0; ; retries++ {
			h, err := j.read()
			if runtime.GOOS != "windows" || !errors.Is(err, sharingViolation) || retries == 25 {
				return h, err
			}
			select {
			case <-ctx.Done():
				return history{}, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	var h history
	err := j.transaction(ctx, func(state *history) bool {
		h = *state
		return false
	})
	return h, err
}
