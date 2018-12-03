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
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Merovius/srvfb/internal/png"

	"golang.org/x/sys/unix"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	listen := flag.String("listen", ":1234", "Address to listen on")
	proxy := flag.String("proxy", "", "Proxy the screen from the given address")
	device := flag.String("device", "", "Framebuffer device to serve")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("usage: srvfb [<flags>]")
	}

	if (*proxy == "") == (*device == "") {
		return errors.New("exactly one of -proxy or -device is required")
	}

	h := new(handler)

	var err error
	if *device != "" {
		h.fb, err = open(*device)
	}
	if err != nil {
		return err
	}
	h.proxy = *proxy
	http.Handle("/", h)
	return http.ListenAndServe(*listen, nil)
}

type handler struct {
	fb    *fb
	proxy string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Println(r.Method, r.URL.Path)
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
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

	w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=endofsection")
	w.WriteHeader(http.StatusOK)

	mpw := multipart.NewWriter(w)
	mpw.SetBoundary("endofsection")
	hdr := make(textproto.MIMEHeader)
	hdr.Add("Content-Type", "binary/octet-stream")

	log.Println("writing header")
	part, err := mpw.CreatePart(hdr)
	if err != nil {
		log.Println(err)
		return
	}
	rhdr := &rawHeader{version, 16, 0, uint32(h.fb.width), uint32(h.fb.height)}
	if err = binary.Write(part, binary.BigEndian, rhdr); err != nil {
		log.Println(err)
		return
	}

	im := new(image.Gray16)
	for {
		log.Println("reading image")
		if err := h.fb.read(im); err != nil {
			log.Println(err)
			return
		}
		log.Println("starting multipart")
		w, err := mpw.CreatePart(hdr)
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("writing image")
		_, err = w.Write(im.Pix)
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("flushing")
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
		read(im *image.Gray16) error
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
		reader = h.fb
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary=endofsection")
	w.WriteHeader(http.StatusOK)

	mpw := multipart.NewWriter(w)
	mpw.SetBoundary("endofsection")
	hdr := make(textproto.MIMEHeader)
	hdr.Add("Content-Type", "image/png")
	im := new(image.Gray16)
	enc := &png.Encoder{CompressionLevel: png.BestSpeed}
	for {
		log.Println("reading image")
		if err := reader.read(im); err != nil {
			log.Println(err)
			return
		}
		log.Println("starting multipart")
		w, err := mpw.CreatePart(hdr)
		if err != nil {
			log.Println(err)
			return
		}
		log.Println("encoding png")
		enc.Encode(w, im)
		log.Println("flushing")
		flusher.Flush()
	}
}

func (h *handler) readImage(im *image.Gray16) error {
	if h.proxy == "" {
		return h.fb.read(im)
	}
	return errors.New("not implemented")
}

type fb struct {
	file   []byte
	width  int
	height int
}

func open(file string) (*fb, error) {
	fb := new(fb)

	// TODO: Use framebuffer ioctls
	base := filepath.Base(file)
	sysfs := filepath.Join("/sys/class/graphics/", base)

	buf, err := ioutil.ReadFile(filepath.Join(sysfs, "virtual_size"))
	if err != nil {
		return nil, err
	}
	sp := strings.Split(string(bytes.TrimSpace(buf)), ",")
	if len(sp) != 2 {
		return nil, errors.New("invalid format of virtual_size")
	}
	if fb.width, err = strconv.Atoi(sp[0]); err != nil {
		return nil, err
	}
	if fb.height, err = strconv.Atoi(sp[1]); err != nil {
		return nil, err
	}
	// TODO: Why is the height too large?
	fb.height /= 2

	buf, err = ioutil.ReadFile(filepath.Join(sysfs, "bits_per_pixel"))
	if err != nil {
		return nil, err
	}
	if bpp, err := strconv.Atoi(string(bytes.TrimSpace(buf))); err != nil || bpp != 16 {
		return nil, errors.New("invalid bits_per_pixel")
	}
	f, err := os.OpenFile(file, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if fb.file, err = unix.Mmap(int(f.Fd()), 0, fb.width*fb.height*2, unix.PROT_READ, unix.MAP_SHARED); err != nil {
		return nil, fmt.Errorf("mmap failed: %v", err)
	}
	return fb, nil
}

func (fb *fb) read(im *image.Gray16) error {
	if im.Pix == nil {
		*im = *image.NewGray16(image.Rect(0, 0, fb.width, fb.height))
	}
	copy(im.Pix, fb.file)
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

func (c *proxyconn) read(im *image.Gray16) error {
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
