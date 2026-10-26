package tspl

import (
	"bytes"
	"testing"

	"github.com/afonsofigs/zpl-print-bridge/internal/render"
)

func TestJob(t *testing.T) {
	// 10 x 2 dots, first dot black: rows of 2 bytes, 1 = white.
	l := &render.Label{Width: 10, Height: 2, Pix: []byte{0x7F, 0xFF, 0xFF, 0xFF}, Copies: 1}
	l3 := &render.Label{Width: 10, Height: 2, Pix: []byte{0xFF, 0xFF, 0xFF, 0xBF}, Copies: 3}
	j := NewJob(Options{WidthMm: 101.6, HeightMm: 152.4, GapMm: 3, Speed: 4, Density: -1, Direction: 1})
	j.Add(l)
	j.Add(l3)

	want := "SIZE 102 mm,153 mm\r\nREFERENCE 0,0\r\nDIRECTION 1,0\r\nGAP 3.0 mm,0.0 mm\r\nOFFSET 0 mm\r\nSPEED 4\r\n" +
		"CLS\r\nBITMAP 0,0,2,2,1,\x7F\xFF\xFF\xFF\r\nPRINT 1,1\r\n" +
		"CLS\r\nBITMAP 0,0,2,2,1,\xFF\xFF\xFF\xBF\r\nPRINT 1,3\r\n"
	if got := string(j.Bytes()); got != want {
		t.Fatalf("job =\n%q\nwant\n%q", got, want)
	}
}

func TestOptionalSettings(t *testing.T) {
	j := NewJob(Options{WidthMm: 50, HeightMm: 30, GapMm: 2, Speed: 0, Density: 8, Direction: 0})
	got := j.Bytes()
	if bytes.Contains(got, []byte("SPEED")) {
		t.Error("SPEED sent with Speed 0")
	}
	if !bytes.Contains(got, []byte("DENSITY 8\r\n")) {
		t.Error("DENSITY 8 missing")
	}
	if !bytes.HasPrefix(got, []byte("SIZE 50 mm,30 mm\r\nREFERENCE 0,0\r\nDIRECTION 0,0\r\n")) {
		t.Errorf("unexpected header: %q", got)
	}
}
