//go:build !darwin && !linux

package platform

import "os"

// DataStart is where f's data starts: 0 on this system, which can't tell.
func DataStart(*os.File) int64 { return 0 }
