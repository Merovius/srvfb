package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	do(run)
}

func run(ctx *context) error {
	listen := flag.Bool("listen", false, "Listen")
	tick := flag.Duration("tick", time.Second, "Time between framebuffer-captures")
	flag.Parse()
	if flag.NArg() < 1 {
		return errors.New("usage: srvfb [<flags>] <addr>")
	}
	addr := flag.Arg(0)
	if !*listen {
		http.Handle("/", server(addr))
		log.Fatal(http.ListenAndServe("localhost:1234", nil))
	}

	if err := ctx.openFB(*tick); err != nil {
		return err
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		// TODO: Write image header
		go ctx.forEach(func(b []byte) error {
			_, err := c.Write(b)
			return err
		})
	}
}

const W, H = 1408, 1872

func server(addr string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Println("Not a flusher")
			http.Error(w, "internal", 500)
			return
		}

		c, err := net.Dial("tcp", addr)
		if err != nil {
			log.Println(err)
			http.Error(w, "internal", 500)
			return
		}
		defer c.Close()

		w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=endofsection")
		mpw := multipart.NewWriter(w)
		mpw.SetBoundary("endofsection")
		hdr := make(textproto.MIMEHeader)
		hdr.Add("Content-Type", "image/jpeg")
		im := image.NewGray16(image.Rect(0, 0, W, H))
		for {
			_, err = io.ReadFull(c, im.Pix)
			if err != nil {
				log.Println(err)
				return
			}
			w, err := mpw.CreatePart(hdr)
			if err != nil {
				log.Println(err)
				return
			}
			if err = jpeg.Encode(w, im, &jpeg.Options{Quality: 90}); err != nil {
				log.Println(err)
				return
			}
			flusher.Flush()
		}
	})
}

type context struct {
	mu sync.RWMutex
	c  *sync.Cond
	n  int32
	b  []byte
	r  []byte
}

func do(f func(*context) error) {
	ctx := new(context)
	ctx.c = sync.NewCond(ctx.mu.RLocker())
	defer func() {
		if v := recover(); v != nil && v != ctx {
			panic(v)
		}
	}()
	if err := f(ctx); err != nil {
		log.Fatal(err)
	}
}

func (ctx *context) openFB(tick time.Duration) error {
	f, err := os.Open("/dev/fb0")
	if err != nil {
		return err
	}
	ctx.r, err = unix.Mmap(int(f.Fd()), 0, W*H*2, unix.PROT_READ, unix.MAP_SHARED) // TODO
	if err == nil {
		go ctx.read(tick)
	}
	return err
}

func (ctx *context) read(d time.Duration) {
	log.Println(d)
	var n uint32

	go func() {
		i := 0
		for range time.Tick(time.Second) {
			i++
			fmt.Printf("%.2f FPS\n", float64(atomic.LoadUint32(&n))/float64(i))
		}
	}()

	for range time.Tick(d) {
		if atomic.LoadInt32(&ctx.n) <= 0 {
			continue
		}
		buf := new(bytes.Buffer)
		r := bytes.NewReader(ctx.r)
		w := io.Writer(buf)
		io.Copy(w, r)
		ctx.mu.Lock()
		ctx.b = buf.Bytes()
		ctx.c.Broadcast()
		ctx.mu.Unlock()
		atomic.AddUint32(&n, 1)
	}
}

func (ctx *context) forEach(f func([]byte) error) {
	atomic.AddInt32(&ctx.n, 1)
	defer atomic.AddInt32(&ctx.n, -1)

	for {
		ctx.mu.RLock()
		ctx.c.Wait()
		b := ctx.b
		ctx.mu.RUnlock()
		if err := f(b); err != nil {
			log.Println(err)
			return
		}
	}
}

type rleReader struct {
	r *bufio.Reader
	n int
	c byte
}

func newReader(r io.Reader) *rleReader {
	return &rleReader{r: bufio.NewReader(r)}
}

func (r *rleReader) Read(p []byte) (n int, err error) {
	for ; n < len(p); n++ {
		if r.n == 0 {
			if err = r.next(); err != nil {
				return n, err
			}
		}
		r.n--
		p[n] = r.c
	}
	return n, nil
}

func (r *rleReader) next() error {
	var buf [3]byte
	_, err := io.ReadFull(r.r, buf[:])
	if err != nil {
		return err
	}
	l := binary.BigEndian.Uint16(buf[:2])
	if l == 0 {
		return errors.New("invalid data")
	}
	r.n = int(l)
	r.c = buf[2]
	return nil
}

type rleWriter struct{ w *bufio.Writer }

func newWriter(w io.Writer) *rleWriter {
	return &rleWriter{bufio.NewWriter(w)}
}

func (w *rleWriter) Write(p []byte) (n int, err error) {
	for n < len(p) {
		c, l := p[n], 1
		for ; n+l < len(p); l++ {
			if p[n+l] != c {
				break
			}
			if l == (1<<16)-1 {
				break
			}
		}
		var buf [3]byte
		binary.BigEndian.PutUint16(buf[:2], uint16(l))
		buf[2] = c
		_, err = w.w.Write(buf[:])
		if err != nil {
			return n, err
		}
		n += l
	}
	return n, w.w.Flush()
}
