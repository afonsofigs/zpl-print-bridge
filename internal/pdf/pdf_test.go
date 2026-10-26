package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"testing"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

func label(copies int) *render.Label {
	l := &render.Label{Width: 812, Height: 1218, Copies: copies, Pix: bytes.Repeat([]byte{0xFF}, 102*1218)}
	l.Pix[0] = 0x7F            // top-left dot black
	l.Pix[len(l.Pix)-1] = 0xFE // last dot of the padded bottom row black
	return l
}

func TestDocument(t *testing.T) {
	d := New(render.Size{WidthMm: 101.6, HeightMm: 152.4, DPI: 203})
	l := label(2)
	d.Add(l)
	d.Add(label(1))
	doc := d.Bytes()

	if !bytes.HasPrefix(doc, []byte("%PDF-1.4\n")) || !bytes.HasSuffix(doc, []byte("%%EOF\n")) {
		t.Fatal("missing PDF header or trailer")
	}
	if n := bytes.Count(doc, []byte("/Type /Page ")); n != 3 {
		t.Errorf("got %d pages, want 3 (2 copies + 1)", n)
	}
	if !bytes.Contains(doc, []byte("/Count 3")) {
		t.Error("page tree count is not 3")
	}

	// The image fills the page exactly, with no colour inversion.
	if !bytes.Contains(doc, []byte("/MediaBox [0 0 288.000 432.000]")) {
		t.Error("page is not 4x6 in (288x432 pt)")
	}
	if !bytes.Contains(doc, []byte("q 288.000 0 0 432.000 0 0 cm /Im0 Do Q")) {
		t.Error("image is not drawn over the whole page")
	}
	if bytes.Contains(doc, []byte("/Decode")) {
		t.Error("image has a /Decode array")
	}

	// The image data is the label bitmap (1 = white in DeviceGray).
	m := regexp.MustCompile(`(?s)/Width 812 /Height 1218 .*?/Length (\d+) >>\nstream\n`).FindSubmatchIndex(doc)
	if m == nil {
		t.Fatal("812x1218 image not found")
	}
	n, _ := strconv.Atoi(string(doc[m[2]:m[3]]))
	zr, err := zlib.NewReader(bytes.NewReader(doc[m[1] : m[1]+n]))
	if err != nil {
		t.Fatal(err)
	}
	pix, _ := io.ReadAll(zr)
	if !bytes.Equal(pix, l.Pix) {
		t.Error("image data differs from the label bitmap")
	}

	checkXref(t, doc)
}

// The page is the bitmap size at the printer resolution, also when the label
// size in mm is not a whole number of dots.
func TestPageMatchesDots(t *testing.T) {
	size := render.Size{WidthMm: 100, HeightMm: 150, DPI: 203}
	w, h := size.Dots() // 799 x 1199
	d := New(size)
	d.Add(&render.Label{Width: w, Height: h, Copies: 1, Pix: bytes.Repeat([]byte{0xFF}, (w+7)/8*h)})
	want := fmt.Sprintf("/MediaBox [0 0 %.3f %.3f]", float64(w)/203*72, float64(h)/203*72)
	if !bytes.Contains(d.Bytes(), []byte(want)) {
		t.Errorf("page size is not %s", want)
	}
}

// Every xref offset must point at the start of its object.
func checkXref(t *testing.T, doc []byte) {
	m := regexp.MustCompile(`startxref\n(\d+)`).FindSubmatch(doc)
	if m == nil {
		t.Fatal("no startxref")
	}
	xref, _ := strconv.Atoi(string(m[1]))
	lines := bytes.Split(doc[xref:], []byte("\n"))
	count := 0
	for i, line := range lines[3:] {
		if !bytes.HasSuffix(line, []byte(" n ")) {
			break
		}
		off, _ := strconv.Atoi(string(line[:10]))
		if !bytes.HasPrefix(doc[off:], []byte(strconv.Itoa(i+1)+" 0 obj")) {
			t.Errorf("xref entry %d points at %q", i+1, doc[off:off+10])
		}
		count++
	}
	if count < 5 {
		t.Errorf("only %d xref entries", count)
	}
}

func TestBytesIsStable(t *testing.T) {
	d := New(render.Size{WidthMm: 101.6, HeightMm: 152.4, DPI: 203})
	d.Add(label(1))
	first := append([]byte(nil), d.Bytes()...)
	if second := d.Bytes(); !bytes.Equal(first, second) || bytes.Count(second, []byte("%%EOF")) != 1 {
		t.Error("a second Bytes call changed the document")
	}
}
