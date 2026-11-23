package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/example/easy-service/internal/process"
)

const (
	prSetSeccomp      = 22
	prSetNoNewPrivs   = 38
	seccompModeFilter = 2
	seccompRetErrno   = 0x00050000
	seccompRetAllow   = 0x7fff0000

	// Linux audit architectures and syscall numbers for amd64 and arm64.
	auditArchX86_64  = 0xc000003e
	auditArchAArch64 = 0xc00000b7
	sysIOUringSetup  = 425
	sysOpenat2       = 437
)

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	len    uint16
	filter *sockFilter
}

// installSeccompFallback denies syscalls that PRoot 5.5 does not translate
// with ENOSYS so standard libraries cleanly fall back to openat/epoll.
func installSeccompFallback() error {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = auditArchX86_64
	case "arm64":
		arch = auditArchAArch64
	default:
		return nil
	}
	const (
		bpfLD  = 0x00
		bpfJMP = 0x05
		bpfRET = 0x06
		bpfW   = 0x00
		bpfABS = 0x20
		bpfJEQ = 0x10
		bpfK   = 0x00

		seccompDataNr   = 0
		seccompDataArch = 4
	)
	filter := []sockFilter{
		// Verify native architecture; pass other ABIs through untouched.
		{code: bpfLD | bpfW | bpfABS, k: seccompDataArch},
		{code: bpfJMP | bpfJEQ | bpfK, jt: 0, jf: 4, k: arch},
		// Load syscall number and return ENOSYS for io_uring_setup and openat2.
		{code: bpfLD | bpfW | bpfABS, k: seccompDataNr},
		{code: bpfJMP | bpfJEQ | bpfK, jt: 1, jf: 0, k: sysIOUringSetup},
		{code: bpfJMP | bpfJEQ | bpfK, jt: 0, jf: 1, k: sysOpenat2},
		{code: bpfRET | bpfK, k: seccompRetErrno | uint32(syscall.ENOSYS)},
		{code: bpfRET | bpfK, k: seccompRetAllow},
	}
	prog := sockFprog{
		len:    uint16(len(filter)),
		filter: &filter[0],
	}
	if _, _, err := syscall.RawSyscall6(
		syscall.SYS_PRCTL,
		prSetSeccomp,
		seccompModeFilter,
		uintptr(unsafe.Pointer(&prog)),
		0, 0, 0,
	); err != 0 {
		return err
	}
	return nil
}

type launchConfig struct {
	UID                                       uint32
	Rootfs, DNS, Temp, Proot, Binary, Command string
	Env                                       []string
}

// Child runs only the internal re-execution path. The descriptor contains app
// settings, never the supervisor's environment. The guest receives no descriptor.
func Child(args []string) (bool, error) {
	if len(args) == 0 || args[0] != "__runtime" {
		return false, nil
	}
	if len(args) != 2 || (args[1] != "launch" && args[1] != "guest") {
		return true, fmt.Errorf("invalid runtime invocation")
	}
	file := os.NewFile(3, "instance configuration")
	var config launchConfig
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return true, err
	}
	runtime.LockOSThread()
	if args[1] == "guest" {
		file.Close()
		// PRoot deliberately reports UID 0 to image programs. Verify the real
		// kernel ownership through /proc rather than that apparent identity.
		sample, err := process.SampleUser(config.UID)
		owned := false
		for _, pid := range sample.PIDs {
			owned = owned || pid == os.Getpid()
		}
		if err != nil || !owned {
			return true, fmt.Errorf("workload privileges were not dropped")
		}
		if err := os.Chdir("/app"); err != nil {
			return true, err
		}
		return true, syscall.Exec("/bin/sh", []string{"/bin/sh", "-c", config.Command}, config.Env)
	}
	if os.Geteuid() != 0 || config.UID < process.MinWorkloadUID {
		return true, fmt.Errorf("invalid runtime privileges")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return true, err
	}
	if err := syscall.Setgroups(nil); err != nil {
		return true, err
	}
	if err := syscall.Setgid(int(config.UID)); err != nil {
		return true, err
	}
	if err := syscall.Setuid(int(config.UID)); err != nil {
		return true, err
	}
	// no_new_privs prevents image setuid binaries and file capabilities from
	// elevating the real Linux identity, including through PRoot's fake root.
	if _, _, err := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); err != 0 {
		return true, err
	}
	if err := installSeccompFallback(); err != nil {
		return true, fmt.Errorf("install seccomp fallback: %w", err)
	}
	argv := []string{config.Proot, "-0", "-r", config.Rootfs,
		"-b", "/proc", "-b", "/dev",
		"-b", config.DNS + ":/etc/resolv.conf!",
		"-b", config.Binary + ":/.easy-service!",
		"-w", "/app", "/.easy-service", "__runtime", "guest"}
	return true, syscall.Exec(config.Proot, argv, []string{"PATH=/usr/bin:/bin", "PROOT_TMP_DIR=" + config.Temp})
}
