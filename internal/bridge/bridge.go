// Package bridge receives ZPL jobs, renders them and sends them to a printer.
package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/afonsofigs/zpl-print-bridge/internal/config"
	"github.com/afonsofigs/zpl-print-bridge/internal/pdf"
	"github.com/afonsofigs/zpl-print-bridge/internal/render"
	"github.com/afonsofigs/zpl-print-bridge/internal/tspl"
	"github.com/afonsofigs/zpl-print-bridge/internal/worker"
)

const (
	maxConns       = 4                // connections read at the same time
	maxConnsPerIP  = 2                // per remote address, when listening off loopback
	submitTimeout  = 60 * time.Second // limit for one lp call
	notifyQueueLen = 8                // notifications beyond this are dropped
	notifyMaxBytes = 200              // longer notification text is cut
)

// ErrNotZPL is returned for data that has no ZPL label.
var ErrNotZPL = errors.New("not a ZPL job")

var errHTTP = errors.New("HTTP request refused")

// Renderer renders a job; see render.Render.
type Renderer func(ctx context.Context, zpl []byte, size render.Size, maxLabels int, each func(*render.Label) error) ([]render.Warning, error)

// Bridge renders and prints one job at a time.
type Bridge struct {
	cfg     config.Config
	log     *slog.Logger
	mu      sync.Mutex
	notes   chan [2]string
	pending sync.WaitGroup // notifications queued or running

	connTime time.Duration // total time to receive one job
	perIP    int           // connections per remote address; 0 for no limit
	ipMu     sync.Mutex
	ipConns  map[string]int

	openMu   sync.Mutex
	open     map[net.Conn]struct{} // connections being read
	stopping bool

	// Render renders a job. The default runs it in a worker process.
	Render Renderer
	// Submit sends data to lp with args and returns the lp output.
	Submit func(ctx context.Context, args []string, data []byte) (string, error)
	// Notify shows a desktop notification.
	Notify func(title, body string)
	now    func() time.Time
}

// New returns a Bridge that renders in a worker process, prints with lp and,
// if cfg.Notify is set, notifies with notify-send.
func New(cfg config.Config, log *slog.Logger) *Bridge {
	notify := func(string, string) {}
	if cfg.Notify {
		notify = func(title, body string) {
			if err := notifySend(title, body); err != nil {
				log.Warn("notification failed", "err", err)
			}
		}
	}
	b := &Bridge{
		cfg: cfg, log: log, notes: make(chan [2]string, notifyQueueLen),
		connTime: max(2*cfg.ReadTimeout, time.Minute), ipConns: map[string]int{}, open: map[net.Conn]struct{}{},
		Render: worker.Render, Submit: runLp, Notify: notify, now: time.Now,
	}
	if !cfg.Loopback() {
		b.perIP = maxConnsPerIP
	}
	go func() {
		for n := range b.notes {
			b.Notify(n[0], n[1])
			b.pending.Done()
		}
	}()
	return b
}

// Serve accepts connections on ln until ctx ends. Each connection carries one
// job. When ctx ends, Serve still accepts the connections waiting in the
// listen backlog, and returns after every received job is handled.
func (b *Bridge) Serve(ctx context.Context, ln net.Listener) error {
	tl, _ := ln.(*net.TCPListener)
	stop := context.AfterFunc(ctx, func() {
		if tl != nil {
			tl.SetDeadline(time.Now()) // wakes Accept
		} else {
			ln.Close()
		}
	})
	defer stop()

	slots := make(chan struct{}, maxConns)
	var wg sync.WaitGroup
	defer b.Wait()
	defer wg.Wait()
	defer ln.Close()
	handle := func(conn net.Conn, haveSlot bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !haveSlot {
				slots <- struct{}{}
			}
			defer func() { <-slots }()
			b.handleConn(conn)
		}()
	}

	delay := 5 * time.Millisecond
	for {
		slots <- struct{}{}
		conn, err := ln.Accept()
		if err == nil {
			delay = 5 * time.Millisecond
			handle(conn, true)
			continue
		}
		<-slots
		if ctx.Err() != nil {
			b.log.Info("stopping: finishing the jobs already received")
			b.endReads()
			// Take the connections already queued in the backlog.
			if tl != nil {
				tl.SetDeadline(time.Now().Add(100 * time.Millisecond))
				for {
					conn, err := tl.Accept()
					if err != nil {
						break
					}
					handle(conn, false)
				}
			}
			return nil
		}
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		// Errors such as too many open files pass; retry with backoff.
		b.log.Warn("accept failed, retrying", "err", err, "delay", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		delay = min(delay*2, time.Second)
	}
}

