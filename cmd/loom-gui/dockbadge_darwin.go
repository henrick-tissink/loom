//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
#import <Cocoa/Cocoa.h>

// setDockBadgeLabel sets the Dock tile badge. Dispatched to the main queue
// because AppKit UI mutation must happen there, and loom calls this from the
// poll goroutine. An empty string clears the badge. Safe before NSApp is up:
// objc nil-messaging makes [nil dockTile] a no-op.
static void setDockBadgeLabel(const char *label) {
  NSString *s = label ? [NSString stringWithUTF8String:label] : @"";
  dispatch_async(dispatch_get_main_queue(), ^{
    [[NSApp dockTile] setBadgeLabel:s];
  });
}
*/
import "C"

import (
	"strconv"
	"unsafe"
)

// setDockBadge shows count on the Dock icon (cleared when count <= 0), so the
// needs-you total is visible even when loom's window is in the background.
func setDockBadge(count int) {
	label := ""
	if count > 0 {
		label = strconv.Itoa(count)
	}
	c := C.CString(label)
	defer C.free(unsafe.Pointer(c))
	C.setDockBadgeLabel(c)
}
