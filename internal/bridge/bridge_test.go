package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/afonsofigs/zpl-print-bridge/internal/config"
	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

const sample = "^XA^FO50,50^BY3^BCN,200,N,N,N^FDTEST-0001^FS^FO50,300^A0N,40,40^FDSample label^FS^XZ"

type submission struct {
	args []string
	data []byte
}

type recorder struct {
	mu     sync.Mutex
	subs   []submission
	notes  []string
	err    error
	called chan struct{}
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.subs) }

func (r *recorder) notifications() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notes...)
}

func newBridge(t *testing.T, cfg config.Config) (*Bridge, *recorder) {
	t.Helper()
	rec := &recorder{called: make(chan struct{}, 100)}
	b := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The worker process is tested in its own package; here render in process.
	b.Render = func(_ context.Context, zpl []byte, size render.Size, maxLabels int, each func(*render.Label) error) ([]render.Warning, error) {
		return render.Render(zpl, size, maxLabels, each)
	}
	b.Submit = func(_ context.Context, args []string, data []byte) (string, error) {
		rec.mu.Lock()
		rec.subs = append(rec.subs, submission{args, data})
		err := rec.err
		rec.mu.Unlock()
		rec.called <- struct{}{}
		return "request id is q-1 (1 file(s))", err
	}
	b.Notify = func(title, body string) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.notes = append(rec.notes, title+": "+body)
	}
	b.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	return b, rec
}

func printerConfig() config.Config {
	c := config.Default()
	c.Printer = "Label-Printer"
	return c
}

func TestTSPLJob(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	if err := b.Handle(context.Background(), []byte(sample)); err != nil {
		t.Fatal(err)
	}
	s := rec.subs[0]
	if got := strings.Join(s.args, " "); got != "-d Label-Printer -t zpl-print-bridge -o raw" {
		t.Errorf("lp args = %q", got)
	}
	if !bytes.HasPrefix(s.data, []byte("SIZE 102 mm,153 mm\r\n")) || !bytes.HasSuffix(s.data, []byte("PRINT 1,1\r\n")) {
		t.Errorf("payload is not a TSPL job: %q...", s.data[:40])
	}
}

