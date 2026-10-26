// Package render turns ZPL into 1-bit label bitmaps at the printer resolution.
package render

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"

	"github.com/ingridhq/zebrash"
	"github.com/ingridhq/zebrash/drawers"
	"github.com/ingridhq/zebrash/elements"
)

// DotsPerMm maps the supported printer resolutions to the dots per mm that
// zebrash draws with.
var DotsPerMm = map[int]int{152: 6, 203: 8, 300: 12, 600: 24}

// Label is a 1-bit bitmap. Pix holds the rows packed MSB first and padded to
// whole bytes, with 1 for white and 0 for black. TSPL BITMAP and 1-bit PDF
// DeviceGray both use this layout.
type Label struct {
	Width, Height int
	Pix           []byte
	Copies        int // from ^PQ, at least 1
}

// RowBytes is the length of one packed row.
func (l *Label) RowBytes() int { return (l.Width + 7) / 8 }

// At reports whether the dot at (x, y) is black.
func (l *Label) At(x, y int) bool {
	return l.Pix[y*l.RowBytes()+x/8]&(0x80>>(x%8)) == 0
}

// Size describes the physical label and the printer resolution.
type Size struct {
	WidthMm, HeightMm float64
	DPI               int
}

// Dots returns the label size in printer dots. Content that zebrash draws
// outside this area is off the physical label and is cut.
func (s Size) Dots() (int, int) {
	return int(math.Round(s.WidthMm / 25.4 * float64(s.DPI))),
		int(math.Round(s.HeightMm / 25.4 * float64(s.DPI)))
}

// Warning describes a label that renders but may not print as intended.
type Warning struct {
	Label int // 1-based
	Text  string
}

// ErrTooManyLabels is returned when a job has more labels, copies included,
// than the limit given to Render.
var ErrTooManyLabels = errors.New("too many labels in job")

