//go:build !darwin && !linux

package disk

import "os"

func mediaOffline(info os.FileInfo) bool { return info == nil }
func mediaFilesystemLocal(string) bool   { return false }
