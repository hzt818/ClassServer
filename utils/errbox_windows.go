// Copyright (c) VisualClassroom. All rights reserved.

//go:build windows

package utils

import (
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	messageBoxW     = user32.NewProc("MessageBoxW")
)

// ShowErrorDialog 在 Windows 上弹出中文错误对话框（阻塞直到用户点击）。
// 用于启动失败等用户必须看到的原因提示；非 Windows 平台为空实现。
func ShowErrorDialog(title, message string) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	msgPtr, _ := syscall.UTF16PtrFromString(message)
	// MB_OK | MB_ICONERROR | MB_TOPMOST = 0x10
	messageBoxW.Call(0, uintptr(unsafe.Pointer(msgPtr)), uintptr(unsafe.Pointer(titlePtr)), 0x10)
}
