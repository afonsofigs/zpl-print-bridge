// Package pdf writes label bitmaps as a PDF with one page per label. The page
// is the bitmap size at the printer resolution, so a renderer at that
// resolution maps one image dot to one printer dot.
package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

// Doc builds a PDF one label at a time. Copies of a label are separate pages.
type Doc struct {
	wPt, hPt float64
	body     bytes.Buffer
	offsets  map[int]int
	pages    []int
	next     int
	done     []byte
}

// Object numbers: 1 catalog, 2 page tree, then per label one image and a
// page and content object for each copy.
const firstFree = 3

// New starts a PDF for labels of the given size.
func New(size render.Size) *Doc {
	w, h := size.Dots()
	dpi := float64(size.DPI)
	d := &Doc{wPt: float64(w) / dpi * 72, hPt: float64(h) / dpi * 72, offsets: map[int]int{}, next: firstFree}
	d.body.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	return d
}

func (d *Doc) obj(n int, dict string, stream []byte) {
	d.offsets[n] = d.body.Len()
	fmt.Fprintf(&d.body, "%d 0 obj\n%s", n, dict)
	if stream != nil {
		d.body.WriteString("\nstream\n")
		d.body.Write(stream)
		d.body.WriteString("\nendstream")
	}
	d.body.WriteString("\nendobj\n")
}

// Add appends l.Copies pages that show the label.
func (d *Doc) Add(l *render.Label) {
	image := d.next
	d.next++
	data := deflate(l.Pix)
	d.obj(image, fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d "+
		"/ColorSpace /DeviceGray /BitsPerComponent 1 /Interpolate false "+
		"/Filter /FlateDecode /Length %d >>", l.Width, l.Height, len(data)), data)

	draw := []byte(fmt.Sprintf("q %.3f 0 0 %.3f 0 0 cm /Im0 Do Q", d.wPt, d.hPt))
	for range max(l.Copies, 1) {
		page, content := d.next, d.next+1
		d.next += 2
		d.obj(content, fmt.Sprintf("<< /Length %d >>", len(draw)), draw)
		d.obj(page, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.3f %.3f] "+
			"/Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>", d.wPt, d.hPt, image, content), nil)
		d.pages = append(d.pages, page)
	}
}

// Bytes finishes the document and returns it. Labels can't be added after it.
func (d *Doc) Bytes() []byte {
	if d.done != nil {
		return d.done
	}
	kids := ""
	for _, p := range d.pages {
		kids += fmt.Sprintf("%d 0 R ", p)
	}
	d.obj(1, "<< /Type /Catalog /Pages 2 0 R >>", nil)
	d.obj(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(d.pages)), nil)

	xref := d.body.Len()
	fmt.Fprintf(&d.body, "xref\n0 %d\n0000000000 65535 f \n", d.next)
	for n := 1; n < d.next; n++ {
		fmt.Fprintf(&d.body, "%010d 00000 n \n", d.offsets[n])
	}
	fmt.Fprintf(&d.body, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", d.next, xref)
	d.done = d.body.Bytes()
	return d.done
}

func deflate(b []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}
