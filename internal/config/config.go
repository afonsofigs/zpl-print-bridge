// Package config reads the "key = value" configuration file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
	"github.com/afonsofigs/zpl-print-bridge/internal/tspl"
)

// Config holds every setting. Load starts from Default().
type Config struct {
	Listen      string        // TCP address that accepts ZPL jobs
	Printer     string        // CUPS queue; empty means archive only
	Mode        string        // "tspl" or "pdf"
	LpOptions   []string      // extra "-o" options for lp
	DPI         int           // printer resolution, must match the ZPL design
	WidthMm     float64       // label width
	HeightMm    float64       // label height
	GapMm       float64       // TSPL gap between labels
	Speed       int           // TSPL speed, 0 omits it
	Density     int           // TSPL density 0-15, -1 omits it
	Direction   int           // TSPL direction, 0 or 1
	ArchiveDir  string        // keeps the .zpl and .pdf of each job; empty disables
	Notify      bool          // desktop notification per job (notify-send)
	MaxJobBytes int64         // larger jobs are rejected
	MaxLabels   int           // jobs with more labels, copies included, are rejected
	ReadTimeout time.Duration // a job ends when no data arrives for this long
}

// Default returns the settings for a 4x6 in label on a 203 dpi printer.
func Default() Config {
	return Config{
		Listen:      "127.0.0.1:9100",
		Mode:        "tspl",
		DPI:         203,
		WidthMm:     101.6,
		HeightMm:    152.4,
		GapMm:       3,
		Speed:       4,
		Density:     -1,
		Direction:   1,
		MaxJobBytes: 16 << 20,
		MaxLabels:   50,
		ReadTimeout: 30 * time.Second,
	}
}

// ErrInvalid marks configuration errors.
var ErrInvalid = errors.New("invalid configuration")

// Load reads path on top of the defaults. If required is false, a missing
// file gives the defaults; otherwise it is an error.
func Load(path string, required bool) (Config, error) {
	c := Default()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	defer f.Close()
	if err := c.parse(f); err != nil {
		return c, fmt.Errorf("%w: %s: %v", ErrInvalid, path, err)
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%w: %s: %v", ErrInvalid, path, err)
	}
	return c, nil
}

func (c *Config) parse(r io.Reader) error {
	seen := map[string]int{}
	s := bufio.NewScanner(r)
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("line %d: expected key = value", n)
		}
		k = strings.TrimSpace(k)
		if prev, dup := seen[k]; dup {
			return fmt.Errorf("line %d: %s already set on line %d", n, k, prev)
		}
		seen[k] = n
		val, err := value(strings.TrimSpace(v))
		if err == nil {
			err = c.Set(k, val)
		}
		if err != nil {
			return fmt.Errorf("line %d: %w", n, err)
		}
	}
	return s.Err()
}

// value returns the text of a setting. A value in single or double quotes is
// taken as is; only a comment may follow it. Otherwise a # at the start or
// after a space starts a comment.
func value(v string) (string, error) {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			if rest := strings.TrimSpace(v[end+2:]); rest != "" && rest[0] != '#' {
				return "", fmt.Errorf("text after the closing quote: %q", rest)
			}
			return v[1 : end+1], nil
		}
	}
	for i := 0; i < len(v); i++ {
		if v[i] == '#' && (i == 0 || v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i]), nil
		}
	}
	return v, nil
}