func TestPDFJobWithArchive(t *testing.T) {
	cfg := printerConfig()
	cfg.Mode = "pdf"
	cfg.LpOptions = []string{"media=w288h432"}
	cfg.ArchiveDir = filepath.Join(t.TempDir(), "archive")
	b, rec := newBridge(t, cfg)
	for i := 0; i < 2; i++ {
		if err := b.Handle(context.Background(), []byte(sample)); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(rec.subs[0].args, " "); !strings.HasSuffix(got, "-o print-scaling=none -o media=w288h432") {
		t.Errorf("lp args = %q", got)
	}
	if !bytes.HasPrefix(rec.subs[0].data, []byte("%PDF-")) {
		t.Error("payload is not a PDF")
	}
	// Two jobs in the same millisecond keep separate files.
	for _, name := range []string{"20260102T030405.000Z.zpl", "20260102T030405.000Z.pdf", "20260102T030405.000Z-1.zpl", "20260102T030405.000Z-1.pdf"} {
		info, err := os.Stat(filepath.Join(cfg.ArchiveDir, name))
		if err != nil {
			t.Error(err)
		} else if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestArchiveOnlyAndFailedJobsAreKept(t *testing.T) {
	cfg := config.Default()
	cfg.ArchiveDir = t.TempDir()
	b, rec := newBridge(t, cfg)
	if err := b.Handle(context.Background(), []byte(sample)); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 0 {
		t.Error("printed without a printer")
	}
	bad := "^XA^FO10,10^GFA,0,0,0,^FS^XZ"
	if err := b.Handle(context.Background(), []byte(bad)); err == nil {
		t.Fatal("renderer failure not returned")
	}
	kept, _ := os.ReadFile(filepath.Join(cfg.ArchiveDir, "20260102T030405.000Z-1.zpl"))
	if string(kept) != bad {
		t.Error("failed job not archived")
	}
}

func TestArchiveFailureInArchiveOnlyMode(t *testing.T) {
	cfg := config.Default()
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	cfg.ArchiveDir = filepath.Join(file, "sub") // cannot be created
	b, _ := newBridge(t, cfg)
	if err := b.Handle(context.Background(), []byte(sample)); err == nil {
		t.Error("archive failure not returned")
	}
}

func TestRejectedInput(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	for _, in := range []string{
		"SIZE 100 mm,150 mm\r\nPRINT 1\r\n",
		"POST / HTTP/1.1\r\nHost: 127.0.0.1:9100\r\nContent-Length: 20\r\n\r\n^XA^FDhi^FS^XZ",
	} {
		if err := b.Handle(context.Background(), []byte(in)); !errors.Is(err, ErrNotZPL) {
			t.Errorf("%q: err = %v, want ErrNotZPL", in[:12], err)
		}
	}
	if err := b.Handle(context.Background(), []byte(" \r\n")); err != nil {
		t.Errorf("empty input: %v", err)
	}
	b.Wait()
	if rec.count() != 0 {
		t.Error("rejected input was printed")
	}
	if len(rec.notes) != 2 {
		t.Errorf("notifications = %q, want 2", rec.notes)
	}
}

func TestPrintFailureNotifies(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	rec.err = errors.New("no such queue")
	if err := b.Handle(context.Background(), []byte(sample)); err == nil {
		t.Fatal("error not returned")
	}
	b.Wait()
	if len(rec.notes) != 1 || !strings.Contains(rec.notes[0], "not printed") {
		t.Errorf("notifications = %q", rec.notes)
	}
}

// Jobs are printed one at a time even when they arrive together.
func TestJobsDoNotOverlap(t *testing.T) {
	b, _ := newBridge(t, printerConfig())
	var active, overlaps atomic.Int32
	b.Submit = func(context.Context, []string, []byte) (string, error) {
		if active.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return "", nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.Handle(context.Background(), []byte(sample)) }()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Error("two jobs were submitted at the same time")
	}
}

// Stopping the service does not cancel a job that was already received.
func TestShutdownKeepsReceivedJob(t *testing.T) {
	b, _ := newBridge(t, printerConfig())
	started := make(chan struct{})
	var submitErr error
	b.Submit = func(ctx context.Context, _ []string, _ []byte) (string, error) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		submitErr = ctx.Err()
		return "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Handle(ctx, []byte(sample)) }()
	<-started
	cancel()
	if err := <-done; err != nil || submitErr != nil {
		t.Errorf("job cancelled on shutdown: %v, %v", err, submitErr)
	}
}

func serve(t *testing.T, cfg config.Config) (*Bridge, *recorder, string, func()) {
	t.Helper()
	b, rec := newBridge(t, cfg)
	addr, stop := serveWith(t, b)
	return b, rec, addr, stop
}

func serveWith(t *testing.T, b *Bridge) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Serve(ctx, ln) }()
	stop := func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	return ln.Addr().String(), stop
}

// send writes data and then closes the write side, keeps the connection
// open, or resets it.
func send(t *testing.T, addr, data, how string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(data))
	switch how {
	case "close":
		c.(*net.TCPConn).CloseWrite()
		io.Copy(io.Discard, c)
		c.Close()
	case "open":
		io.Copy(io.Discard, c) // returns when the server closes
		c.Close()
	case "reset":
		time.Sleep(50 * time.Millisecond)
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
	}
}

func waitPrints(t *testing.T, rec *recorder, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-rec.called:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d jobs printed", i, n)
		}
	}
}

func TestServe(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = 300 * time.Millisecond
	_, rec, addr, stop := serve(t, cfg)

	send(t, addr, sample, "close")                            // CUPS socket backend
	send(t, addr, sample, "open")                             // printed after the idle timeout
	send(t, addr, sample, "reset")                            // printed: the label was complete
	send(t, addr, sample+"^XA^FO10,10^GB50,50,50^FS", "open") // the complete label is printed
	send(t, addr, "^XA^FO10,10^GB50,50,50^FS", "open")        // nothing complete: dropped
	waitPrints(t, rec, 4)
	stop()
	if n := rec.count(); n != 4 {
		t.Errorf("got %d submissions, want 4", n)
	}
	last := rec.subs[3].data
	if bytes.Count(last, []byte("PRINT 1,")) != 1 {
		t.Error("the incomplete second label was printed")
	}
}

// A job over the limit is rejected even when its first label is complete.
func TestTooLargeJob(t *testing.T) {
	cfg := printerConfig()
	cfg.MaxJobBytes = int64(len(sample)) + 10
	_, rec, addr, stop := serve(t, cfg)
	send(t, addr, sample+sample, "close")
	time.Sleep(200 * time.Millisecond)
	stop()
	if rec.count() != 0 {
		t.Error("oversized job printed")
	}
}

