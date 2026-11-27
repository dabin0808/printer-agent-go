//go:build windows

// 独立最小复现：验证 EnumPrintersW 两种 level 的枚举
package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

func main() {
	winspool := syscall.NewLazyDLL("winspool.drv")
	procEnum := winspool.NewProc("EnumPrintersW")

	for _, level := range []uintptr{4, 1} {
		var needed, returned uint32
		flags := uintptr(2 | 4) // LOCAL | CONNECTIONS
		r1, _, err := procEnum.Call(flags, 0, level, 0, 0,
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
		fmt.Printf("level=%d 预估: r1=%d needed=%d returned=%d err=%v\n", level, r1, needed, returned, err)
		if needed == 0 {
			continue
		}
		buf := make([]byte, needed)
		r1, _, err = procEnum.Call(flags, 0, level, uintptr(unsafe.Pointer(&buf[0])),
			uintptr(needed), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)))
		fmt.Printf("level=%d 枚举: r1=%d returned=%d err=%v\n", level, r1, returned, err)

		if level == 1 {
			// PRINTER_INFO_1W: {Flags DWORD; pDescription; pName; pComment} 三个指针
			type pi1 struct {
				Flags       uint32
				_           uint32
				Description *uint16
				Name        *uint16
				Comment     *uint16
			}
			infos := unsafe.Slice((*pi1)(unsafe.Pointer(&buf[0])), returned)
			for _, it := range infos {
				fmt.Printf("  name=%q desc=%q\n",
					utf16At(buf, uintptr(unsafe.Pointer(it.Name))),
					utf16At(buf, uintptr(unsafe.Pointer(it.Description))))
			}
		} else {
			type pi4 struct {
				Flags       uint32
				_           uint32
				PrinterName *uint16
				ServerName  *uint16
			}
			infos := unsafe.Slice((*pi4)(unsafe.Pointer(&buf[0])), returned)
			for _, it := range infos {
				fmt.Printf("  printer=%q server=%q\n",
					utf16At(buf, uintptr(unsafe.Pointer(it.PrinterName))),
					utf16At(buf, uintptr(unsafe.Pointer(it.ServerName))))
			}
		}
	}
}

func utf16At(buf []byte, p uintptr) string {
	start := p - uintptr(unsafe.Pointer(&buf[0]))
	if start >= uintptr(len(buf)) {
		return "(bad ptr)"
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[start])), (uintptr(len(buf))-start)/2)
	for j, c := range u16 {
		if c == 0 {
			return syscall.UTF16ToString(u16[:j])
		}
	}
	return "(unterminated)"
}
