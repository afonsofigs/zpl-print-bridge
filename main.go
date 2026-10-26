// Command zpl-print-bridge accepts ZPL like a network Zebra printer and prints
// it on a printer that does not understand ZPL.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/afonsofigs/zpl-print-bridge/internal/bridge"
	"github.com/afonsofigs/zpl-print-bridge/internal/config"
	"github.com/afonsofigs/zpl-print-bridge/internal/pdf"
	"github.com/afonsofigs/zpl-print-bridge/internal/render"
	"github.com/afonsofigs/zpl-print-bridge/internal/tspl"
	"github.com/afonsofigs/zpl-print-bridge/internal/worker"
)

// exitConfig is EX_CONFIG from sysexits.h. The systemd unit does not restart on it.
const exitConfig = 78

const usage = `Usage:
  zpl-print-bridge [-config FILE] [serve]              listen for ZPL jobs and print them
  zpl-print-bridge [-config FILE] print FILE           print one ZPL file ("-" reads stdin)
  zpl-print-bridge [-config FILE] render FILE OUT      write the labels to OUT, which must end in .pdf or .tspl
  zpl-print-bridge -version

Options go before the command.
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == worker.Arg {
		worker.Main(os.Args[2:])
		return
	}
	defaultPath := defaultConfigPath()
	cfgPath := flag.String("config", defaultPath, "configuration file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	if *showVersion {
		fmt.Println(version())
		return
	}

	args := flag.Args()
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	wantArgs := map[string]int{"serve": 0, "print": 1, "render": 2}
	if n, known := wantArgs[cmd]; !known || len(args) != n {
		flag.Usage()
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })
	cfg, err := config.Load(*cfgPath, explicit)
	if err != nil {
		fail(err)
	}
	if cmd == "serve" || cmd == "print" {
		if err := cfg.RequireOutput(); err != nil {
			fail(fmt.Errorf("%w: %s: %v", config.ErrInvalid, *cfgPath, err))
		}
	}

	switch {
	case cmd == "serve" && len(args) == 0:
		// The first signal starts a clean stop; a second one ends the process.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		context.AfterFunc(ctx, stop)
		// Keeps the service process well inside the unit's memory limit.
		debug.SetMemoryLimit(384 << 20)
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			// An address in use or not yet assigned can work later, so systemd
			// restarts the service for those.
			if !errors.Is(err, syscall.EADDRINUSE) && !errors.Is(err, syscall.EADDRNOTAVAIL) {
				err = fmt.Errorf("%w: listen: %v", config.ErrInvalid, err)
			}
			fail(err)
		}
		log.Info("listening", "addr", ln.Addr().String(), "printer", cfg.Printer, "mode", cfg.Mode, "version", version())
		if !cfg.Loopback() {
			log.Warn("listening on a network address: any device that reaches it can print, without authentication", "addr", ln.Addr().String())
		}
		if err := bridge.New(cfg, log).Serve(ctx, ln); err != nil {
			fail(err)
		}
	case cmd == "print" && len(args) == 1:
		data := readInput(args[0])
		if len(bytes.TrimSpace(data)) == 0 {
			fail(fmt.Errorf("%s: no data", args[0]))
		}
		b := bridge.New(cfg, log)
		err := b.Handle(context.Background(), data)
		b.Wait()
		if err != nil {
			fail(err)
		}
	case cmd == "render" && len(args) == 2:
		if err := renderFile(cfg, readInput(args[0]), args[1], log); err != nil {
			fail(err)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func renderFile(cfg config.Config, data []byte, out string, log *slog.Logger) error {
	var add func(*render.Label)
	var finish func() []byte
	switch strings.ToLower(filepath.Ext(out)) {
	case ".pdf":
		d := pdf.New(cfg.Size())
		add, finish = d.Add, d.Bytes
	case ".tspl":
		j := tspl.NewJob(cfg.TSPLOptions())
		add, finish = j.Add, j.Bytes
	default:
		return fmt.Errorf("output must end in .pdf or .tspl: %s", out)
	}
	warnings, err := worker.Render(context.Background(), data, cfg.Size(), cfg.MaxLabels, func(l *render.Label) error { add(l); return nil })
	for _, w := range warnings {
		log.Warn("label warning", "label", w.Label, "warning", w.Text)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(out, finish(), 0o644)
}

func readInput(path string) []byte {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		fail(err)
	}
	return data
}

func defaultConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "zpl-print-bridge.conf"
	}
	return filepath.Join(dir, "zpl-print-bridge", "config")
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "zpl-print-bridge:", err)
	if errors.Is(err, config.ErrInvalid) {
		os.Exit(exitConfig)
	}
	os.Exit(1)
}
