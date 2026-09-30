//go:build !unix

package metrics

func cpuSeconds() (float64, bool) { return 0, false }
