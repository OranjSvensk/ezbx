package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func appendEvent(t *testing.T, j Journal, name string, after uint64) uint64 {
	t.Helper()
	n, err := j.Append(t.Context(), name, struct{}{}, after)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func readHistory(t *testing.T, j Journal) history {
	t.Helper()
	var got history
	if err := j.transaction(t.Context(), func(h *history) bool {
		got = *h
		return false
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReplayHandsOffToLive(t *testing.T) {
	j := Open(t.TempDir(), "t")
	since := time.Now().Add(-time.Second)
	appendEvent(t, j, "box.created", 0)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var got []Record
	err := j.Follow(ctx, &since, func(e Record) error {
		got = append(got, e)
		switch len(got) {
		case 1:
			appendEvent(t, j, "box.started", 0)
		case 2:
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(got) != 2 || got[0].Type != "box.created" || got[1].Type != "box.started" {
		t.Fatalf("events %v, error %v", got, err)
	}
	h := readHistory(t, j)
	if got[0].ID == got[1].ID || got[0].ID != h.Entries[0].ID {
		t.Fatalf("IDs changed or repeated: %v", got)
	}
}

func TestLiveOnlySkipsHistoryAndCancelsWhileIdle(t *testing.T) {
	j := Open(t.TempDir(), "t")
	appendEvent(t, j, "box.created", 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reading := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	j.now = func() time.Time {
		once.Do(func() { close(reading) })
		return time.Now()
	}
	go func() {
		done <- j.Follow(ctx, nil, func(e Record) error {
			t.Errorf("live-only stream replayed %v", e)
			return nil
		})
	}()
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("live reader did not open the journal")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle reader did not stop")
	}
}

func TestDestroyReplacesHistoryAndExpires(t *testing.T) {
	j := Open(t.TempDir(), "t")
	now := time.Now()
	j.now = func() time.Time { return now }
	start := appendEvent(t, j, "exec.started", 0)
	since := now
	now = now.Add(time.Second)
	destroy := appendEvent(t, j, "box.destroyed", 0)
	if n := appendEvent(t, j, "exec.exited", start); n != 0 {
		t.Fatalf("old exec exit was recorded at %d", n)
	}
	var got []Record
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := j.Follow(ctx, &since, func(e Record) error {
		if e.Type != "box.destroyed" {
			t.Fatalf("destroy left a prior event in replay: %+v", e)
		}
		got = append(got, e)
		return nil
	})
	if !errors.Is(err, ErrHistoryLost) || len(got) != 1 || got[0].Type != "box.destroyed" {
		t.Fatalf("events %v, error %v", got, err)
	}
	if n := appendEvent(t, j, "box.started", 0); n != destroy+1 {
		t.Fatalf("name reuse lost sequence: got %d, want %d", n, destroy+1)
	}
	now = now.Add(destroyTTL)
	h := readHistory(t, j)
	if h.Dropped != destroy || len(h.Entries) != 1 || h.Entries[0].Type != "box.started" {
		t.Fatalf("after expiration: %+v", h)
	}
}

func TestConcurrentWritersKeepAllRecords(t *testing.T) {
	j := Open(t.TempDir(), "t")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const writers = 16
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			if _, err := j.Append(ctx, "box.started", struct{}{}, 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	h := readHistory(t, j)
	if len(h.Entries) != writers {
		t.Fatalf("got %d records, want %d", len(h.Entries), writers)
	}
	for i, e := range h.Entries {
		if e.Sequence != uint64(i+1) {
			t.Fatalf("record %d has sequence %d", i, e.Sequence)
		}
	}
}

func TestRetentionGapAndInclusiveReplay(t *testing.T) {
	j := Open(t.TempDir(), "t")
	at := time.Now().UTC()
	h := history{Last: maxRecords}
	for i := range maxRecords {
		h.Entries = append(h.Entries, entry{
			Sequence: uint64(i + 1),
			Record:   Record{Type: "box.started", TS: at.Add(time.Duration(i) * time.Second)},
		})
	}
	if err := j.transaction(t.Context(), func(state *history) bool {
		*state = h
		return true
	}); err != nil {
		t.Fatal(err)
	}
	j.now = func() time.Time { return at.Add(maxRecords * time.Second) }
	appendEvent(t, j, "box.stopped", 0)
	if err := j.Follow(t.Context(), &at, func(e Record) error {
		t.Fatalf("unexpected event after retention gap: %+v", e)
		return nil
	}); !errors.Is(err, ErrHistoryLost) {
		t.Fatalf("expected retention gap, got %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	since := at.Add(time.Second)
	count := 0
	err := j.Follow(ctx, &since, func(e Record) error {
		count++
		if count == 1 && !e.TS.Equal(since) {
			t.Errorf("replay started at %v, want %v", e.TS, since)
		}
		if e.Type == "box.stopped" {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || count != maxRecords {
		t.Fatalf("replayed %d records, error %v", count, err)
	}
}
