// Copyright (c) VisualClassroom. All rights reserved.

//go:build !windows

package utils

// ShowErrorDialog 非 Windows 平台为空实现（错误仍会输出到日志）。
func ShowErrorDialog(title, message string) {}
