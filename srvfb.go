// Copyright 2018 Axel Wagner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command srvfb serves a framebuffer device over HTTP.
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash"
	"hash/fnv"
	"image"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Merovius/srvfb/internal/fb"
	"github.com/Merovius/srvfb/internal/png"

	"golang.org/x/sys/unix"
)

func main() {
	if err := run(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "", "Address to listen on")
	proxy := flag.String("proxy", "", "Proxy the screen from the given address")
	device := flag.String("device", "", "Framebuffer device to serve")
	idle := flag.Duration("idle", 0, "Exit if there's no activity for this time. 0 disables this")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("usage: srvfb [<flags>]")
	}

	if (*proxy == "") == (*device == "") {
		return errors.New("exactly one of -proxy or -device is required")
	}
	if len(listenFDs) > 1 {
		return errors.New("more than one file descriptor passed by service manager")
	}
	var (
		l   net.Listener
		err error
	)
	if len(listenFDs) > 0 {
		if *listen != "" {
			return errors.New("can't use -listen with socket activation")
		}
		l, err = net.FileListener(listenFDs[0])
	} else {
		if *listen == "" {
			return errors.New("no file descriptor passed by service manager and no -listen set")
		}
		l, err = net.Listen("tcp", *listen)
	}
	if err != nil {
		return err
	}
	l = wrapListener(l, *idle)

	h := new(handler)

	if *device != "" {
		h.fb, err = fb.Open(*device)
	}
	if err != nil {
		return err
	}
	h.proxy = *proxy
	http.Handle("/", h)
	if err = http.Serve(l, nil); err == errIdle {
		log.Printf("No activity for %v, shutting down", *idle)
		err = nil
	}
	return err
}

type handler struct {
	fb    *fb.Device
	proxy string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println(r.Method, r.URL.Path)
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/":
		h.serveIndex(w, r)
	case "/video":
		h.serveVideo(w, r)
	case "/raw":
		if h.fb == nil {
			http.Error(w, "Not serving raw streams in proxy mode", http.StatusNotImplemented)
			return
		}
		h.serveRaw(w, r)
	default:
		http.Error(w, fmt.Sprintf("%q not found", r.URL.Path), http.StatusNotFound)
	}
}

const version = 1

type rawHeader struct {
	Version      uint8
	BitsPerPixel uint8
	_            uint16 // reserved
	Width        uint32
	Height       uint32
}

