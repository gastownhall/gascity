//go:build linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	nativeCgroupRoot   = "/sys/fs/cgroup"
	nativeCgroup2Magic = 0x63677270
)

type linuxNativeCapacityHost struct{}

func newNativeCapacityHost() nativeCapacityHost { return linuxNativeCapacityHost{} }

func nativeBoundedRead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return nil, fmt.Errorf("native kernel input unavailable")
	}
	return data, nil
}

func (linuxNativeCapacityHost) Identity() (nativeKernelIdentity, error) {
	var identity nativeKernelIdentity
	boot, err := nativeBoundedRead("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return identity, err
	}
	identity.BootID = strings.TrimSpace(string(boot))
	identity.PIDNamespace, err = os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return identity, err
	}
	identity.CgroupNamespace, err = os.Readlink("/proc/self/ns/cgroup")
	if err != nil {
		return identity, err
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(nativeCgroupRoot, &fs); err != nil || uint64(fs.Type) != nativeCgroup2Magic {
		return identity, fmt.Errorf("native cgroup2 mount unavailable")
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(nativeCgroupRoot, &stat); err != nil {
		return identity, err
	}
	identity.CgroupMountDevice, identity.CgroupMountInode = uint64(stat.Dev), stat.Ino
	return identity, nil
}

func (linuxNativeCapacityHost) Resources(c nativeCapacityConfig) error {
	mem, err := nativeBoundedRead("/proc/meminfo")
	if err != nil {
		return err
	}
	available := uint64(0)
	found := false
	for _, line := range strings.Split(string(mem), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "MemAvailable:" {
			if found || len(fields) != 3 || fields[2] != "kB" {
				return fmt.Errorf("native memory evidence invalid")
			}
			available, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return fmt.Errorf("native memory evidence invalid")
			}
			found = true
		}
	}
	if !found || float64(available)/(1024*1024) < c.Thresholds.MemoryGiB {
		return fmt.Errorf("native memory headroom unavailable")
	}
	for _, resource := range []struct {
		name      string
		threshold float64
	}{{"memory", c.Thresholds.MemoryPSI}, {"io", c.Thresholds.IOPSI}} {
		data, err := nativeBoundedRead("/proc/pressure/" + resource.name)
		if err != nil {
			return err
		}
		value, err := nativeFullPSI(string(data))
		if err != nil || value >= resource.threshold {
			return fmt.Errorf("native pressure headroom unavailable")
		}
	}
	return nil
}

func nativeFullPSI(data string) (float64, error) {
	found, lineFound, value := false, false, float64(0)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "full" {
			continue
		}
		if lineFound {
			return 0, fmt.Errorf("native pressure evidence ambiguous")
		}
		lineFound = true
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "avg10=") {
				continue
			}
			var err error
			value, err = strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
			if found || err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return 0, fmt.Errorf("native pressure evidence invalid")
			}
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("native pressure evidence unavailable")
	}
	return value, nil
}

type nativeLimitedOutput struct {
	data     []byte
	overflow bool
}

func (b *nativeLimitedOutput) Write(data []byte) (int, error) {
	n := len(data)
	room := 65536 - len(b.data)
	if n > room {
		b.overflow = true
		data = data[:room]
	}
	b.data = append(b.data, data...)
	return n, nil
}

