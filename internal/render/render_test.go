package render

import (
	"errors"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var size4x6 = Size{WidthMm: 101.6, HeightMm: 152.4, DPI: 203}

func renderAll(t *testing.T, zpl string, size Size) ([]*Label, []Warning) {
	t.Helper()
	var labels []*Label
	warnings, err := Render([]byte(zpl), size, 1000, func(l *Label) error {
		labels = append(labels, l)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return labels, warnings
}

func TestDots(t *testing.T) {
	for _, tc := range []struct{ dpi, w, h int }{{203, 812, 1218}, {300, 1200, 1800}, {600, 2400, 3600}} {
		w, h := Size{WidthMm: 101.6, HeightMm: 152.4, DPI: tc.dpi}.Dots()
		if w != tc.w || h != tc.h {
			t.Errorf("%d dpi: Dots() = %dx%d, want %dx%d", tc.dpi, w, h, tc.w, tc.h)
		}
	}
}

// A Code 128 with ^BY3 has bars of 3, 6, 9 or 12 dots. Any other width means
// the bitmap was resampled, which makes the barcode unreadable.
func TestBarcodeModulesAreExact(t *testing.T) {
	labels, _ := renderAll(t, "^XA^FO50,50^BY3^BCN,200,N,N,N^FDZPL-PRINT-BRIDGE-0123456789^FS^XZ", size4x6)
	l := labels[0]
	var runs []int
	cur, n := l.At(0, 150), 0
	for x := 0; x < l.Width; x++ {
		if v := l.At(x, 150); v == cur {
			n++
		} else {
			if cur {
				runs = append(runs, n)
			}
			cur, n = v, 1
		}
	}
	if len(runs) < 20 {
		t.Fatalf("found %d bars, want a full barcode", len(runs))
	}
	for i, r := range runs {
		if r%3 != 0 || r > 12 {
			t.Errorf("bar %d is %d dots wide, want a multiple of 3 up to 12", i, r)
		}
	}
}

// Boxes in the four corners must reach the last row and column.
func TestEdges(t *testing.T) {
	labels, _ := renderAll(t, "^XA^FO0,0^GB10,10,10^FS^FO802,0^GB10,10,10^FS^FO0,1208^GB10,10,10^FS^FO802,1208^GB10,10,10^FS^XZ", size4x6)
	l := labels[0]
	if l.Width != 812 || l.Height != 1218 {
		t.Fatalf("label is %dx%d, want 812x1218", l.Width, l.Height)
	}
	for _, p := range [][2]int{{0, 0}, {811, 0}, {0, 1217}, {811, 1217}} {
		if !l.At(p[0], p[1]) {
			t.Errorf("corner %v is white", p)
		}
	}
	if l.At(400, 600) {
		t.Error("centre is black")
	}
}

func TestMultipleLabelsAndCopies(t *testing.T) {
	labels, _ := renderAll(t, "^XA^FO10,10^GB100,100,100^FS^PQ3^XZ^XA^FO10,10^GB50,50,50^FS^XZ", size4x6)
	if len(labels) != 2 {
		t.Fatalf("got %d labels, want 2", len(labels))
	}
	if labels[0].Copies != 3 || labels[1].Copies != 1 {
		t.Errorf("copies = %d, %d; want 3, 1", labels[0].Copies, labels[1].Copies)
	}
}

// ZebraDesigner sends a settings block before the label. ^PQ applies to the
// block that draws, and the last ^PQ in a block wins.
func TestCopiesWithSettingsBlock(t *testing.T) {
	labels, _ := renderAll(t, "^XA~TA000~JSN^LT0^MNW^MTD^PON^PMN^LH0,0^JMA^PR4,4^JUS^LRN^CI0^XZ"+
		"^XA^FO10,10^GB100,100,100^FS^PQ2^PQ3,0,1,Y^XZ", size4x6)
	if len(labels) != 1 || labels[0].Copies != 3 {
		t.Fatalf("got %d labels, copies %d; want 1 label, 3 copies", len(labels), labels[0].Copies)
	}
}

// ^PQ belongs to its own block, also with stored formats and lowercase commands.
func TestCopiesStayWithTheirBlock(t *testing.T) {
	labels, _ := renderAll(t, "^xa^FO10,10^GB100,100,100^FS^xz^XA^DFR:F.ZPL^FO1,1^FDx^FS^PQ7^XZ^XA^XFR:F.ZPL^PQ2^XZ", size4x6)
	if len(labels) != 2 || labels[0].Copies != 1 || labels[1].Copies != 2 {
		var got []int
		for _, l := range labels {
			got = append(got, l.Copies)
		}
		t.Fatalf("copies = %v, want [1 2]", got)
	}
}

// Huge ^PQ values must not overflow the label count.
func TestHugeCopyCount(t *testing.T) {
	zpl := "^XA^FO10,10^GB50,50,50^FS^PQ9223372036854775807^XZ"
	for _, job := range []string{zpl, zpl + zpl, "^XA^FO1,1^GB5,5,5^FS^PQ99999999999999999999999^XZ"} {
		_, err := Render([]byte(job), size4x6, 50, func(*Label) error { t.Fatal("label rendered"); return nil })
		if !errors.Is(err, ErrTooManyLabels) {
			t.Errorf("err = %v, want ErrTooManyLabels", err)
		}
	}
}

// ^POI turns the label 180 degrees within the exact label size.
func TestInvertedLabelEdges(t *testing.T) {
	labels, _ := renderAll(t, "^XA^POI^FO0,0^GB10,10,10^FS^XZ", size4x6)
	l := labels[0]
	black := 0
	for y := l.Height - 10; y < l.Height; y++ {
		for x := l.Width - 10; x < l.Width; x++ {
			if l.At(x, y) {
				black++
			}
		}
	}
	if black != 100 {
		t.Errorf("inverted corner box has %d black dots, want 100", black)
	}
}

func TestMaxLabelsCountsCopies(t *testing.T) {
	_, err := Render([]byte("^XA^FO10,10^GB10,10,10^FS^PQ5^XZ"), size4x6, 4, func(*Label) error { return nil })
	if !errors.Is(err, ErrTooManyLabels) {
		t.Fatalf("err = %v, want ErrTooManyLabels", err)
	}
}

func TestWarnings(t *testing.T) {
	_, warnings := renderAll(t, "^XA^PW1200^FO10,10^GB10,10,10^FS^XZ^XA^FO10,10^GB10,10,1,W^FS^XZ", size4x6)
	var text []string
	for _, w := range warnings {
		text = append(text, w.Text)
	}
	got := strings.Join(text, "; ")
	if !strings.Contains(got, "wider than the label") || !strings.Contains(got, "blank") {
		t.Errorf("warnings = %q", got)
	}
}

// Inputs that make zebrash panic must come back as errors.
func TestRendererPanicIsAnError(t *testing.T) {
	for _, zpl := range []string{
		"^XA^FO10,10^BCN,100^FDJOÃO^FS^XZ",
		"^XA^FO10,10^GFA,0,0,0,^FS^XZ",
		"^XA^GF^XZ",
	} {
		if _, err := Render([]byte(zpl), size4x6, 10, func(*Label) error { return nil }); err == nil {
			t.Errorf("%q: no error", zpl)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	ok := func(*Label) error { return nil }
	if _, err := Render([]byte("^XA^XZ"), Size{WidthMm: 100, HeightMm: 150, DPI: 250}, 10, ok); err == nil {
		t.Error("unsupported dpi accepted")
	}
	if _, err := Render([]byte("hello"), size4x6, 10, ok); err == nil {
		t.Error("data without labels accepted")
	}
}

// The carrier labels in the zebrash test data are 4x8 in at 203 dpi. They
// must match zebrash's reference images dot for dot. The references are
// drawn without ^POI, so labels with ^POI are compared with the reference
// turned 180 degrees. zebrash centres a label narrower than the media (^PW);
// the references use 813 dots of media and the bridge the exact 812.
func TestCarrierLabelsMatchReference(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/ingridhq/zebrash").Output()
	if err != nil {
		t.Fatal("zebrash module not found:", err)
	}
	dir := filepath.Join(strings.TrimSpace(string(out)), "testdata")
	size := Size{WidthMm: 101.6, HeightMm: 203.2, DPI: 203}
	carriers := []string{"amazon", "dbs", "dhlecommercetr", "dhlpaket", "dhlparceluk", "dpdpl", "fedex", "glscz",
		"glsdk_return", "icapaket", "jcpenney", "kmart", "pnldpd", "pocztex", "porterbuddy", "posten",
		"swisspost", "ups", "ups_surepost", "usps"}
	for _, name := range carriers {
		zpl, err := os.ReadFile(filepath.Join(dir, name+".zpl"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		ref := loadPNG(t, filepath.Join(dir, name+".png"))
		rw, rh := ref.Bounds().Dx(), ref.Bounds().Dy()
		inverted := strings.Contains(string(zpl), "^POI")
		labels, _ := renderAll(t, string(zpl), size)
		l := labels[0]
		dx := 0
		if m := regexp.MustCompile(`\^PW(\d+)`).FindSubmatch(zpl); m != nil {
			if pw, _ := strconv.Atoi(string(m[1])); pw < l.Width {
				dx = (rw-pw)/2 - (l.Width-pw)/2
			}
		}
		diff := 0
		for y := 0; y < l.Height && y < rh; y++ {
			for x := 0; x < l.Width && x < rw; x++ {
				rx, ry := x+dx, y
				if inverted {
					rx, ry = l.Width-1-x+dx, l.Height-1-y
				}
				if rx < 0 || rx >= rw {
					continue
				}
				r, g, b, _ := ref.At(rx, ry).RGBA()
				if l.At(x, y) != ((r+g+b)/3 < 0x8000) {
					diff++
				}
			}
		}
		if diff != 0 {
			t.Errorf("%s: %d dots differ from the zebrash reference", name, diff)
		}
	}
}

func loadPNG(t *testing.T, path string) image.Image {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// Scanning a large hostile job must not allocate per match.
func TestLastIndexCommandDoesNotAllocate(t *testing.T) {
	data := []byte("^XA" + strings.Repeat("^xz", 100000))
	if got := LastIndexCommand(data, "XZ"); got != len(data)-3 {
		t.Fatalf("LastIndexCommand = %d, want %d", got, len(data)-3)
	}
	if a := testing.AllocsPerRun(5, func() { LastIndexCommand(data, "XZ"); IndexCommand(data, "XA", 0) }); a != 0 {
		t.Errorf("%.0f allocations, want 0", a)
	}
}

func TestBlockAndWarningLimits(t *testing.T) {
	ok := func(*Label) error { return nil }
	if _, err := Render([]byte(strings.Repeat("^XA^XZ", 200)), size4x6, 50, ok); !errors.Is(err, ErrTooManyLabels) {
		t.Errorf("200 blocks: err = %v, want ErrTooManyLabels", err)
	}
	job := "^XA^FO1,1^GB5,5,5^FS^XZ" + strings.Repeat("^PQ2^XZ", 60) + strings.Repeat("^XA^PW1200^FO1,1^GB5,5,5^FS^XZ", 20)
	warnings, err := Render([]byte(job), size4x6, 50, ok)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != maxWarnings+1 || !strings.Contains(warnings[maxWarnings].Text, "more warnings") {
		t.Errorf("got %d warnings, want %d and a count", len(warnings), maxWarnings+1)
	}
	pq := 0
	for _, w := range warnings {
		if strings.Contains(w.Text, "^PQ") {
			pq++
		}
	}
	if pq != 1 {
		t.Errorf("^PQ warning given %d times, want once", pq)
	}
}