// takeIP counts a connection from host and reports whether it is within the
// per-address limit. Each true result needs a releaseIP.
func (b *Bridge) takeIP(host string) bool {
	if b.perIP == 0 {
		return true
	}
	b.ipMu.Lock()
	defer b.ipMu.Unlock()
	if b.ipConns[host] >= b.perIP {
		return false
	}
	b.ipConns[host]++
	return true
}

func (b *Bridge) releaseIP(host string) {
	if b.perIP == 0 {
		return
	}
	b.ipMu.Lock()
	defer b.ipMu.Unlock()
	if b.ipConns[host]--; b.ipConns[host] <= 0 {
		delete(b.ipConns, host)
	}
}

func (b *Bridge) handleConn(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	host, _, _ := net.SplitHostPort(remote)
	if !b.takeIP(host) {
		b.log.Warn("connection refused: too many from this address", "remote", remote)
		return
	}
	defer b.releaseIP(host)
	data, readErr := b.read(conn)
	switch {
	case errors.Is(readErr, errHTTP):
		b.reject(readErr.Error(), remote)
		return
	case errors.Is(readErr, errTooLarge):
		b.reject(fmt.Sprintf("job larger than max_job_bytes (%d)", b.cfg.MaxJobBytes), remote)
		return
	case readErr != nil:
		// A timeout or reset after complete labels still prints those labels.
		end := lastLabelEnd(data)
		if end < 0 {
			if len(bytes.TrimSpace(data)) > 0 {
				b.reject(fmt.Sprintf("incomplete job (%v)", readErr), remote)
			}
			return
		}
		if len(bytes.TrimSpace(data[end:])) > 0 {
			b.log.Warn("incomplete data after the last label ignored", "remote", remote, "bytes", len(data)-end, "err", readErr)
			b.notify("ZPL job cut short", "The end of the job did not arrive. Only the complete labels print.")
		}
		data = data[:end]
	default:
		if end := lastLabelEnd(data); end >= 0 && hasLabelStart(data[end:]) {
			b.log.Warn("unfinished label after the last ^XZ ignored", "remote", remote)
			b.notify("ZPL job cut short", "The last label has no ^XZ and does not print.")
		}
	}
	if err := b.handle(context.Background(), data, remote); err != nil && !errors.Is(err, ErrNotZPL) {
		b.log.Error("job failed", "remote", remote, "err", worker.Shorten(err.Error(), 300))
	}
}

var errTooLarge = errors.New("job too large")

// endReads ends the reads in progress when the service stops: each connection
// keeps the data it has, and its complete labels print.
func (b *Bridge) endReads() {
	b.openMu.Lock()
	defer b.openMu.Unlock()
	b.stopping = true
	for c := range b.open {
		c.SetReadDeadline(time.Now())
	}
}

// track registers conn for endReads and returns the function that removes it.
func (b *Bridge) track(conn net.Conn) func() {
	b.openMu.Lock()
	defer b.openMu.Unlock()
	b.open[conn] = struct{}{}
	return func() {
		b.openMu.Lock()
		defer b.openMu.Unlock()
		delete(b.open, conn)
	}
}

