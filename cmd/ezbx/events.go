package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/OranjSvensk/ezbx/internal/events"
)

// recordEvent reports a journal failure on stderr without changing the
// result of an operation that already took effect.
func recordEvent(t target, name string, data map[string]any, after uint64, stderr io.Writer) uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sequence, err := events.Open(t.dirs.State, t.name).Append(ctx, name, data, after)
	if err != nil {
		fmt.Fprintf(stderr, "ezbx events: couldn't record %s: %v\n", name, err)
		return 0
	}
	return sequence
}

func runEvents(args []string, stdout, stderr io.Writer) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return streamEvents(ctx, args, stdout, stderr)
}

func streamEvents(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonl := fs.Bool("jsonl", false, "write one JSON line per event")
	cfg := fs.String("config", "", "config file")
	box := fs.String("box", "", "box name")
	var since *time.Time
	fs.Func("since", "replay from an RFC3339 timestamp, inclusive", func(value string) error {
		ts, err := time.Parse(time.RFC3339Nano, value)
		if err == nil {
			since = &ts
		}
		return err
	})
	words, rest, err := parseInterleaved(fs, args)
	if err != nil {
		return 2
	}
	if !*jsonl || len(words) != 0 || len(rest) != 0 {
		return report(stderr, usageError("Usage: ezbx events --jsonl [--box NAME] [--config FILE] [--since RFC3339]"))
	}
	// With an explicit name, the journal remains addressable even after
	// destroy --remove-config removes the entire selected config file.
	if *box != "" && *cfg != "" {
		if _, err := os.Stat(*cfg); errors.Is(err, os.ErrNotExist) {
			*cfg = ""
		}
	}
	t, err := resolveBox(*cfg, *box)
	if err != nil {
		return report(stderr, err)
	}
	encoder := json.NewEncoder(stdout)
	err = events.Open(t.dirs.State, t.name).Follow(ctx, since, func(record events.Record) error {
		return encoder.Encode(record)
	})
	if errors.Is(err, context.Canceled) {
		return 0
	}
	return report(stderr, err)
}

func execWithEvents(ctx context.Context, t target, o execOptions, argv []string, stdout, stderr, diagnostics io.Writer) (int, time.Duration, error) {
	id := events.NewID()
	sequence := recordEvent(t, "exec.started", map[string]any{
		"execId": id, "program": eventProgram(o.command[0]), "argCount": len(o.command) - 1,
	}, 0, diagnostics)
	start := time.Now()
	code, err := runDocker(ctx, argv, os.Stdin, stdout, stderr)
	took := time.Since(start)
	if sequence != 0 {
		data := map[string]any{"execId": id, "durationMs": took.Milliseconds(), "timedOut": timedOut(o, code, took)}
		if err == nil {
			data["exitCode"] = code
		} else {
			data["errorCode"] = codeOf(err)
		}
		recordEvent(t, "exec.exited", data, sequence, diagnostics)
	}
	return code, took, err
}

func eventProgram(command string) string {
	name := path.Base(strings.ReplaceAll(command, "\\", "/"))
	if len(name) > 128 || name == "." || name == ".." || name == "/" {
		return "(unnamed)"
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._+-", c)) {
			return "(unnamed)"
		}
	}
	return name
}
