package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

// The test binary runs the worker when started with Arg, like the real binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == Arg {
		Main(os.Args[2:])
		return
	}
	Command = func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, os.Args[0], append([]string{Arg}, args...)...), nil
	}
	os.Exit(m.Run())
}

var size4x6 = render.Size{WidthMm: 101.6, HeightMm: 152.4, DPI: 203}

func TestRendersLabels(t *testing.T) {
	var labels []*render.Label
	warnings, err := Render(context.Background(),
		[]byte("^XA^FO10,10^GB100,100,100^FS^PQ2^XZ^XA^PW1200^FO10,10^GB50,50,50^FS^XZ"),
		size4x6, 10, func(l *render.Label) error { labels = append(labels, l); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels[0].Copies != 2 || !labels[0].At(15, 15) || labels[1].Width != 812 {
		t.Fatalf("unexpected labels: %d", len(labels))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Text, "^PW") {
		t.Errorf("warnings = %+v", warnings)
	}
}

func TestErrorsComeBack(t *testing.T) {
	ok := func(*render.Label) error { return nil }
	_, err := Render(context.Background(), []byte("^XA^FO10,10^GB10,10,10^FS^PQ9^XZ"), size4x6, 5, ok)
	if !errors.Is(err, render.ErrTooManyLabels) {
		t.Errorf("err = %v, want ErrTooManyLabels", err)
	}
	if _, err := Render(context.Background(), []byte("^XA^GF^XZ"), size4x6, 5, ok); err == nil || !strings.Contains(err.Error(), "renderer failed") {
		t.Errorf("panic not reported: %v", err)
	}
}

// A job that makes the renderer allocate gigabytes kills only the worker.
func TestMemoryLimit(t *testing.T) {
	_, err := Render(context.Background(), []byte("^XA^FO0,0^A0N,32000,32000^FDWWWWWWWW^FS^XZ"), size4x6, 5,
		func(*render.Label) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "renderer stopped") {
		t.Fatalf("err = %v, want the worker to stop", err)
	}
	if !strings.Contains(err.Error(), "out of memory") {
		t.Errorf("err = %v, want the out of memory message", err)
	}
}

func TestCallbackErrorStopsLabels(t *testing.T) {
	stop := errors.New("stop")
	n := 0
	_, err := Render(context.Background(), []byte("^XA^FO1,1^GB5,5,5^FS^XZ^XA^FO1,1^GB5,5,5^FS^XZ"), size4x6, 5,
		func(*render.Label) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Errorf("err = %v after %d labels, want stop after 1", err, n)
	}
}

// Stopping the service must not stop a render in progress: the worker
// ignores ctx and runs in its own process group.
func TestIgnoresCancelAndOwnGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orig := Command
	defer func() { Command = orig }()
	var cmd *exec.Cmd
	Command = func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		c, err := orig(ctx, args...)
		cmd = c
		return c, err
	}
	n := 0
	if _, err := Render(ctx, []byte("^XA^FO1,1^GB5,5,5^FS^XZ"), size4x6, 5, func(*render.Label) error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("cancelled ctx stopped the render: %v, %d labels", err, n)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("worker shares the service's process group")
	}
}

func TestTimeoutAndNoLabels(t *testing.T) {
	origCmd, origLimits := Command, limitsFor
	defer func() { Command, limitsFor = origCmd, origLimits }()
	limitsFor = func(render.Size) limits { return limits{200 * time.Millisecond, time.Second, time.Minute} }
	Command = func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "5"), nil
	}
	ok := func(*render.Label) error { return nil }
	if _, err := Render(context.Background(), []byte("^XA^XZ"), size4x6, 5, ok); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want timed out", err)
	}
	Command = func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "true"), nil
	}
	if _, err := Render(context.Background(), []byte("^XA^XZ"), size4x6, 5, ok); err == nil || !strings.Contains(err.Error(), "no labels") {
		t.Errorf("err = %v, want no labels", err)
	}
}

func TestLabelTimeScales(t *testing.T) {
	small := LabelTime(size4x6)
	big := LabelTime(render.Size{WidthMm: 101.6, HeightMm: 152.4, DPI: 600})
	if small != 10*time.Second || big <= small {
		t.Errorf("LabelTime = %s and %s", small, big)
	}
}

// The time limit restarts with each label, so a long job of normal labels
// finishes while a stuck label stops the worker.
func TestTimeLimitIsPerLabel(t *testing.T) {
	orig := limitsFor
	defer func() { limitsFor = orig }()
	limitsFor = func(render.Size) limits { return limits{5 * time.Second, 2 * time.Second, time.Minute} }
	job := strings.Repeat("^XA^FO10,10^GB100,100,100^FS^XZ", 20)
	n := 0
	if _, err := Render(context.Background(), []byte(job), size4x6, 50, func(*render.Label) error { n++; return nil }); err != nil || n != 20 {
		t.Fatalf("err = %v after %d labels", err, n)
	}
}

func TestShorten(t *testing.T) {
	if got := Shorten("abcdef", 10); got != "abcdef" {
		t.Errorf("short text changed: %q", got)
	}
	if got := Shorten("ããããã", 3); got != "ã..." {
		t.Errorf("Shorten cut a character: %q", got)
	}
}
