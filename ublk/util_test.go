package ublk

import "unsafe"

func unsafePointer(b []byte) unsafe.Pointer {
	return unsafe.Pointer(unsafe.SliceData(b))
}
