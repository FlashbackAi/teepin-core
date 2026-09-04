// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package agentrunner

import (
	"fmt"
	"testing"
	"time"
)

func TestParseCPUStatLine(t *testing.T) {
	// Real /proc/stat shape: "cpu  user nice system idle iowait irq softirq steal guest guest_nice"
	stat := "cpu  10132153 290696 3084719 46828483 16683 0 25195 0 175628 0\n" +
		"cpu0 1234567 12345 123456 1234567 1234 0 1234 0 12345 0\n" +
		"intr 123456789 0 0 ...\n"

	sample, ok := parseCPUStatLine(stat)
	if !ok {
		t.Fatal("parseCPUStatLine failed on well-formed input")
	}
	wantTotal := uint64(10132153 + 290696 + 3084719 + 46828483 + 16683 + 0 + 25195 + 0 + 175628 + 0)
	wantIdle := uint64(46828483 + 16683) // idle + iowait
	if sample.total != wantTotal {
		t.Errorf("total = %d, want %d", sample.total, wantTotal)
	}
	if sample.idle != wantIdle {
		t.Errorf("idle = %d, want %d", sample.idle, wantIdle)
	}
}

func TestParseCPUStatLine_NoCPULine(t *testing.T) {
	if _, ok := parseCPUStatLine("intr 123456789 0 0\nctxt 987654321\n"); ok {
		t.Error("expected failure when no \"cpu \" line is present")
	}
}

func TestParseCPUStatLine_MalformedField(t *testing.T) {
	if _, ok := parseCPUStatLine("cpu  10132153 not-a-number 3084719 46828483\n"); ok {
		t.Error("expected failure on a non-numeric field")
	}
}

func TestCPUPercentFromSamples(t *testing.T) {
	// 10s wall-clock delta, 3s of it idle -> 70% used.
	prev := cpuSample{idle: 100, total: 1000}
	cur := cpuSample{idle: 103, total: 1010}
	percent, ok := cpuPercentFromSamples(prev, cur)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if percent < 69.9 || percent > 70.1 {
		t.Errorf("percent = %v, want ~70", percent)
	}
}

func TestCPUPercentFromSamples_FullyIdle(t *testing.T) {
	prev := cpuSample{idle: 100, total: 1000}
	cur := cpuSample{idle: 110, total: 1010}
	percent, ok := cpuPercentFromSamples(prev, cur)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if percent < -0.1 || percent > 0.1 {
		t.Errorf("percent = %v, want ~0 (all delta was idle)", percent)
	}
}

func TestCPUPercentFromSamples_NoAdvance(t *testing.T) {
	sample := cpuSample{idle: 100, total: 1000}
	if _, ok := cpuPercentFromSamples(sample, sample); ok {
		t.Error("expected ok=false when totalDelta is 0 (would divide by zero)")
	}
}

func TestParseMemInfo(t *testing.T) {
	meminfo := "MemTotal:       32780000 kB\n" +
		"MemFree:         1234000 kB\n" +
		"MemAvailable:   16390000 kB\n" +
		"Buffers:          500000 kB\n"

	usedGB, ok := parseMemInfo(meminfo)
	if !ok {
		t.Fatal("parseMemInfo failed on well-formed input")
	}
	wantGB := float64(32780000-16390000) / (1024.0 * 1024.0)
	if usedGB < wantGB-0.001 || usedGB > wantGB+0.001 {
		t.Errorf("usedGB = %v, want %v", usedGB, wantGB)
	}
}

func TestParseMemInfo_MissingMemAvailable(t *testing.T) {
	// Very old kernels (<3.14) lack MemAvailable entirely.
	meminfo := "MemTotal:       32780000 kB\nMemFree:         1234000 kB\n"
	if _, ok := parseMemInfo(meminfo); ok {
		t.Error("expected failure when MemAvailable is absent")
	}
}

func TestReadCPUSample_FileReadError(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()
	readFileFunc = func(string) ([]byte, error) { return nil, fmt.Errorf("permission denied") }

	if _, ok := readCPUSample(); ok {
		t.Error("expected ok=false when the underlying file read fails")
	}
}

func TestReadMemoryUsedGB_FileReadError(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()
	readFileFunc = func(string) ([]byte, error) { return nil, fmt.Errorf("permission denied") }

	if _, ok := readMemoryUsedGB(); ok {
		t.Error("expected ok=false when the underlying file read fails")
	}
}