func nativeSystemctl(args ...string) ([]byte, error) {
	binary, err := exec.LookPath("systemctl")
	if err != nil || !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("native user manager unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"--user"}, args...)...)
	var out nativeLimitedOutput
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = &out, io.Discard, time.Second
	if err := cmd.Run(); err != nil || ctx.Err() != nil || out.overflow {
		return nil, fmt.Errorf("native user manager unavailable")
	}
	return out.data, nil
}

func (linuxNativeCapacityHost) Command(unit string) (runtime.CommandScope, error) {
	binary, err := exec.LookPath("systemd-run")
	if err != nil || !filepath.IsAbs(binary) {
		return runtime.CommandScope{}, fmt.Errorf("native scope launcher unavailable")
	}
	version, err := nativeSystemctl("show", "--property=Version", "--value")
	if err != nil || strings.TrimSpace(string(version)) == "" {
		return runtime.CommandScope{}, fmt.Errorf("native user manager unavailable")
	}
	major := strings.TrimSpace(string(version))
	for i, ch := range major {
		if ch < '0' || ch > '9' {
			major = major[:i]
			break
		}
	}
	n, err := strconv.Atoi(major)
	if err != nil || n < 254 {
		return runtime.CommandScope{}, fmt.Errorf("native scope launcher lacks literal argument support")
	}
	executable, err := os.Executable()
	if err != nil || !filepath.IsAbs(executable) {
		return runtime.CommandScope{}, fmt.Errorf("native entry executable unavailable")
	}
	return runtime.CommandScope{Launcher: binary, Unit: unit, EntryCommand: []string{executable}}, nil
}

func (host linuxNativeCapacityHost) SelfScope(unit string) (nativeScopeObservation, error) {
	data, err := nativeBoundedRead("/proc/self/cgroup")
	if err != nil {
		return nativeScopeObservation{}, err
	}
	group := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			if group != "" {
				return nativeScopeObservation{}, fmt.Errorf("native self cgroup ambiguous")
			}
			group = strings.TrimPrefix(line, "0::")
		}
	}
	if group == "" || filepath.Base(group) != unit {
		return nativeScopeObservation{}, fmt.Errorf("native self cgroup mismatch")
	}
	return host.observeGroup(nativeCapacitySeat{Unit: unit}, group)
}

func (linuxNativeCapacityHost) Observe(seat nativeCapacitySeat) (nativeScopeObservation, error) {
	var observed nativeScopeObservation
	group := seat.CgroupPath
	if group == "" {
		data, err := nativeSystemctl("show", "--property=ControlGroup", "--value", seat.Unit)
		if err != nil {
			return observed, err
		}
		group = strings.TrimSuffix(string(data), "\n")
		if group == "" {
			return observed, nil
		} // No positive witness; still occupied.
	}
	return linuxNativeCapacityHost{}.observeGroup(seat, group)
}

func (linuxNativeCapacityHost) observeGroup(seat nativeCapacitySeat, group string) (nativeScopeObservation, error) {
	var observed nativeScopeObservation
	if !strings.HasPrefix(group, "/") || filepath.Clean(group) != group || filepath.Base(group) != seat.Unit || strings.ContainsAny(group, "\x00\r\n") {
		return observed, fmt.Errorf("native scope path invalid")
	}
	dir := filepath.Join(nativeCgroupRoot, strings.TrimPrefix(group, "/"))
	var stat syscall.Stat_t
	if err := syscall.Stat(dir, &stat); err != nil {
		// This is a direct kernel removal witness for a previously populated
		// exact cgroup under the already-pinned boot/namespace/cgroup2 mount.
		// Linux cannot remove a cgroup with live descendants. A missing unit
		// from systemd, by itself, never reaches this branch with a known path.
		if errors.Is(err, os.ErrNotExist) && seat.CgroupPath != "" {
			observed.Empty = true
			return observed, nil
		}
		return observed, fmt.Errorf("native scope kernel evidence unavailable")
	}
	if seat.CgroupPath != "" && (uint64(stat.Dev) != seat.CgroupDevice || stat.Ino != seat.CgroupInode) {
		return observed, fmt.Errorf("native scope incarnation mismatch")
	}
	data, err := nativeBoundedRead(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return observed, err
	}
	seen := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "populated" {
			if seen || len(fields) != 2 || (fields[1] != "0" && fields[1] != "1") {
				return observed, fmt.Errorf("native scope populated evidence invalid")
			}
			seen = true
			observed.Populated, observed.Empty = fields[1] == "1", fields[1] == "0"
		}
	}
	if !seen {
		return observed, fmt.Errorf("native scope populated evidence unavailable")
	}
	observed.CgroupPath, observed.Device, observed.Inode = group, uint64(stat.Dev), stat.Ino
	return observed, nil
}
