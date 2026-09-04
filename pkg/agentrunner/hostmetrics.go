// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package agentrunner

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// readFileFunc is os.ReadFile by default, swapped out in tests to feed
// fixture content without a real /proc — the sandbox running this
// package's tests need not even be Linux.
var readFileFunc = os.ReadFile

// cpuSample is one /proc/stat aggregate "cpu " line's fields, in
// jiffies — kept between reportInventory calls so cpuPercentFromSamples
// can compute a DELTA. A single /proc/stat read gives only cumulative
// totals since boot, which is meaningless as a percentage on its own;
// genuine utilization needs two samples across a known time window, same
// technique top/htop use internally.
type cpuSample struct {
	idle, total uint64
}

// readCPUSample reads and parses /proc/stat's aggregate "cpu " line.
// ok=false if the file cannot be read or parsed — the caller treats that
// as "no utilization data this report", never as a reason to fail the
// whole inventory report (GPU/instance data must still go out
// regardless). File I/O is kept to this one thin wrapper so
// parseCPUStatLine below can be tested directly against fixture text —
// this package's tests do not assume a real /proc exists (the sandbox
// running them may not even be Linux).
func readCPUSample() (cpuSample, bool) {
	data, err := readFileFunc("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	return parseCPUStatLine(string(data))
}

// parseCPUStatLine extracts the aggregate "cpu " line from /proc/stat's
// full contents.
func parseCPUStatLine(statContents string) (cpuSample, bool) {
	for _, line := range strings.Split(statContents, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop the "cpu" label itself
		var total, idle uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			total += v
			// idle (field 3) and iowait (field 4) both count as "the CPU
			// was not doing work" — the standard definition top/htop use.
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return cpuSample{idle: idle, total: total}, true
	}
	return cpuSample{}, false
}

// cpuPercentFromSamples computes utilization between two /proc/stat
// samples taken at different times. ok=false only when the counters did
// not advance at all between them (totalDelta == 0), which would divide
// by zero — a real possibility if it is called twice in the same jiffy,
// not just a defensive check.
//
// Deliberately a pure function taking two already-read samples, rather
// than reading /proc/stat itself: whether a "previous sample" exists at
// all is caller state (Runner.lastCPUSample), and mixing "the read
// failed" with "there is no previous sample yet" into one bool, as an
// earlier version of this function did, meant a failed read could get
// silently stored as the next call's baseline — corrupting the NEXT
// delta with a comparison against a bogus zero sample. Keeping the read
// and the diff as separate steps (see Runner.hostUtilization, the only
// caller) makes it possible to update the stored sample ONLY when a read
// genuinely succeeded.
func cpuPercentFromSamples(prev, cur cpuSample) (percent float64, ok bool) {
	totalDelta := cur.total - prev.total
	idleDelta := cur.idle - prev.idle
	if totalDelta == 0 {
		return 0, false
	}
	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100, true
}

// readMemoryUsedGB reads and parses /proc/meminfo, returning (MemTotal -
// MemAvailable) in GB — the standard "actually in use" definition.
// MemAvailable (not MemFree) already accounts for reclaimable
// cache/buffers the kernel would hand back under real pressure, so it
// does not overstate usage the way MemFree alone would (a Linux box
// with plenty of headroom often shows MemFree near zero simply because
// the page cache filled it, which is not memory pressure). ok=false if
// the file cannot be read or either field is missing. See
// readCPUSample's own comment on why parsing is factored out
// separately (parseMemInfo) from the file read.
func readMemoryUsedGB() (usedGB float64, ok bool) {
	data, err := readFileFunc("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	return parseMemInfo(string(data))
}

// parseMemInfo extracts MemTotal/MemAvailable from /proc/meminfo's full
// contents and returns the difference in GB.
func parseMemInfo(meminfoContents string) (usedGB float64, ok bool) {
	var totalKB, availKB uint64
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(meminfoContents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, haveTotal = v, true
		case "MemAvailable:":
			availKB, haveAvail = v, true
		}
	}
	if !haveTotal || !haveAvail || totalKB < availKB {
		return 0, false
	}
	return float64(totalKB-availKB) / (1024.0 * 1024.0), true
}

// ---------------------------------------------------------------------
// Network throughput — same cumulative-counter-needs-a-delta shape as
// CPU above, but the delta is over WALL-CLOCK time (a byte rate), not
// jiffies, so the caller (Runner.hostUtilization) supplies the elapsed
// duration rather than it being implicit in the sample itself.
// ---------------------------------------------------------------------

// netSample is /proc/net/dev's rx/tx byte counters, summed across every
// "real" interface — see parseNetDev for what counts.
type netSample struct {
	rxBytes, txBytes uint64
}

// virtualIfacePrefixes excludes loopback and the container/CNI/VPN
// interfaces a GPU or home node commonly runs (Docker, Kubernetes CNIs,
// bridges, tunnels) — these either double-count traffic already counted
// on a physical interface, or carry no meaningful "this host's internet
// usage" signal. Best-effort, not exhaustive: an unusual CNI plugin's own
// naming convention could slip through uncounted, which undercounts
// rather than overcounts — the safer failure direction for a metric nobody
// bills against.
var virtualIfacePrefixes = []string{
	"lo", "veth", "docker", "br-", "cni", "flannel", "cali", "tun", "tap",
	"virbr", "vmnet", "lxc", "vnet",
}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// readNetSample reads and sums /proc/net/dev. See readCPUSample's own
// comment for why the file read and the parsing (parseNetDev) are kept
// separate.
func readNetSample() (netSample, bool) {
	data, err := readFileFunc("/proc/net/dev")
	if err != nil {
		return netSample{}, false
	}
	return parseNetDev(string(data))
}

// parseNetDev sums rx/tx bytes across every non-virtual interface listed
// in /proc/net/dev's full contents. Format is two header lines then one
// line per interface: "  iface: rx_bytes rx_packets ... tx_bytes
// tx_packets ..." (18 fields total after the interface name, 8 rx + 8
// tx). ok=false only when NOT ONE interface line parses — a single
// malformed/unrecognised line is skipped rather than failing the whole
// read, since one odd interface should not blind the report to every
// other one.
func parseNetDev(contents string) (netSample, bool) {
	var sample netSample
	var sawAny bool
	for _, line := range strings.Split(contents, "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue // header lines have no colon
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" || isVirtualIface(name) {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 16 {
			continue
		}
		rx, errRx := strconv.ParseUint(fields[0], 10, 64)
		tx, errTx := strconv.ParseUint(fields[8], 10, 64)
		if errRx != nil || errTx != nil {
			continue
		}
		sample.rxBytes += rx
		sample.txBytes += tx
		sawAny = true
	}
	return sample, sawAny
}