// TestRunner_HostUtilization_DeltaAcrossCalls is the regression test for
// the real bug an earlier version of this code had: a failed read must
// never overwrite a good stored sample with a bogus zero value, and the
// FIRST successful read must be stored as a baseline (yielding 0%, not
// an error) even though no percentage can be reported yet.
func TestRunner_HostUtilization_DeltaAcrossCalls(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()

	statCall := 0
	readFileFunc = func(path string) ([]byte, error) {
		switch path {
		case "/proc/meminfo":
			return []byte("MemTotal: 1000 kB\nMemAvailable: 500 kB\n"), nil
		case "/proc/net/dev", "/proc/diskstats":
			// Not relevant to this CPU/mem-focused test — fail cleanly
			// rather than participate in the /proc/stat call count below.
			return nil, fmt.Errorf("not relevant to this test")
		case "/proc/stat":
			statCall++
			switch statCall {
			case 1:
				return []byte("cpu  100 0 0 900 0 0 0 0 0 0\n"), nil // total=1000, idle=900
			case 2:
				return []byte("cpu  200 0 0 900 0 0 0 0 0 0\n"), nil // total=1100, idle=900 -> delta all "used"
			default:
				return nil, fmt.Errorf("unexpected extra /proc/stat read")
			}
		default:
			return nil, fmt.Errorf("unexpected path %q", path)
		}
	}

	r := &Runner{}

	cpu1, _, _, _, _, _ := r.hostUtilization()
	if cpu1 != 0 {
		t.Errorf("first call: cpuPercent = %v, want 0 (no prior sample to diff against)", cpu1)
	}
	if !r.haveLastCPUSample {
		t.Error("first successful read must be stored as the baseline for the next call")
	}

	cpu2, mem2, _, _, _, _ := r.hostUtilization()
	if cpu2 < 99.9 || cpu2 > 100.1 {
		t.Errorf("second call: cpuPercent = %v, want ~100 (entire delta was non-idle)", cpu2)
	}
	if mem2 < 0.0004 || mem2 > 0.0006 { // (1000-500)kB in GB
		t.Errorf("memUsedGB = %v, want ~0.000477", mem2)
	}
}

// TestRunner_HostUtilization_FailedReadDoesNotCorruptBaseline is the
// direct regression test for the bug itself: a read failure between two
// good reads must not reset the stored baseline, and must not produce a
// nonsense percentage from comparing against a zero sample.
func TestRunner_HostUtilization_FailedReadDoesNotCorruptBaseline(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()

	statCall := 0
	readFileFunc = func(path string) ([]byte, error) {
		switch path {
		case "/proc/meminfo", "/proc/net/dev", "/proc/diskstats":
			return nil, fmt.Errorf("not relevant to this test")
		case "/proc/stat":
			statCall++
			switch statCall {
			case 1:
				return []byte("cpu  100 0 0 900 0 0 0 0 0 0\n"), nil // total=1000, idle=900
			case 2:
				return nil, fmt.Errorf("transient read failure")
			case 3:
				return []byte("cpu  200 0 0 900 0 0 0 0 0 0\n"), nil // total=1100, idle=900
			default:
				return nil, fmt.Errorf("unexpected extra /proc/stat read")
			}
		default:
			return nil, fmt.Errorf("unexpected path %q", path)
		}
	}

	r := &Runner{}

	r.hostUtilization()                 // establishes the baseline (call 1)
	cpuMid, _, _, _, _, _ := r.hostUtilization() // read fails (call 2) — baseline must survive untouched
	if cpuMid != 0 {
		t.Errorf("during a failed read, cpuPercent = %v, want 0 (no new data, not corrupted data)", cpuMid)
	}
	if r.lastCPUSample.total != 1000 {
		t.Fatalf("a failed read must not overwrite the stored baseline, got total=%d, want 1000", r.lastCPUSample.total)
	}

	cpuFinal, _, _, _, _, _ := r.hostUtilization() // call 3, diffs correctly against the SURVIVING baseline from call 1
	if cpuFinal < 99.9 || cpuFinal > 100.1 {
		t.Errorf("cpuPercent = %v, want ~100 — the failed read in between must not have corrupted the delta", cpuFinal)
	}
}

// ---------------------------------------------------------------------
// Network throughput
// ---------------------------------------------------------------------

func TestParseNetDev(t *testing.T) {
	dev := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo:  116232     852    0    0    0     0          0         0   116232     852    0    0    0     0       0          0\n" +
		"  eth0: 2782917   19835    0    0    0     0          0         0   200000     1234    0    0    0     0       0          0\n" +
		"docker0:   50000     100    0    0    0     0          0         0     6000       50    0    0    0     0       0          0\n"

	sample, ok := parseNetDev(dev)
	if !ok {
		t.Fatal("parseNetDev failed on well-formed input")
	}
	// Only eth0 counts — lo is loopback, docker0 is a virtual bridge.
	if sample.rxBytes != 2782917 {
		t.Errorf("rxBytes = %d, want 2782917 (eth0 only)", sample.rxBytes)
	}
	if sample.txBytes != 200000 {
		t.Errorf("txBytes = %d, want 200000 (eth0 only)", sample.txBytes)
	}
}