func TestRenderFailureNotifies(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	if err := b.Handle(context.Background(), []byte("^XA^GF^XZ")); err == nil {
		t.Fatal("renderer failure not returned")
	}
	b.Wait()
	if n := rec.notifications(); len(n) != 1 || !strings.Contains(n[0], "failed") {
		t.Errorf("notifications = %q", n)
	}
}

func TestCopiesAreCountedAndWarned(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	if err := b.Handle(context.Background(), []byte("^XA^PW1200^FO10,10^GB50,50,50^FS^PQ3^XZ")); err != nil {
		t.Fatal(err)
	}
	b.Wait()
	if !bytes.Contains(rec.subs[0].data, []byte("PRINT 1,3\r\n")) {
		t.Error("TSPL job does not print 3 copies")
	}
	notes := strings.Join(rec.notifications(), " | ")
	if !strings.Contains(notes, "3 label(s) queued") || !strings.Contains(notes, "^PW") {
		t.Errorf("notifications = %q", notes)
	}
}

// A flood of bad jobs shows a bounded number of notifications.
func TestNotificationQueueIsBounded(t *testing.T) {
	b, _ := newBridge(t, printerConfig())
	release := make(chan struct{})
	var shown atomic.Int32
	b.Notify = func(string, string) { <-release; shown.Add(1) }
	for i := 0; i < 50; i++ {
		b.Handle(context.Background(), []byte("not zpl"))
	}
	close(release)
	b.Wait()
	if n := shown.Load(); n > notifyQueueLen+1 {
		t.Errorf("%d notifications shown, want at most %d", n, notifyQueueLen+1)
	}
}

// An HTTP request is refused as soon as its first bytes arrive, even if the
// browser keeps the connection open.
func TestHTTPRefusedAtOnce(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = 10 * time.Second
	_, rec, addr, stop := serve(t, cfg)
	defer stop()
	start := time.Now()
	send(t, addr, "\r\nGET /print HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", "open")
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("HTTP connection held for %s", d)
	}
	if rec.count() != 0 {
		t.Error("HTTP request printed")
	}
}

// A sender that keeps sending within read_timeout is not cut off.
func TestIdleTimeoutIsRenewed(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = 300 * time.Millisecond
	_, rec, addr, stop := serve(t, cfg)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{sample[:20], sample[20:40], sample[40:]} {
		c.Write([]byte(part))
		time.Sleep(200 * time.Millisecond)
	}
	c.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, c)
	c.Close()
	waitPrints(t, rec, 1)
	stop()
}

// Jobs already sent but still in the listen backlog are printed on shutdown.
func TestShutdownDrainsBacklog(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	b.Submit = func(context.Context, []string, []byte) (string, error) {
		time.Sleep(100 * time.Millisecond)
		rec.mu.Lock()
		rec.subs = append(rec.subs, submission{})
		rec.mu.Unlock()
		return "", nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Serve(ctx, ln) }()
	const jobs = 8
	for i := 0; i < jobs; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(sample))
		c.Close()
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := rec.count(); n != jobs {
		t.Errorf("%d of %d jobs printed after shutdown", n, jobs)
	}
}

// With an archive in pdf mode, lp gets one complete PDF.
func TestPDFWithArchiveSendsOneDocument(t *testing.T) {
	cfg := printerConfig()
	cfg.Mode = "pdf"
	cfg.ArchiveDir = t.TempDir()
	b, rec := newBridge(t, cfg)
	if err := b.Handle(context.Background(), []byte(sample)); err != nil {
		t.Fatal(err)
	}
	payload := rec.subs[0].data
	if n := bytes.Count(payload, []byte("%%EOF")); n != 1 {
		t.Errorf("lp payload has %d %%%%EOF markers, want 1", n)
	}
	archived, _ := os.ReadFile(filepath.Join(cfg.ArchiveDir, "20260102T030405.000Z.pdf"))
	if !bytes.Equal(archived, payload) {
		t.Error("archived PDF differs from the printed one")
	}
}

// zebrash reads ^xa and ^xz in any case, so the bridge must too.
func TestLowercaseLabelStartAndEnd(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	job := strings.NewReplacer("^XA", "^xa", "^XZ", "^xz").Replace(sample)
	if err := b.Handle(context.Background(), []byte(job)); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 {
		t.Error("lowercase job not printed")
	}
}

// Job text in a notification is cut and its markup escaped.
func TestNotificationIsEscaped(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	rec.err = errors.New("<b>bold</b> & <a href='x'>link</a> " + strings.Repeat("x", 1000))
	b.Handle(context.Background(), []byte(sample))
	b.Wait()
	n := rec.notifications()
	if len(n) != 1 || strings.ContainsAny(n[0], "<>") || !strings.Contains(n[0], "&lt;b&gt;") || len(n[0]) > 300 {
		t.Errorf("notification = %q", n)
	}
}

