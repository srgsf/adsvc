package metrics

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
)

// registerProcess adds the process_* metrics this platform can report:
// CPU on unix, memory and descriptors from /proc on Linux and Android.
// Resident memory includes the memory-mapped ad index; the ffmpeg children
// are separate processes and not counted.
func registerProcess() {
	NewCounterFunc("process_cpu_seconds_total", "User and system CPU time spent, in seconds.", func(e Emit) {
		if v, ok := cpuSeconds(); ok {
			e(v)
		}
	})
	if runtime.GOOS != "linux" && runtime.GOOS != "android" {
		return
	}
	NewGaugeFunc("process_resident_memory_bytes", "Resident memory size in bytes.", func(e Emit) {
		if _, rss, ok := statm(); ok {
			e(rss)
		}
	})
	NewGaugeFunc("process_virtual_memory_bytes", "Virtual memory size in bytes.", func(e Emit) {
		if vm, _, ok := statm(); ok {
			e(vm)
		}
	})
	NewGaugeFunc("process_open_fds", "Number of open file descriptors.", func(e Emit) {
		if ents, err := os.ReadDir("/proc/self/fd"); err == nil {
			e(float64(len(ents)))
		}
	})
}

// statm returns the virtual and resident sizes from /proc/self/statm.
func statm() (vm, rss float64, ok bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, 0, false
	}
	f := bytes.Fields(b)
	if len(f) < 2 {
		return 0, 0, false
	}
	size, err1 := strconv.ParseUint(string(f[0]), 10, 64)
	res, err2 := strconv.ParseUint(string(f[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	page := float64(os.Getpagesize())
	return float64(size) * page, float64(res) * page, true
}