// read returns the data of one connection. read_timeout is an idle timeout,
// renewed on every read; a connection gets connTime in total. When the
// service stops, the read ends with the data already received.
func (b *Bridge) read(conn net.Conn) ([]byte, error) {
	defer b.track(conn)()
	end := time.Now().Add(b.connTime)
	buf := make([]byte, 64<<10)
	var data []byte
	for first := true; ; first = false {
		deadline := time.Now().Add(b.cfg.ReadTimeout)
		if deadline.After(end) {
			deadline = end
		}
		b.openMu.Lock()
		if b.stopping {
			deadline = time.Now().Add(100 * time.Millisecond)
		}
		conn.SetReadDeadline(deadline)
		b.openMu.Unlock()
		n, err := conn.Read(buf)
		data = append(data, buf[:n]...)
		if int64(len(data)) > b.cfg.MaxJobBytes {
			return nil, errTooLarge
		}
		if first && isHTTP(data) {
			return nil, errHTTP
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return data, nil
			}
			return data, err
		}
	}
}

func hasLabelStart(data []byte) bool { return render.IndexCommand(data, "XA", 0) >= 0 }

// lastLabelEnd returns the index just after the last ^XZ, or -1.
func lastLabelEnd(data []byte) int {
	if i := render.LastIndexCommand(data, "XZ"); i >= 0 {
		return i + 3
	}
	return -1
}

var httpRequest = regexp.MustCompile(`^(GET|POST|PUT|HEAD|OPTIONS|DELETE|PATCH|CONNECT|TRACE) \S+ HTTP/`)

// isHTTP reports an HTTP request line. Web pages can reach local ports, and
// a print port must not print what they send.
func isHTTP(data []byte) bool { return httpRequest.Match(bytes.TrimLeft(data, " \t\r\n")) }

// Handle renders one job and sends it to the archive and the printer.
func (b *Bridge) Handle(ctx context.Context, data []byte) error {
	return b.handle(ctx, data, "")
}