func TestParseNetDev_OnlyVirtualInterfaces(t *testing.T) {
	dev := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"    lo:  116232     852    0    0    0     0          0         0   116232     852    0    0    0     0       0          0\n"
	if _, ok := parseNetDev(dev); ok {
		t.Error("expected ok=false when every interface is virtual/loopback")
	}
}

func TestNetRatesFromSamples(t *testing.T) {
	prev := netSample{rxBytes: 0, txBytes: 0}
	cur := netSample{rxBytes: 10 * 1024 * 1024, txBytes: 2 * 1024 * 1024} // 10MB rx, 2MB tx
	rx, tx, ok := netRatesFromSamples(prev, cur, 2*time.Second)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if rx < 4.9 || rx > 5.1 {
		t.Errorf("rxMBps = %v, want ~5 (10MB over 2s)", rx)
	}
	if tx < 0.9 || tx > 1.1 {
		t.Errorf("txMBps = %v, want ~1 (2MB over 2s)", tx)
	}
}

func TestNetRatesFromSamples_ZeroElapsed(t *testing.T) {
	sample := netSample{rxBytes: 100, txBytes: 100}
	if _, _, ok := netRatesFromSamples(sample, sample, 0); ok {
		t.Error("expected ok=false when elapsed is zero (would divide by zero)")
	}
}

func TestNetRatesFromSamples_CounterReset(t *testing.T) {
	// An interface flapping (down/up) resets its counters to zero — the
	// current sample reading LOWER than the previous one, not merely
	// equal. Must not underflow into a bogus multi-exabyte spike.
	prev := netSample{rxBytes: 1000, txBytes: 1000}
	cur := netSample{rxBytes: 10, txBytes: 10}
	if _, _, ok := netRatesFromSamples(prev, cur, time.Second); ok {
		t.Error("expected ok=false when a counter goes backwards (interface reset)")
	}
}

func TestReadNetSample_FileReadError(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()
	readFileFunc = func(string) ([]byte, error) { return nil, fmt.Errorf("permission denied") }

	if _, ok := readNetSample(); ok {
		t.Error("expected ok=false when the underlying file read fails")
	}
}

// ---------------------------------------------------------------------
// Storage I/O throughput
// ---------------------------------------------------------------------

func TestParseDiskStats(t *testing.T) {
	stats := "   8       0 sda 8231 149 659203 9160 4142 6448 725792 9308 0 8058 18471\n" +
		"   8       1 sda1 8199 149 659001 9130 4128 6448 725792 9296 0 8042 18426\n" +
		" 259       0 nvme0n1 12345 0 987654 1000 5432 0 654321 2000 0 1500 3000\n" +
		"259       1 nvme0n1p1 12000 0 900000 900 5000 0 600000 1800 0 1400 2800\n" +
		"   7       0 loop0 12 0 96 4 0 0 0 0 0 0 4\n" +
		" 253       0 dm-0 500 0 4000 100 200 0 1600 50 0 80 150\n"

	sample, ok := parseDiskStats(stats)
	if !ok {
		t.Fatal("parseDiskStats failed on well-formed input")
	}
	// Only sda and nvme0n1 (whole disks) count — sda1/nvme0n1p1 are
	// partitions, loop0 and dm-0 are excluded by wholeDiskRE entirely.
	wantRead := uint64(659203 + 987654)
	wantWrite := uint64(725792 + 654321)
	if sample.readSectors != wantRead {
		t.Errorf("readSectors = %d, want %d (sda + nvme0n1 only)", sample.readSectors, wantRead)
	}
	if sample.writeSectors != wantWrite {
		t.Errorf("writeSectors = %d, want %d (sda + nvme0n1 only)", sample.writeSectors, wantWrite)
	}
}

func TestParseDiskStats_OnlyPartitionsAndVirtual(t *testing.T) {
	stats := "   8       1 sda1 8199 149 659001 9130 4128 6448 725792 9296 0 8042 18426\n" +
		"   7       0 loop0 12 0 96 4 0 0 0 0 0 0 4\n"
	if _, ok := parseDiskStats(stats); ok {
		t.Error("expected ok=false when every device is a partition or virtual/loop device")
	}
}

func TestDiskRatesFromSamples(t *testing.T) {
	prev := diskSample{readSectors: 0, writeSectors: 0}
	// 20480 sectors * 512 bytes = 10MB read, over 2s -> 5MB/s.
	cur := diskSample{readSectors: 20480, writeSectors: 4096}
	read, write, ok := diskRatesFromSamples(prev, cur, 2*time.Second)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if read < 4.9 || read > 5.1 {
		t.Errorf("readMBps = %v, want ~5", read)
	}
	if write < 0.9 || write > 1.1 {
		t.Errorf("writeMBps = %v, want ~1 (4096 sectors = 2MB over 2s)", write)
	}
}

