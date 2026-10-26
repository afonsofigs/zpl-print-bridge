package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	c, err := Load(path, false)
	if err != nil || c.Listen != "127.0.0.1:9100" {
		t.Errorf("optional missing file: %v, %+v", err, c)
	}
	if _, err := Load(path, true); !errors.Is(err, ErrInvalid) {
		t.Errorf("required missing file: err = %v", err)
	}
}

func TestParse(t *testing.T) {
	c, err := Load(write(t, `
# comment
listen = 127.0.0.1:9200
printer = "Label-Printer"   # the label printer
mode = pdf
tspl_density =
read_timeout = 5s
archive_dir = ~/labels
lp_options = "media=w288h432 #2"
`), true)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	switch {
	case c.Listen != "127.0.0.1:9200", c.Printer != "Label-Printer", c.Mode != "pdf":
		t.Errorf("basic keys not read: %+v", c)
	case len(c.LpOptions) != 2 || c.LpOptions[1] != "#2":
		t.Errorf("quoted value with # = %q", c.LpOptions)
	case c.Density != -1:
		t.Errorf("empty tspl_density = %d, want -1", c.Density)
	case c.ReadTimeout != 5*time.Second:
		t.Errorf("read_timeout = %s", c.ReadTimeout)
	case c.ArchiveDir != filepath.Join(home, "labels"):
		t.Errorf("archive_dir = %q", c.ArchiveDir)
	}
}

// The example file says empty keys use the defaults; listen must never
// become an empty, all-interfaces address.
func TestEmptyValuesKeepDefaults(t *testing.T) {
	c, err := Load(write(t, "listen =\nmode =\ndpi =\ntspl_speed =\nread_timeout =\nmax_labels =\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	d := Default()
	if c.Listen != d.Listen || c.Mode != d.Mode || c.DPI != d.DPI || c.Speed != d.Speed ||
		c.ReadTimeout != d.ReadTimeout || c.MaxLabels != d.MaxLabels {
		t.Errorf("empty values changed defaults: %+v", c)
	}
}

func TestErrors(t *testing.T) {
	for _, text := range []string{
		"no equals sign",
		"unknown = 1",
		"dpi = abc",
		"dpi = 250",
		"mode = escpos",
		"tspl_density = 16",
		"tspl_gap_mm = -3",
		"tspl_speed = -1",
		"label_width_mm = 2000",
		"dpi = 600\nlabel_width_mm = 300\nlabel_height_mm = 600",
		"max_job_bytes = 20000000",
		"max_labels = 1000",
		"dpi = 600\nmax_labels = 500",
		"read_timeout = 6m",
		"read_timeout = 1ns",
		"max_labels = 0",
		"listen = 9100",
		"listen = lclhost:9100",
		"printer = \"a\" junk",
		"printer = a\nprinter = b",
	} {
		if _, err := Load(write(t, text), true); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", text, err)
		}
	}
}

func TestRequireOutput(t *testing.T) {
	c := Default()
	if err := c.RequireOutput(); err == nil {
		t.Errorf("no printer and no archive: err = %v", err)
	}
	c.Printer = "x"
	if err := c.RequireOutput(); err != nil {
		t.Error(err)
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9100": true, "[::1]:9100": true, "localhost:9100": true,
		"0.0.0.0:9100": false, ":9100": false, "192.168.1.5:9100": false,
	} {
		c := Default()
		c.Listen = addr
		if got := c.Loopback(); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestCommentAtStartOfValue(t *testing.T) {
	c, err := Load(write(t, "printer = # none yet\nlp_options = # none\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Printer != "" || len(c.LpOptions) != 0 {
		t.Errorf("comment taken as value: %q, %q", c.Printer, c.LpOptions)
	}
}

// The memory budget counts each label twice (TSPL job and PDF), and the error
// names the real limit.
func TestMemoryBudget(t *testing.T) {
	limit := maxJobMemory / (2 * ((812 + 7) / 8) * 1624) // 4x8 in at 203 dpi
	base := "label_height_mm = 203.2\nmax_labels = %d\n"
	if _, err := Load(write(t, fmt.Sprintf(base, limit)), true); err != nil {
		t.Errorf("%d labels at 4x8 in rejected: %v", limit, err)
	}
	_, err := Load(write(t, fmt.Sprintf(base, limit+1)), true)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", limit)) {
		t.Errorf("%d labels at 4x8 in: err = %v, want at most %d", limit+1, err, limit)
	}
}