// Set assigns one setting from its text form. An empty value keeps the default.
func (c *Config) Set(key, val string) error {
	d := Default()
	var err error
	switch key {
	case "printer":
		c.Printer = val
	case "archive_dir":
		c.ArchiveDir = expandHome(val)
	case "lp_options":
		c.LpOptions = strings.Fields(val)
	case "tspl_density":
		c.Density = d.Density
		if val != "" {
			c.Density, err = strconv.Atoi(val)
		}
	case "listen", "mode", "dpi", "label_width_mm", "label_height_mm", "tspl_gap_mm",
		"tspl_speed", "tspl_direction", "notify", "max_job_bytes", "max_labels", "read_timeout":
		if val == "" {
			return c.reset(key, d)
		}
		err = c.setValue(key, val)
	default:
		return fmt.Errorf("unknown setting %q", key)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func (c *Config) setValue(key, val string) error {
	var err error
	switch key {
	case "listen":
		c.Listen = val
	case "mode":
		c.Mode = val
	case "dpi":
		c.DPI, err = strconv.Atoi(val)
	case "label_width_mm":
		c.WidthMm, err = strconv.ParseFloat(val, 64)
	case "label_height_mm":
		c.HeightMm, err = strconv.ParseFloat(val, 64)
	case "tspl_gap_mm":
		c.GapMm, err = strconv.ParseFloat(val, 64)
	case "tspl_speed":
		c.Speed, err = strconv.Atoi(val)
	case "tspl_direction":
		c.Direction, err = strconv.Atoi(val)
	case "notify":
		c.Notify, err = strconv.ParseBool(val)
	case "max_job_bytes":
		c.MaxJobBytes, err = strconv.ParseInt(val, 10, 64)
	case "max_labels":
		c.MaxLabels, err = strconv.Atoi(val)
	case "read_timeout":
		c.ReadTimeout, err = time.ParseDuration(val)
	}
	return err
}

func (c *Config) reset(key string, d Config) error {
	switch key {
	case "listen":
		c.Listen = d.Listen
	case "mode":
		c.Mode = d.Mode
	case "dpi":
		c.DPI = d.DPI
	case "label_width_mm":
		c.WidthMm = d.WidthMm
	case "label_height_mm":
		c.HeightMm = d.HeightMm
	case "tspl_gap_mm":
		c.GapMm = d.GapMm
	case "tspl_speed":
		c.Speed = d.Speed
	case "tspl_direction":
		c.Direction = d.Direction
	case "notify":
		c.Notify = d.Notify
	case "max_job_bytes":
		c.MaxJobBytes = d.MaxJobBytes
	case "max_labels":
		c.MaxLabels = d.MaxLabels
	case "read_timeout":
		c.ReadTimeout = d.ReadTimeout
	}
	return nil
}

// Validate rejects settings that cannot work.
func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return fmt.Errorf("listen: %q is not an IP address or localhost", host)
	}
	if _, ok := render.DotsPerMm[c.DPI]; !ok {
		var dpis []int
		for d := range render.DotsPerMm {
			dpis = append(dpis, d)
		}
		sort.Ints(dpis)
		return fmt.Errorf("dpi must be one of %v", dpis)
	}
	switch {
	case c.Mode != "tspl" && c.Mode != "pdf":
		return fmt.Errorf("mode must be tspl or pdf, got %q", c.Mode)
	case c.WidthMm < 10 || c.WidthMm > 300 || c.HeightMm < 10 || c.HeightMm > 600:
		return errors.New("label_width_mm must be 10-300 and label_height_mm 10-600")
	case dotArea(c) > maxDots:
		return fmt.Errorf("label too large at %d dpi: at most %d million dots", c.DPI, maxDots/1_000_000)
	case jobBytes(c) > maxJobMemory:
		return fmt.Errorf("max_labels too high for this label size: at most %d", maxJobMemory/(2*labelBytes(c)))
	case c.GapMm < 0 || c.GapMm > 30:
		return errors.New("tspl_gap_mm must be 0-30")
	case c.Speed < 0 || c.Speed > 15:
		return errors.New("tspl_speed must be 0-15")
	case c.Density < -1 || c.Density > 15:
		return errors.New("tspl_density must be 0-15 or empty")
	case c.Direction != 0 && c.Direction != 1:
		return errors.New("tspl_direction must be 0 or 1")
	case c.MaxJobBytes < 1 || c.MaxJobBytes > 16<<20:
		return errors.New("max_job_bytes must be 1-16777216")
	case c.MaxLabels < 1 || c.MaxLabels > 500:
		return errors.New("max_labels must be 1-500")
	case c.ReadTimeout < 100*time.Millisecond || c.ReadTimeout > 5*time.Minute:
		return errors.New("read_timeout must be 100ms-5m")
	}
	return nil
}

// Memory budget. maxDots keeps one label within the worker memory limit: the
// renderer holds a 4-byte canvas plus a PNG of it. maxJobMemory caps the
// bitmaps of one job in the service process: the TSPL job and the PDF.
const (
	maxDots      = 25_000_000
	maxJobMemory = 128 << 20
)

func dotArea(c Config) int {
	w, h := c.Size().Dots()
	return w * h
}

func labelBytes(c Config) int {
	w, h := c.Size().Dots()
	return (w + 7) / 8 * h
}

// jobBytes is the worst case: every label distinct, held in both outputs.
func jobBytes(c Config) int { return 2 * c.MaxLabels * labelBytes(c) }

// RequireOutput rejects a configuration that would drop every job.
func (c Config) RequireOutput() error {
	if c.Printer == "" && c.ArchiveDir == "" {
		return errors.New("set printer, archive_dir or both")
	}
	return nil
}

// Loopback reports whether Listen only accepts local connections.
func (c Config) Loopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// TSPLOptions returns the TSPL job settings.
func (c Config) TSPLOptions() tspl.Options {
	return tspl.Options{
		WidthMm: c.WidthMm, HeightMm: c.HeightMm, GapMm: c.GapMm,
		Speed: c.Speed, Density: c.Density, Direction: c.Direction,
	}
}

// Size returns the label size for rendering.
func (c Config) Size() render.Size {
	return render.Size{WidthMm: c.WidthMm, HeightMm: c.HeightMm, DPI: c.DPI}
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}
