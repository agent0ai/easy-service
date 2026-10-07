package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"github.com/example/easy-service/internal/process"
)

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
	if _, _, err := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); err != 0 {
		return true, err
	}
	argv := []string{config.Proot, "-0", "-r", config.Rootfs,
		"-b", "/proc", "-b", "/dev",
		"-b", config.DNS + ":/etc/resolv.conf!",
		"-b", config.Binary + ":/.easy-service!",
		"-w", "/app", "/.easy-service", "__runtime", "guest"}
	return true, syscall.Exec(config.Proot, argv, []string{"PATH=/usr/bin:/bin", "PROOT_TMP_DIR=" + config.Temp})
}