// netRatesFromSamples computes MB/s from two /proc/net/dev samples and
// the wall-clock time between them. ok=false when elapsed is not
// positive (nothing to divide by) or either counter went BACKWARDS — an
// interface reset (down/up, driver reload) resets its counters to zero,
// and treating that as "negative traffic" via uint64 underflow would
// produce a nonsense multi-exabyte spike instead of just skipping the
// one bad reading, same defensive shape as cpuPercentFromSamples' own
// totalDelta==0 guard.
func netRatesFromSamples(prev, cur netSample, elapsed time.Duration) (rxMBps, txMBps float64, ok bool) {
	if elapsed <= 0 || cur.rxBytes < prev.rxBytes || cur.txBytes < prev.txBytes {
		return 0, 0, false
	}
	secs := elapsed.Seconds()
	const mb = 1024.0 * 1024.0
	return float64(cur.rxBytes-prev.rxBytes) / mb / secs,
		float64(cur.txBytes-prev.txBytes) / mb / secs,
		true
}

// ---------------------------------------------------------------------
// Storage I/O throughput — same shape as network above: cumulative
// sector counters in /proc/diskstats, summed across whole-disk devices
// only, delta'd over wall-clock time into MB/s.
// ---------------------------------------------------------------------

// diskSample is /proc/diskstats' sector counters, summed across every
// whole-disk device — see parseDiskStats for what counts.
type diskSample struct {
	readSectors, writeSectors uint64
}

// wholeDiskRE matches physical whole-disk device names (sda, nvme0n1,
// vda, xvda, hda) and deliberately excludes their partitions (sda1,
// nvme0n1p1), loop devices, ram disks, device-mapper (dm-*) and md/RAID
// devices. Counting partitions alongside their parent disk would double
// the reading; dm-/md devices are layered ON TOP of physical disks this
// pattern already counts, so including them would double-count there
// too for the common case (LVM/RAID over local block devices) — the
// tradeoff is a network-attached or exotic block device outside this
// naming convention goes uncounted, the same "undercount over overcount"
// choice made in isVirtualIface above.
var wholeDiskRE = regexp.MustCompile(`^(sd[a-z]+|hd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme[0-9]+n[0-9]+)$`)

// readDiskSample reads and sums /proc/diskstats.
func readDiskSample() (diskSample, bool) {
	data, err := readFileFunc("/proc/diskstats")
	if err != nil {
		return diskSample{}, false
	}
	return parseDiskStats(string(data))
}

// parseDiskStats sums sectors read/written across every whole-disk
// device in /proc/diskstats' full contents. Each line is "major minor
// name reads_completed reads_merged sectors_read ms_reading
// writes_completed writes_merged sectors_written ..." — sectors_read is
// field index 5, sectors_written is field index 9 (0-indexed after
// major/minor/name), and /proc/diskstats always reports sectors in
// fixed 512-byte units regardless of the device's real physical sector
// size (a kernel convention, not this code's assumption). ok=false only
// when NOT ONE device line parses, same "one bad line skips, doesn't
// blind the read" posture as parseNetDev.
func parseDiskStats(contents string) (diskSample, bool) {
	var sample diskSample
	var sawAny bool
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		name := fields[2]
		if !wholeDiskRE.MatchString(name) {
			continue
		}
		read, errRead := strconv.ParseUint(fields[5], 10, 64)
		written, errWrite := strconv.ParseUint(fields[9], 10, 64)
		if errRead != nil || errWrite != nil {
			continue
		}
		sample.readSectors += read
		sample.writeSectors += written
		sawAny = true
	}
	return sample, sawAny
}

// diskSectorBytes is /proc/diskstats' fixed reporting unit — see
// parseDiskStats' own comment.
const diskSectorBytes = 512

// diskRatesFromSamples computes MB/s from two /proc/diskstats samples
// and the wall-clock time between them. Same guards as
// netRatesFromSamples: non-positive elapsed, or a counter that went
// backwards (a device disappearing and a different one taking the same
// name across a reboot-less hot-swap is the realistic version of this
// for disks, rare but not impossible) skip the reading rather than
// underflow into a nonsense spike.
func diskRatesFromSamples(prev, cur diskSample, elapsed time.Duration) (readMBps, writeMBps float64, ok bool) {
	if elapsed <= 0 || cur.readSectors < prev.readSectors || cur.writeSectors < prev.writeSectors {
		return 0, 0, false
	}
	secs := elapsed.Seconds()
	const mb = 1024.0 * 1024.0
	return float64(cur.readSectors-prev.readSectors) * diskSectorBytes / mb / secs,
		float64(cur.writeSectors-prev.writeSectors) * diskSectorBytes / mb / secs,
		true
}
