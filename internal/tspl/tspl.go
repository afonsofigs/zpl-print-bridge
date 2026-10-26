// Package tspl encodes label bitmaps as TSPL, the language of TSC printers and
// of many low-cost 4x6 thermal printers.
package tspl

import (
	"bytes"
	"fmt"
	"math"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

// Options holds the print settings sent in the job header.
type Options struct {
	WidthMm, HeightMm float64
	GapMm             float64
	Speed             int // 0 omits SPEED
	Density           int // -1 omits DENSITY (printer default)
	Direction         int // 0 or 1
}

// Job builds one TSPL job, one label at a time.
//
// The header and BITMAP mode match rastertolabeltspl, the CUPS filter in
// Phomemo's Linux driver (LabelPrinter 1.2.1). SIZE is in whole mm, rounded
// up. BITMAP uses mode 1 on a cleared buffer. A PM-246S-BT prints this form.
type Job struct{ buf bytes.Buffer }

// NewJob writes the job header.
func NewJob(o Options) *Job {
	j := &Job{}
	b := &j.buf
	fmt.Fprintf(b, "SIZE %d mm,%d mm\r\n", ceil(o.WidthMm), ceil(o.HeightMm))
	b.WriteString("REFERENCE 0,0\r\n")
	fmt.Fprintf(b, "DIRECTION %d,0\r\n", o.Direction)
	fmt.Fprintf(b, "GAP %.1f mm,0.0 mm\r\n", o.GapMm)
	b.WriteString("OFFSET 0 mm\r\n")
	if o.Speed > 0 {
		fmt.Fprintf(b, "SPEED %d\r\n", o.Speed)
	}
	if o.Density >= 0 {
		fmt.Fprintf(b, "DENSITY %d\r\n", o.Density)
	}
	return j
}

// Add appends one label, printed l.Copies times.
func (j *Job) Add(l *render.Label) {
	copies := max(l.Copies, 1)
	b := &j.buf
	b.WriteString("CLS\r\n")
	fmt.Fprintf(b, "BITMAP 0,0,%d,%d,1,", l.RowBytes(), l.Height)
	b.Write(l.Pix)
	fmt.Fprintf(b, "\r\nPRINT 1,%d\r\n", copies)
}

// Bytes returns the job.
func (j *Job) Bytes() []byte { return j.buf.Bytes() }

func ceil(v float64) int { return int(math.Ceil(v - 1e-9)) }
