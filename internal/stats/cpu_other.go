//go:build !linux

package stats

import "time"

// procCPU is only supported on Linux: on other systems CPU shows as 0%.
func procCPU(pid int) (time.Duration, bool) { return 0, false }
