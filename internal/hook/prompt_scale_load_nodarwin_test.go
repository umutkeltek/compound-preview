//go:build !darwin

package hook

// sysctlLoadAverageOneMinute does not exist on this platform: the load
// probe falls through to /proc/loadavg (see prompt_scale_load_test.go).
func sysctlLoadAverageOneMinute() (float64, bool) {
	return 0, false
}
