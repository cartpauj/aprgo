//go:build !linux

package gps

import (
	"errors"
	"os"
)

// openGPSSerial is a stub for non-Linux builds; aprgo's serial I/O (RF + GPS)
// is Linux-only. gpsd over TCP still works on any platform.
func openGPSSerial(path string, baud int) (*os.File, error) {
	return nil, errors.New("local serial GPS is only supported on Linux (use gpsd instead)")
}
