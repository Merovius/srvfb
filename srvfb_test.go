package main

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func TestRLE(t *testing.T) {
	var data []byte
	rnd := rand.New(rand.NewSource(0))
	for i := 0; i < 10; i++ {
		c := byte(rnd.Intn(256))
		l := rnd.Intn(10)
		n := len(data)
		data = append(data, make([]byte, l)...)
		for j := n; j < len(data); j++ {
			data[j] = c
		}
	}

	buf := new(bytes.Buffer)
	r, w := newReader(buf), newWriter(buf)
	n, err := w.Write(data)
	if err != nil || n < len(data) {
		t.Fatalf("Write() = %d, %v, want %d, <nil>", n, err, len(data))
	}
	got := make([]byte, len(data)+1)
	n, err = io.ReadFull(r, got)
	if n != len(data) || err != io.ErrUnexpectedEOF {
		t.Errorf("Read() = %d, %v, want %d, %v", n, err, len(data), io.ErrUnexpectedEOF)
	}
	got = got[:len(got)-1]
	if bytes.Compare(got, data) != 0 {
		t.Fatalf("Read() = %x\nWant %x", got, data)
	}
}
