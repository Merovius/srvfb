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

// Package fb implements framebuffer interaction via ioctls and mmap.
package fb

import (
	"errors"
	"fmt"
	"image"
	"unsafe"

	"golang.org/x/sys/unix"
)

type Device struct {
	fd    uintptr
	mmap  []byte
	finfo FixScreeninfo
}

func Open(dev string) (*Device, error) {
	fd, err := unix.Open(dev, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", dev, err)
	}
	if int(uintptr(fd)) != fd {
		unix.Close(fd)
		return nil, errors.New("fd overflows")
	}
	d := &Device{fd: uintptr(fd)}

	_, _, eno := unix.Syscall(unix.SYS_IOCTL, d.fd, FBIOGET_FSCREENINFO, uintptr(unsafe.Pointer(&d.finfo)))
	if eno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("FBIOGET_FSCREENINFO: %v", eno)
	}

	d.mmap, err = unix.Mmap(fd, 0, int(d.finfo.Smem_len), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("mmap: %v", err)
	}
	return d, nil
}

func (d *Device) VarScreeninfo() (VarScreeninfo, error) {
	var vinfo VarScreeninfo
	_, _, eno := unix.Syscall(unix.SYS_IOCTL, d.fd, FBIOGET_VSCREENINFO, uintptr(unsafe.Pointer(&vinfo)))
	if eno != 0 {
		return vinfo, fmt.Errorf("FBIOGET_VSCREENINFO: %v", eno)
	}
	return vinfo, nil
}

func (d *Device) Image() (image.Image, error) {
	vinfo, err := d.VarScreeninfo()
	if err != nil {
		return nil, err
	}
	if vinfo.Bits_per_pixel != 16 {
		return nil, fmt.Errorf("%d bits per pixel unsupported", vinfo.Bits_per_pixel)
	}
	virtual := image.Rect(0, 0, int(vinfo.Xres_virtual), int(vinfo.Yres_virtual))
	if virtual.Dx()*virtual.Dy()*2 != len(d.mmap) {
		return nil, errors.New("virtual resolution doesn't match framebuffer size")
	}
	visual := image.Rect(int(vinfo.Xoffset), int(vinfo.Yoffset), int(vinfo.Xres), int(vinfo.Yres))
	if !visual.In(virtual) {
		return nil, errors.New("visual resolution not contained in virtual resolution")
	}
	return &image.Gray16{
		Pix:    d.mmap,
		Stride: int(d.finfo.Line_length),
		Rect:   visual,
	}, nil
}

func (d *Device) Close() error {
	e1 := unix.Munmap(d.mmap)
	if e2 := unix.Close(int(d.fd)); e2 != nil {
		return e2
	}
	return e1
}
