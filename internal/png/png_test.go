package png

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"io/ioutil"
	"os"
	"testing"
)

func init() {
	f, err := os.Open("testimg.png")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	pim, err := png.Decode(f)
	if err != nil {
		panic(err)
	}
	bounds := pim.Bounds()

	im = image.NewGray16(bounds)
	draw.Draw(im, bounds, pim, image.Pt(0, 0), draw.Over)
}

var (
	im  *image.Gray16
	enc = &png.Encoder{CompressionLevel: png.NoCompression}
)

func TestOwn(t *testing.T) {
	buf := new(bytes.Buffer)
	WritePNG(buf, im)
	img, err := png.Decode(buf)
	if err != nil {
		t.Fatalf("Generated invalid png: %v", err)
	}
	got, ok := img.(*image.Gray16)
	if !ok {
		t.Fatal("Decoded png is not Gray16")
	}
	if bytes.Compare(got.Pix, im.Pix) != 0 {
		t.Error("Generated PNGs differ")
	}
}

func BenchmarkStdlib(b *testing.B) {
	for i := 0; i < b.N; i++ {
		enc.Encode(ioutil.Discard, im)
	}
}

func BenchmarkOwn(b *testing.B) {
	for i := 0; i < b.N; i++ {
		WritePNG(ioutil.Discard, im)
	}
}
