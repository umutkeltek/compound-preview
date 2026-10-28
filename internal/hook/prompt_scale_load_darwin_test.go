//go:build darwin

package hook

import (
	"encoding/binary"
	"syscall"
	"testing"
)

// sysctl.Sysctl returns vm.loadavg as the binary Darwin struct loadavg, not
// the braced text printed by the sysctl command. On supported 64-bit Darwin,
// ldavg[0] is a fixed-point uint32 at offset 0 and fscale is a long at offset
// 16. syscall.Sysctl may remove the final NUL byte, so only the first 20
// bytes are required. A failed decode means contention is unknown.
func sysctlLoadAverageOneMinute() (float64, bool) {
	value, err := syscall.Sysctl("vm.loadavg")
	if err != nil {
		return 0, false
	}
	return parseDarwinSysctlLoadAverage(value)
}

func parseDarwinSysctlLoadAverage(raw string) (float64, bool) {
	data := []byte(raw)
	if len(data) < 20 {
		return 0, false
	}
	fixed := binary.NativeEndian.Uint32(data[:4])
	scale := binary.NativeEndian.Uint32(data[16:20])
	if scale == 0 {
		return 0, false
	}
	return float64(fixed) / float64(scale), true
}

func TestParseDarwinSysctlLoadAverage(t *testing.T) {
	data := make([]byte, 24)
	binary.NativeEndian.PutUint32(data[:4], 33792)
	binary.NativeEndian.PutUint32(data[4:8], 20480)
	binary.NativeEndian.PutUint32(data[8:12], 10240)
	binary.NativeEndian.PutUint32(data[16:20], 2048)
	for _, raw := range []string{string(data), string(data[:23])} {
		got, ok := parseDarwinSysctlLoadAverage(raw)
		if !ok || got != 16.5 {
			t.Fatalf("binary vm.loadavg len=%d parsed as (%v,%v), want (16.5,true)", len(raw), got, ok)
		}
	}
	if _, ok := parseDarwinSysctlLoadAverage(string(data[:19])); ok {
		t.Fatal("accepted truncated binary load average")
	}
	binary.NativeEndian.PutUint32(data[16:20], 0)
	if _, ok := parseDarwinSysctlLoadAverage(string(data)); ok {
		t.Fatal("accepted zero fscale")
	}
}

func TestDarwinSysctlLoadAverageOneMinute(t *testing.T) {
	got, ok := sysctlLoadAverageOneMinute()
	if !ok || got < 0 {
		t.Fatalf("live vm.loadavg unavailable or invalid: (%v,%v)", got, ok)
	}
}