// Render parses zpl and calls each for every label (^XA...^XZ), in order.
// Labels are drawn one at a time. ZPL positions are printer dots, so the DPI
// must match the label design.
//
// Some malformed ZPL makes zebrash allocate without limit, which a Go program
// can't recover from. Callers that handle untrusted input use the worker
// package, which runs Render in a child process with a memory limit.
func Render(zpl []byte, size Size, maxLabels int, each func(*Label) error) (warnings []Warning, err error) {
	dpmm, ok := DotsPerMm[size.DPI]
	if !ok || size.WidthMm <= 0 || size.HeightMm <= 0 {
		return nil, fmt.Errorf("invalid label size %+v", size)
	}
	// zebrash panics on some malformed input. A bad job must not stop the caller.
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ZPL renderer failed: %v", r)
		}
	}()

	// Each ^XA...^XZ block goes to the same parser in turn. Stored formats and
	// graphics carry over, and each label gets the ^PQ of its own block.
	type job struct {
		info   elements.LabelInfo
		copies int
	}
	var jobs []job
	parser := zebrash.NewParser()
	total := 0
	var warn warnList
	defer func() { warnings = warn.result() }()
	blks, err := blocks(zpl, 2*maxLabels+20)
	if err != nil {
		return nil, err
	}
	pqIgnored := false
	for _, block := range blks {
		infos, err := parser.Parse(block)
		if err != nil {
			return nil, fmt.Errorf("parse ZPL: %w", err)
		}
		q := copiesOf(block, maxLabels)
		if len(infos) != 1 && q > 1 {
			if !pqIgnored {
				warn.add(Warning{0, "^PQ ignored in a block that is not one label"})
				pqIgnored = true
			}
			q = 1
		}
		for _, info := range infos {
			total += q // q is at most maxLabels+1, so this can't overflow
			if total > maxLabels {
				return nil, fmt.Errorf("%w: more than %d", ErrTooManyLabels, maxLabels)
			}
			jobs = append(jobs, job{info, q})
		}
	}
	if len(jobs) == 0 {
		return nil, errors.New("no labels in ZPL data")
	}

	// zebrash sizes its canvas from millimetres, rounding up. Giving it the
	// exact dot size keeps ^POI labels from shifting by the rounding.
	w, h := size.Dots()
	opts := drawers.DrawerOptions{
		LabelWidthMm:         float64(w) / float64(dpmm),
		LabelHeightMm:        float64(h) / float64(dpmm),
		Dpmm:                 dpmm,
		EnableInvertedLabels: true,
	}
	drawer := zebrash.NewDrawer()
	for i, j := range jobs {
		info := j.info
		if info.PrintWidth > w {
			warn.add(Warning{i + 1, fmt.Sprintf("^PW %d is wider than the label (%d dots); the right side is cut", info.PrintWidth, w)})
		}
		var buf bytes.Buffer
		if err := drawer.DrawLabelAsPng(info, &buf, opts); err != nil {
			return nil, fmt.Errorf("draw label %d: %w", i+1, err)
		}
		img, err := png.Decode(&buf)
		if err != nil {
			return nil, fmt.Errorf("decode label %d: %w", i+1, err)
		}
		l, blank := toLabel(img, w, h)
		if blank {
			warn.add(Warning{i + 1, "label is blank"})
		}
		l.Copies = j.copies
		if err := each(l); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// maxWarnings is the number of warnings kept per job; the rest are counted.
const maxWarnings = 10

type warnList struct {
	list  []Warning
	extra int
}

func (w *warnList) add(x Warning) {
	if len(w.list) < maxWarnings {
		w.list = append(w.list, x)
	} else {
		w.extra++
	}
}

func (w *warnList) result() []Warning {
	if w.extra > 0 {
		return append(w.list, Warning{0, fmt.Sprintf("%d more warnings", w.extra)})
	}
	return w.list
}

// toLabel thresholds img into a packed w x h bitmap.
func toLabel(img image.Image, w, h int) (*Label, bool) {
	l := &Label{Width: w, Height: h}
	rb := l.RowBytes()
	l.Pix = bytes.Repeat([]byte{0xFF}, rb*h)
	gray, isGray := img.(*image.Gray)
	b := img.Bounds()
	blank := true
	for y := 0; y < h && b.Min.Y+y < b.Max.Y; y++ {
		for x := 0; x < w && b.Min.X+x < b.Max.X; x++ {
			var v uint8
			if isGray {
				v = gray.GrayAt(b.Min.X+x, b.Min.Y+y).Y
			} else {
				r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				v = uint8((19595*r + 38470*g + 7471*bl + 1<<15) >> 24)
			}
			if v < 128 {
				l.Pix[y*rb+x/8] &^= 0x80 >> (x % 8)
				blank = false
			}
		}
	}
	return l, blank
}

// IndexCommand returns the index of the first ^<cmd> in data at or after
// from, ignoring case, or -1. cmd is two upper-case letters.
func IndexCommand(data []byte, cmd string, from int) int {
	for i := from; i+2 < len(data); i++ {
		if isCommand(data, i, cmd) {
			return i
		}
	}
	return -1
}

// LastIndexCommand returns the index of the last ^<cmd> in data, ignoring
// case, or -1. It doesn't allocate, so it is safe on large untrusted jobs.
func LastIndexCommand(data []byte, cmd string) int {
	for i := len(data) - 3; i >= 0; i-- {
		if isCommand(data, i, cmd) {
			return i
		}
	}
	return -1
}

func isCommand(data []byte, i int, cmd string) bool {
	return data[i] == '^' && data[i+1]&^0x20 == cmd[0] && data[i+2]&^0x20 == cmd[1]
}

// blocks splits zpl after each ^XZ. Text after the last ^XZ is a block too.
// A job with more than limit blocks is rejected.
func blocks(zpl []byte, limit int) ([][]byte, error) {
	var out [][]byte
	prev := 0
	for i := IndexCommand(zpl, "XZ", 0); i >= 0; i = IndexCommand(zpl, "XZ", prev) {
		out = append(out, zpl[prev:i+3])
		prev = i + 3
		if len(out) > limit {
			return nil, fmt.Errorf("%w: more than %d ^XZ blocks", ErrTooManyLabels, limit)
		}
	}
	if prev < len(zpl) {
		out = append(out, zpl[prev:])
	}
	return out, nil
}

// copiesOf returns the copy count of the last ^PQ in block, at most limit+1.
// Line breaks and tabs inside the number are skipped, as zebrash does.
func copiesOf(block []byte, limit int) int {
	i := LastIndexCommand(block, "PQ")
	if i < 0 {
		return 1
	}
	n, digits := 0, 0
	for _, c := range block[i+3:] {
		if c == '\r' || c == '\n' || c == '\t' {
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		digits++
		if n = n*10 + int(c-'0'); n > limit {
			return limit + 1
		}
	}
	if digits == 0 || n < 1 {
		return 1
	}
	return n
}