func (h *handler) serveRaw(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("Not a Flusher")
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	vinfo, err := h.fb.VarScreeninfo()
	if err != nil {
		log.Println(err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=endofsection")
	w.WriteHeader(http.StatusOK)

	mpw := multipart.NewWriter(w)
	mpw.SetBoundary("endofsection")
	hdr := make(textproto.MIMEHeader)
	hdr.Add("Content-Type", "binary/octet-stream")

	part, err := mpw.CreatePart(hdr)
	if err != nil {
		log.Println(err)
		return
	}
	rhdr := &rawHeader{version, 16, 0, vinfo.Xres, vinfo.Yres}
	if err = binary.Write(part, binary.BigEndian, rhdr); err != nil {
		log.Println(err)
		return
	}

	im := new(image.Gray16)
	var dedup deduper
	for {
		if err := h.readImage(im); err != nil {
			log.Println(err)
			return
		}
		if dedup.skip(im.Pix) {
			continue
		}
		w, err := mpw.CreatePart(hdr)
		if err != nil {
			log.Println(err)
			return
		}
		_, err = w.Write(im.Pix)
		if err != nil {
			log.Println(err)
			return
		}
		flusher.Flush()
	}
}

func (h *handler) serveVideo(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("Not a flusher")
		http.Error(w, "Internal Server Error", 500)
		return
	}

	var reader interface {
		readImage(im *image.Gray16) error
	}

	if h.proxy != "" {
		c, err := dialProxy(h.proxy)
		if err != nil {
			log.Println(err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		defer c.close()
		reader = c
	} else {
		reader = h
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=endofsection")
	w.WriteHeader(http.StatusOK)

	mpw := multipart.NewWriter(w)
	mpw.SetBoundary("endofsection")
	hdr := make(textproto.MIMEHeader)
	hdr.Add("Content-Type", "image/png")
	im := new(image.Gray16)
	enc := &png.Encoder{CompressionLevel: png.BestSpeed}
	var dedup deduper
	for {
		if err := reader.readImage(im); err != nil {
			log.Println(err)
			return
		}
		if dedup.skip(im.Pix) {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		w, err := mpw.CreatePart(hdr)
		if err != nil {
			log.Println(err)
			return
		}
		enc.Encode(w, im)
		flusher.Flush()
	}
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	const idx = `<!DOCTYPE html>
<html>
	<head>
		<meta charset="UTF-8">
		<title>srvfb</title>
		<style>
			body {
				width: 100%;
				height: 100%;
			}

			#stream {
				position: absolute;
				top: 0;
				left: 0;
				background: url("video");
				background-position: center;
				background-size: contain;
				background-repeat: no-repeat;
				background-color: black;
				transform: rotate(0deg);
			}
		</style>

		<script>
			document.onreadystatechange = function(e) {
				if (document.readyState !== "complete") {
					return;
				}
				let rotate = 0;
				let stream = document.querySelector('#stream')
				let w = stream.width;
				let h = stream.height;
				let resize = function() {
					let [nt, nl, nh, nw] = [0,0,0,0];
					if ((w > h) == (rotate%2)) {
						nh = window.innerHeight;
						nw = window.innerWidth;
					} else {
						nh = window.innerWidth;
						nw = window.innerHeight;
					}
					if (rotate%2) {
						// CSS is black magic to me. We have to offset the
						// image when it's rotated. I have no idea why these
						// offsets work - but empirically, they seem to do.
						nl = (nh-nw)/2;
						nt = (nw-nh)/2;
					}
					stream.style.height = nh + "px";
					stream.style.width = nw + "px";
					stream.style.top = nt + "px";
					stream.style.left = nl + "px";
					stream.style.transform = 'rotate('+rotate*90+'deg)';
				};
				resize();
				stream.onclick = function(ev) {
					rotate = (rotate+1)%4;
					resize();
				};
				window.onresize = resize;
			};
		</script>
	</head>
	<body>
		<div id="stream"></div>
	</body>
</html>`
	io.WriteString(w, idx)
}

func (h *handler) readImage(im *image.Gray16) error {
	vim, err := h.fb.Image()
	if err != nil {
		return err
	}
	gim, ok := vim.(*image.Gray16)
	if !ok {
		return errors.New("framebuffer is not 16-bit grayscale")
	}
	if len(im.Pix) < len(gim.Pix) {
		im.Pix = append(im.Pix, make([]byte, len(gim.Pix)-len(im.Pix))...)
	}
	copy(im.Pix, gim.Pix)
	im.Stride = gim.Stride
	im.Rect = gim.Rect
	return nil
}

type proxyconn struct {
	r      *multipart.Reader
	closer io.Closer
	width  int
	height int
}

func dialProxy(addr string) (*proxyconn, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s/raw", addr))
	if err != nil {
		return nil, err
	}
	c := &proxyconn{closer: resp.Body}
	if err = c.readHdr(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return c, nil
}

func (c *proxyconn) readHdr(resp *http.Response) error {
	mt, parms, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	if mt != "multipart/x-mixed-replace" {
		return fmt.Errorf("unknown media type %q", mt)
	}
	if parms["boundary"] == "" {
		return fmt.Errorf("no boundary in media type %q", resp.Header.Get("Content-Type"))
	}
	c.r = multipart.NewReader(resp.Body, parms["boundary"])

	part, err := c.r.NextPart()
	if err != nil {
		return err
	}
	defer part.Close()
	if ct := part.Header.Get("Content-Type"); ct != "binary/octet-stream" {
		return fmt.Errorf("unknown Content-Type %q for part", ct)
	}

	var hdr rawHeader
	if err := binary.Read(part, binary.BigEndian, &hdr); err != nil {
		return err
	}
	log.Printf("Got header: %#x", hdr)
	if hdr.Version != version {
		return fmt.Errorf("incompatible version %d", hdr.BitsPerPixel)
	}
	if hdr.BitsPerPixel != 16 {
		return fmt.Errorf("incompatible bits per pixel %d", hdr.BitsPerPixel)
	}
	c.width = int(hdr.Width)
	c.height = int(hdr.Height)
	return nil
}

func (c *proxyconn) readImage(im *image.Gray16) error {
	if len(im.Pix) != c.width*c.height*2 {
		*im = *image.NewGray16(image.Rect(0, 0, c.width, c.height))
	}
	part, err := c.r.NextPart()
	if err != nil {
		return err
	}
	defer part.Close()
	if ct := part.Header.Get("Content-Type"); ct != "binary/octet-stream" {
		return fmt.Errorf("unknown Content-Type %q for part", ct)
	}
	_, err = io.ReadFull(part, im.Pix)
	return err
}

func (c *proxyconn) close() {
	c.closer.Close()
}

// deduper keeps state to deduplicate sent frames. For some reason, Chrome only
// seems to show a frame *after* the frame after has been sent (i.e. it lags
// behind one frame), so we only start skipping after two consecutive frames
// are identical.
type deduper struct {
	h  hash.Hash32
	h1 uint32
	h2 uint32
}

func (d *deduper) skip(b []byte) bool {
	if d.h == nil {
		d.h = fnv.New32a()
	}
	d.h.Reset()
	d.h.Write(b)
	h := d.h.Sum32()
	if h == d.h1 && h == d.h2 {
		return true
	}
	d.h1, d.h2 = d.h2, h
	return false
}

var errIdle = errors.New("idle timeout")

// wrapListener wraps l with an idle timeout, if possible. A zero timeout
// disables timeouts. If setting a timeout fails, the returned Listener falls
// back to the behavior of the wrapped Listener.
func wrapListener(l net.Listener, timeout time.Duration) net.Listener {
	tl, ok := l.(*net.TCPListener)
	if !ok || timeout == 0 {
		return l
	}
	return &listener{
		TCPListener: tl,
		timeout:     timeout,
	}
}

type listener struct {
	acceptMu sync.Mutex
	active   uint32
	*net.TCPListener
	timeout time.Duration
}

// Accept implements net.Conn. Connections returned by Accept are tracked. Once
// all active connections are closed and the idle timeout expires, Accept
// returns errIdle.
func (l *listener) Accept() (net.Conn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()

	if atomic.LoadUint32(&l.active) == 0 {
		l.SetDeadline(time.Now().Add(l.timeout))
	}
	c, err := l.TCPListener.Accept()
	if err == nil {
		atomic.AddUint32(&l.active, 1)
		l.SetDeadline(time.Time{})
		return &conn{l: l, Conn: c}, nil
	}
	to, ok := err.(interface{ Timeout() bool })
	if !ok || !to.Timeout() {
		return nil, err
	}
	return nil, errIdle
}

type conn struct {
	o sync.Once
	l *listener
	net.Conn
}

func (c *conn) Close() error {
	c.o.Do(func() {
		if atomic.AddUint32(&c.l.active, ^uint32(0)) == 0 {
			c.l.SetDeadline(time.Now().Add(c.l.timeout))
		}
	})
	return c.Conn.Close()
}

var listenFDs []*os.File

func init() {
	defer func() {
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
	}()
	var (
		pid   int
		fds   int
		names []string
		err   error
	)
	if s := os.Getenv("LISTEN_PID"); s == "" {
		return
	} else {
		pid, err = strconv.Atoi(s)
	}
	if err != nil {
		log.Printf("Can't parse $LISTEN_PID: %v", err)
		return
	}
	if os.Getpid() != pid {
		return
	}
	if s := os.Getenv("LISTEN_FDS"); s == "" {
		return
	} else {
		fds, err = strconv.Atoi(s)
	}
	if err != nil {
		log.Printf("Can't parse $LISTEN_PID: %v", err)
		return
	}
	if s := os.Getenv("LISTEN_FDNAMES"); s != "" {
		names = strings.Split(s, ":")
	}
	for i := len(names); i < fds; i++ {
		names = append(names, "unknown")
	}
	for i := 0; i < fds; i++ {
		unix.FcntlInt(3+uintptr(i), unix.F_SETFD, unix.FD_CLOEXEC)
		listenFDs = append(listenFDs, os.NewFile(3+uintptr(i), names[i]))
	}
}
