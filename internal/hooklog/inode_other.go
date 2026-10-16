//go:build !unix

package hooklog

import "os"

func inode(os.FileInfo) uint64 { return 0 }