// Labels after the last complete one are reported to the user.
func TestCutShortJobNotifies(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = 200 * time.Millisecond
	b, rec, addr, stop := serve(t, cfg)
	send(t, addr, sample+"^XA^FO10,10^GB50,50,50^FS", "reset")
	waitPrints(t, rec, 1)
	stop()
	b.Wait()
	if notes := strings.Join(rec.notifications(), " | "); !strings.Contains(notes, "cut short") {
		t.Errorf("notifications = %q", notes)
	}
}

// One address can't hold more than its share of the connections.
func TestPerAddressLimit(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = 2 * time.Second
	b, rec := newBridge(t, cfg)
	b.perIP = 1
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Serve(ctx, ln) }()

	hold, err := net.Dial("tcp", ln.Addr().String()) // sends nothing yet
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	send(t, ln.Addr().String(), sample, "close") // refused at once
	if time.Since(start) > time.Second || rec.count() != 0 {
		t.Errorf("second connection from the same address was served")
	}
	hold.Write([]byte(sample))
	hold.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, hold)
	hold.Close()
	waitPrints(t, rec, 1)
	cancel()
	<-done
}

// A sender that keeps trickling data is cut off after the total time.
func TestTotalConnectionTime(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = 300 * time.Millisecond
	b, rec := newBridge(t, cfg)
	b.connTime = 500 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- b.Serve(ctx, ln) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	closed := make(chan struct{})
	go func() { io.Copy(io.Discard, c); close(closed) }()
	for i := 0; i < 30; i++ {
		select {
		case <-closed:
			i = 30
		default:
			c.Write([]byte("^"))
			time.Sleep(100 * time.Millisecond)
		}
	}
	<-closed
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("trickling connection lasted %s", d)
	}
	c.Close()
	cancel()
	<-done
	if rec.count() != 0 {
		t.Error("incomplete trickled job printed")
	}
}

func TestLpRunsInOwnGroup(t *testing.T) {
	cmd := lpCommand(context.Background(), []string{"-d", "x"}, nil)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("lp shares the service's process group")
	}
}

func TestPerAddressLimitFollowsListen(t *testing.T) {
	cfg := printerConfig()
	b, _ := newBridge(t, cfg)
	cfg.Listen = "0.0.0.0:9100"
	n, _ := newBridge(t, cfg)
	if b.perIP != 0 || n.perIP != maxConnsPerIP {
		t.Errorf("perIP = %d on loopback and %d on the network", b.perIP, n.perIP)
	}
}

// A job that ends with an unfinished label after a normal close is reported.
func TestUnfinishedLabelAtCloseNotifies(t *testing.T) {
	b, rec, addr, stop := serve(t, printerConfig())
	send(t, addr, sample+"^XA^FO10,10^GB50,50,50^FS", "close")
	waitPrints(t, rec, 1)
	stop()
	b.Wait()
	if notes := strings.Join(rec.notifications(), " | "); !strings.Contains(notes, "cut short") {
		t.Errorf("notifications = %q", notes)
	}
}

// Logged errors are cut, so a huge job can't flood the log.
func TestLoggedErrorIsCut(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	var logBuf bytes.Buffer
	b.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	rec.err = errors.New(strings.Repeat("x", 100000))
	addr, stop := serveWith(t, b)
	send(t, addr, sample, "close")
	waitPrints(t, rec, 1)
	stop()
	if logBuf.Len() > 2000 {
		t.Errorf("log is %d bytes for one failed job", logBuf.Len())
	}
}

// Stopping the service ends open reads at once and prints their complete labels.
func TestStopEndsOpenReads(t *testing.T) {
	cfg := printerConfig()
	cfg.ReadTimeout = time.Minute
	_, rec, addr, stop := serve(t, cfg)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(sample)) // the connection stays open
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	stop()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("stop took %s", d)
	}
	if rec.count() != 1 {
		t.Error("the complete label was not printed on stop")
	}
}

func TestInvalidUTF8Notification(t *testing.T) {
	b, rec := newBridge(t, printerConfig())
	rec.err = errors.New("bad \xff\xfe bytes")
	b.Handle(context.Background(), []byte(sample))
	b.Wait()
	for _, n := range rec.notifications() {
		if !utf8.ValidString(n) {
			t.Errorf("notification is not valid UTF-8: %q", n)
		}
	}
}
