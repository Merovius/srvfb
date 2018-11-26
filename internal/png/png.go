package png

import (
	"encoding/binary"
	"fmt"
	"hash/adler32"
	"hash/crc32"
	"image"
	"io"
)

func WritePNG(out io.Writer, im *image.Gray16) {
	bounds := im.Bounds()

	// PNG Header
	fmt.Fprint(out, "\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(bounds.Dx())) // width
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(bounds.Dy())) // height
	ihdr[8] = 16                                               // bit-depth
	ihdr[9] = 0                                                // color-type = grayscale
	ihdr[10] = 0                                               // compression-method
	ihdr[11] = 0                                               // filter-method
	ihdr[12] = 0                                               // interlace-method
	writeChunk(out, ihdr, "IHDR")
	writeIDAT(out, im)
	writeChunk(out, nil, "IEND")
}

func writeIDAT(out io.Writer, im *image.Gray16) {
	var buf [4]byte

	chunkSum := crc32.NewIEEE()
	chunkContent := io.MultiWriter(out, chunkSum)
	zlibSum := adler32.New()
	contentLen := len(im.Pix) + im.Bounds().Dy()
	fw := flate(chunkContent, contentLen)
	zlibContent := io.MultiWriter(&fw, zlibSum)

	// chunk-header. Chunk content is
	//   * chunk type (not counted for length)
	//   * 6 bytes for zlib header/footer
	//   * pixel-data
	//   * 1 zero-byte filter method per line
	dflen := uint32(deflateLen(contentLen))
	binary.BigEndian.PutUint32(buf[:], 6+dflen)
	out.Write(buf[:])
	chunkContent.Write([]byte("IDAT"))

	// zlib-header
	chunkContent.Write([]byte{0x78, 0x01})

	// "compressed" content
	for i := 0; i < len(im.Pix); i += im.Stride {
		zlibContent.Write([]byte{0})
		zlibContent.Write(im.Pix[i : i+im.Stride])
	}
	fw.Close()
	// zlib-footer
	chunkContent.Write(zlibSum.Sum(buf[:0]))
	// chunk-footer
	out.Write(chunkSum.Sum(buf[:0]))
}

func deflateLen(n int) int {
	nblocks := (1 + (n-1)/0xffff)
	return n + (nblocks+1)*5
}

func flate(w io.Writer, total int) flateWriter {
	return flateWriter{w: w, total: total}
}

type flateWriter struct {
	w     io.Writer
	block int
	total int
}

var total = 0

func (w *flateWriter) Write(p []byte) (n int, err error) {
	total += len(p)
	if w.block == 0 {
		w.block = 0xffff
		if w.total < w.block {
			w.block = w.total
		}
		w.total -= w.block
		var hdr [5]byte
		binary.LittleEndian.PutUint16(hdr[1:], uint16(w.block))
		binary.LittleEndian.PutUint16(hdr[3:], ^uint16(w.block))
		_, err = w.w.Write(hdr[:])
		if err != nil {
			return 0, err
		}
	}
	if len(p) <= w.block {
		n, err = w.w.Write(p)
		w.block -= n
		return n, err
	}
	x := w.block
	n, err = w.w.Write(p[:x])
	w.block -= n
	if err != nil {
		return n, err
	}
	m, err := w.Write(p[x:])
	return n + m, err
}

func (w *flateWriter) Close() error {
	if w.total != 0 || w.block != 0 {
		panic("wrote less than anticipated")
	}
	w.w.Write([]byte{1, 0, 0, 0xff, 0xff})
	return nil
}

func writeChunk(out io.Writer, b []byte, typ string) {
	if len(typ) != 4 {
		panic("len(typ) != 4")
	}
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(b)))
	out.Write(buf[:])

	h := crc32.NewIEEE()
	w := io.MultiWriter(out, h)
	io.WriteString(w, typ)
	w.Write(b)
	out.Write(h.Sum(buf[:0]))
}
