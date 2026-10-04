package main

import "unsafe"

// addr is the address of b's first byte, for alignment.
func addr(b []byte) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}
