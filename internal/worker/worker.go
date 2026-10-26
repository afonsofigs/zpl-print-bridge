// Package worker renders a job in a child process with a memory limit.
//
// Some malformed ZPL makes zebrash allocate gigabytes. In Go that is a fatal
// error that recover() can't catch. The renderer runs in a child process, so
// only that job fails.
package worker

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

// Arg is the hidden command that starts a worker: <binary> __render ...
const Arg = "__render"

// MemoryLimit caps the data segment of the child process.
const MemoryLimit = 512 << 20

// Time limits for one job. The worker gets FirstLabel to parse the job and
// draw the first label, then LabelTime for each further label. MaxJobTime
// caps the whole job, below the unit's TimeoutStopSec.
const (
	FirstLabel = 60 * time.Second
	MaxJobTime = 4 * time.Minute
)

// LabelTime is the time allowed per label: 10 s per million dots, at least 10 s.
func LabelTime(size render.Size) time.Duration {
	w, h := size.Dots()
	return time.Duration(max(1, float64(w)*float64(h)/1e6) * float64(10*time.Second))
}

type limits struct{ first, label, total time.Duration }

// limitsFor returns the time limits; tests replace it.
var limitsFor = func(size render.Size) limits { return limits{FirstLabel, LabelTime(size), MaxJobTime} }

// Command returns the command that starts a worker. /proc/self/exe is the
// running binary, also after an upgrade replaces the file on disk.
// Tests replace it.
var Command = func(ctx context.Context, args ...string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "/proc/self/exe", append([]string{Arg}, args...)...), nil
}

type message struct {
	Label    *render.Label
	Warnings []render.Warning
	Err      string
	TooMany  bool
}

// Render has the same contract as render.Render, but runs it in a worker
// process. The worker ignores ctx cancellation and signals sent to the
// service's process group. It stops when a label takes too long.
func Render(ctx context.Context, zpl []byte, size render.Size, maxLabels int, each func(*render.Label) error) ([]render.Warning, error) {
	lim := limitsFor(size)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lim.total)
	defer cancel()
	var timedOut atomic.Bool
	idle := time.AfterFunc(lim.first, func() { timedOut.Store(true); cancel() })
	defer idle.Stop()
	cmd, err := Command(ctx,
		strconv.Itoa(size.DPI),
		strconv.FormatFloat(size.WidthMm, 'f', -1, 64),
		strconv.FormatFloat(size.HeightMm, 'f', -1, 64),
		strconv.Itoa(maxLabels))
	if err != nil {
		return nil, err
	}
	cmd.Stdin = bytes.NewReader(zpl)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start renderer: %w", err)
	}

	var warnings []render.Warning
	var result error
	labels := 0
	dec := gob.NewDecoder(out)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			if !errors.Is(err, io.EOF) && result == nil {
				result = fmt.Errorf("read renderer output: %w", err)
			}
			break
		}
		switch {
		case m.Label != nil:
			idle.Reset(lim.label)
			labels++
			if result == nil {
				result = each(m.Label)
			}
		case m.Warnings != nil:
			warnings = m.Warnings
		case m.TooMany:
			result = fmt.Errorf("%w (limit %d)", render.ErrTooManyLabels, maxLabels)
		case m.Err != "":
			result = errors.New(m.Err)
		}
	}
	io.Copy(io.Discard, out)
	err = cmd.Wait()
	switch {
	case timedOut.Load() || ctx.Err() == context.DeadlineExceeded:
		result = fmt.Errorf("renderer timed out after %d label(s)", labels)
	case result != nil:
	case err != nil:
		result = fmt.Errorf("renderer stopped (%v): %s", err, summary(stderr.Bytes()))
	case labels == 0:
		result = errors.New("renderer returned no labels")
	}
	return warnings, result
}

// Main runs in the child process. args are dpi, width, height and max labels.
// The ZPL comes on stdin; labels, warnings and the result go to stdout.
func Main(args []string) {
	if len(args) != 4 {
		fmt.Fprintln(os.Stderr, "worker: expected 4 arguments")
		os.Exit(2)
	}
	dpi, err1 := strconv.Atoi(args[0])
	w, err2 := strconv.ParseFloat(args[1], 64)
	h, err3 := strconv.ParseFloat(args[2], 64)
	maxLabels, err4 := strconv.Atoi(args[3])
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(2)
	}
	size := render.Size{WidthMm: w, HeightMm: h, DPI: dpi}

	// The limits also hold if the parent dies and leaves the worker running.
	mem := uint64(MemoryLimit)
	cpu := uint64(MaxJobTime/time.Second) + 5
	if err := errors.Join(
		syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: mem, Max: mem}),
		syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: cpu, Max: cpu}),
	); err != nil {
		fmt.Fprintln(os.Stderr, "worker: limits:", err)
		os.Exit(2)
	}
	debug.SetMemoryLimit(int64(mem) * 3 / 4)
	zpl, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(2)
	}

	enc := gob.NewEncoder(os.Stdout)
	warnings, err := render.Render(zpl, size, maxLabels, func(l *render.Label) error {
		return enc.Encode(message{Label: l})
	})
	if warnings != nil {
		enc.Encode(message{Warnings: warnings})
	}
	if err != nil {
		enc.Encode(message{Err: Shorten(err.Error(), 500), TooMany: errors.Is(err, render.ErrTooManyLabels)})
	}
	os.Exit(0)
}

// summary returns the Go runtime's "fatal error:" or "panic:" line, or the
// first line of the child's error output.
func summary(stderr []byte) string {
	lines := bytes.Split(bytes.TrimSpace(stderr), []byte("\n"))
	pick := lines[0]
	for _, l := range lines {
		if bytes.HasPrefix(l, []byte("fatal error:")) || bytes.HasPrefix(l, []byte("panic:")) {
			pick = l
			break
		}
	}
	if len(pick) > 200 {
		pick = pick[:200]
	}
	if len(pick) == 0 {
		return "no error output"
	}
	return string(pick)
}

// Shorten cuts s to at most n bytes on a UTF-8 boundary. zebrash errors quote
// the failing command, which can be megabytes long.
func Shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