func (b *Bridge) handle(ctx context.Context, data []byte, remote string) error {
	if len(bytes.TrimSpace(data)) == 0 {
		b.log.Debug("empty connection ignored", "remote", remote)
		return nil
	}
	if isHTTP(data) {
		b.reject(errHTTP.Error(), remote)
		return ErrNotZPL
	}
	if !hasLabelStart(data) {
		b.reject(fmt.Sprintf("no ZPL label start (^XA) in %d bytes", len(data)), remote)
		return ErrNotZPL
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.cfg

	var base string
	if c.ArchiveDir != "" {
		var err error
		if base, err = b.archiveZPL(data); err != nil {
			err = fmt.Errorf("archive: %w", err)
			if c.Printer == "" {
				b.notify("ZPL job not saved", err.Error())
				return err
			}
			b.log.Error("archive failed", "err", err)
		}
	}

	var doc *pdf.Doc
	if c.Mode == "pdf" || base != "" {
		doc = pdf.New(c.Size())
	}
	var job *tspl.Job
	if c.Printer != "" && c.Mode == "tspl" {
		job = tspl.NewJob(c.TSPLOptions())
	}
	labels := 0
	warnings, err := b.Render(ctx, data, c.Size(), c.MaxLabels, func(l *render.Label) error {
		labels += max(l.Copies, 1)
		if doc != nil {
			doc.Add(l)
		}
		if job != nil {
			job.Add(l)
		}
		return nil
	})
	if len(warnings) > 0 {
		var text []string
		for _, w := range warnings {
			b.log.Warn("label warning", nonEmpty("label", w.Label, "warning", w.Text, "remote", remote)...)
			text = append(text, w.Text)
		}
		b.notify("ZPL label warning", strings.Join(text, "; "))
	}
	if err != nil {
		b.notify("ZPL job failed", err.Error())
		return err
	}
	var docBytes []byte
	if doc != nil {
		docBytes = doc.Bytes()
	}

	archived := ""
	if base != "" {
		if err := os.WriteFile(base+".pdf", docBytes, 0o600); err != nil {
			err = fmt.Errorf("archive: %w", err)
			if c.Printer == "" {
				b.notify("ZPL job not saved", err.Error())
				return err
			}
			b.log.Error("archive failed", "err", err)
		} else {
			archived = base + ".pdf"
		}
	}

	if c.Printer == "" {
		b.log.Info("job archived", nonEmpty("labels", labels, "file", archived, "remote", remote)...)
		b.notify("Label saved", fmt.Sprintf("%d label(s) saved as %s", labels, archived))
		return nil
	}

	args := []string{"-d", c.Printer, "-t", "zpl-print-bridge"}
	var payload []byte
	if job != nil {
		args = append(args, "-o", "raw")
		payload = job.Bytes()
	} else {
		args = append(args, "-o", "print-scaling=none")
		payload = docBytes
	}
	for _, o := range c.LpOptions {
		args = append(args, "-o", o)
	}

	// A received job is printed even if the service is stopping.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), submitTimeout)
	defer cancel()
	out, err := b.Submit(sctx, args, payload)
	if err != nil {
		msg := fmt.Sprintf("Printing to %s failed: %v", c.Printer, err)
		if archived != "" {
			msg += ". Saved as " + archived
		}
		b.notify("ZPL job not printed", msg)
		return fmt.Errorf("lp: %w", err)
	}
	b.log.Info("job sent to CUPS", nonEmpty("labels", labels, "printer", c.Printer, "mode", c.Mode, "lp", out, "file", archived, "remote", remote)...)
	b.notify("Label sent to printer", fmt.Sprintf("%d label(s) queued on %s", labels, c.Printer))
	return nil
}

// reject logs and notifies a job that is not printed.
func (b *Bridge) reject(reason, remote string) {
	b.log.Warn("job rejected", nonEmpty("reason", reason, "remote", remote)...)
	b.notify("ZPL job rejected", reason)
}

var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// notify queues a notification. When the queue is full it is dropped, so a
// flood of bad jobs can't start a process per job. The body can contain text
// from the job, so it is cut and its markup is escaped.
func (b *Bridge) notify(title, body string) {
	body = markup.Replace(worker.Shorten(strings.ToValidUTF8(body, "?"), notifyMaxBytes))
	b.pending.Add(1)
	select {
	case b.notes <- [2]string{title, body}:
	default:
		b.pending.Done()
		b.log.Debug("notification dropped", "title", title)
	}
}

// Wait returns when every queued notification has been shown.
func (b *Bridge) Wait() { b.pending.Wait() }

// archiveZPL writes the job before rendering, so failed jobs are kept too.
// It returns the base path for the matching .pdf.
func (b *Bridge) archiveZPL(data []byte) (string, error) {
	if err := os.MkdirAll(b.cfg.ArchiveDir, 0o700); err != nil {
		return "", err
	}
	stamp := b.now().UTC().Format("20060102T150405.000Z")
	for i := 0; i < 100; i++ {
		name := stamp
		if i > 0 {
			name = fmt.Sprintf("%s-%d", stamp, i)
		}
		base := filepath.Join(b.cfg.ArchiveDir, name)
		f, err := os.OpenFile(base+".zpl", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return base, werr
	}
	return "", errors.New("no free file name")
}

// lpCommand returns the lp command. It runs in its own process group, so a
// Ctrl+C or a stop signal to the service's group doesn't cancel a submission.
func lpCommand(ctx context.Context, args []string, data []byte) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "lp", args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func runLp(ctx context.Context, args []string, data []byte) (string, error) {
	out, err := lpCommand(ctx, args, data).CombinedOutput()
	out = bytes.TrimSpace(out)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, out)
	}
	return string(out), nil
}

func notifySend(title, body string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "notify-send", "-a", "zpl-print-bridge", title, body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// nonEmpty returns the key-value pairs without the empty strings.
func nonEmpty(kv ...any) []any {
	var out []any
	for i := 0; i+1 < len(kv); i += 2 {
		if v, ok := kv[i+1].(string); ok && v == "" {
			continue
		}
		out = append(out, kv[i], kv[i+1])
	}
	return out
}