func TestDiskRatesFromSamples_CounterReset(t *testing.T) {
	prev := diskSample{readSectors: 1000, writeSectors: 1000}
	cur := diskSample{readSectors: 10, writeSectors: 10}
	if _, _, ok := diskRatesFromSamples(prev, cur, time.Second); ok {
		t.Error("expected ok=false when a counter goes backwards")
	}
}

func TestReadDiskSample_FileReadError(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()
	readFileFunc = func(string) ([]byte, error) { return nil, fmt.Errorf("permission denied") }

	if _, ok := readDiskSample(); ok {
		t.Error("expected ok=false when the underlying file read fails")
	}
}

// TestRunner_HostUtilization_NetDiskDeltaAcrossCalls proves network and
// disk rates behave the same way CPU% does: zero (not an error) on the
// first call, a real rate from the second call once a baseline exists —
// and, critically, that a failure in ONE of the three (CPU/net/disk) on
// a given call does not corrupt the other two's independently-tracked
// baselines. This is the net/disk analogue of
// TestRunner_HostUtilization_FailedReadDoesNotCorruptBaseline above,
// covering exactly the bug class this file's own Runner field comment
// warns about: sharing state across metrics that can fail independently.
func TestRunner_HostUtilization_NetDiskDeltaAcrossCalls(t *testing.T) {
	orig := readFileFunc
	defer func() { readFileFunc = orig }()

	netCall, diskCall := 0, 0
	readFileFunc = func(path string) ([]byte, error) {
		switch path {
		case "/proc/stat", "/proc/meminfo":
			return nil, fmt.Errorf("not relevant to this test")
		case "/proc/net/dev":
			netCall++
			switch netCall {
			case 1:
				return []byte("eth0: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"), nil
			case 2:
				// mid-run failure: must not corrupt the stored net baseline
				return nil, fmt.Errorf("transient")
			case 3:
				return []byte("eth0: 10485760 0 0 0 0 0 0 0 2097152 0 0 0 0 0 0 0\n"), nil // 10MB rx, 2MB tx
			default:
				return nil, fmt.Errorf("unexpected extra /proc/net/dev read")
			}
		case "/proc/diskstats":
			diskCall++
			switch diskCall {
			case 1, 2, 3:
				// disk read succeeds every call, independent of net's failure
				return []byte(fmt.Sprintf("8 0 sda 0 0 %d 0 0 0 %d 0 0 0 0\n", diskCall*20480, diskCall*4096)), nil
			default:
				return nil, fmt.Errorf("unexpected extra /proc/diskstats read")
			}
		default:
			return nil, fmt.Errorf("unexpected path %q", path)
		}
	}

	r := &Runner{}
	_, _, rx1, tx1, read1, write1 := r.hostUtilization()
	if rx1 != 0 || tx1 != 0 {
		t.Errorf("first call: net rates = (%v,%v), want (0,0) — no prior sample yet", rx1, tx1)
	}
	if read1 != 0 || write1 != 0 {
		t.Errorf("first call: disk rates = (%v,%v), want (0,0) — no prior sample yet", read1, write1)
	}

	// A small sleep between calls guarantees a non-zero wall-clock delta
	// for the rate calculations below — back-to-back calls with no sleep
	// can otherwise land on the same clock tick (coarse timer resolution),
	// which would make elapsed==0 and mask the very behavior under test.
	time.Sleep(5 * time.Millisecond)

	// Second call: net's read fails (netCall=2), disk's succeeds. Net
	// must report zero (no corrupted delta); disk must still compute a
	// real rate from its own independently-tracked baseline.
	_, _, rx2, tx2, read2, write2 := r.hostUtilization()
	if rx2 != 0 || tx2 != 0 {
		t.Errorf("second call (net read failed): net rates = (%v,%v), want (0,0)", rx2, tx2)
	}
	if read2 <= 0 || write2 <= 0 {
		t.Errorf("second call: disk rates = (%v,%v), want >0 — disk's baseline must be unaffected by net's failure", read2, write2)
	}
	if !r.haveLastNetSample {
		t.Error("net's baseline from call 1 must survive call 2's failed read")
	}

	time.Sleep(5 * time.Millisecond)

	// Third call: net succeeds again — its delta must be against the
	// SURVIVING call-1 baseline (10MB/2MB over the wall-clock gap since
	// call 1, not call 2, since call 2 never updated it), proving the
	// failure in between did not corrupt it.
	_, _, rx3, tx3, _, _ := r.hostUtilization()
	if rx3 <= 0 || tx3 <= 0 {
		t.Errorf("third call: net rates = (%v,%v), want >0 (diffed against the surviving call-1 baseline)", rx3, tx3)
	}
}
