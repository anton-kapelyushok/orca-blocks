//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func main() {
	mountBasics()

	command := cmdlineValue("orca.command_b64")
	if command != "" {
		decoded, err := base64.StdEncoding.DecodeString(command)
		if err != nil {
			logf("invalid command encoding: %v", err)
			reboot(2)
			return
		}
		command = string(decoded)
	} else {
		command = `echo orca image-rootfs ok`
	}

	workdir := decodeOptional("orca.workdir_b64")
	envText := decodeOptional("orca.env_b64")
	env := os.Environ()
	if envText != "" {
		for _, line := range strings.Split(envText, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				env = append(env, line)
			}
		}
	}

	logf("started image-rootfs command=%q", command)
	cmd := exec.Command("/bin/sh", "-lc", command)
	cmd.Env = env
	if workdir != "" {
		cmd.Dir = workdir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	emitCaptured("stdout", stdout.String())
	emitCaptured("stderr", stderr.String())
	exitCode := 0
	if err != nil {
		exitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}

	sync()
	if exitCode == 0 {
		logf("command ok exit_code=0")
		logf("image-rootfs ok")
	} else {
		logf("command failed exit_code=%d", exitCode)
	}
	reboot(exitCode)
}

type consoleWriter struct{}

func (consoleWriter) Write(p []byte) (int, error) {
	if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
		defer f.Close()
		_, _ = f.Write(p)
	}
	return len(p), nil
}

func mountBasics() {
	_ = os.MkdirAll("/proc", 0o755)
	_ = os.MkdirAll("/sys", 0o755)
	_ = os.MkdirAll("/dev", 0o755)
	_ = os.MkdirAll("/run", 0o755)
	_ = os.MkdirAll("/tmp", 0o1777)
	_ = os.MkdirAll("/sys/fs/cgroup", 0o755)

	_ = syscall.Mount("proc", "/proc", "proc", 0, "")
	_ = syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	_ = syscall.Mount("devtmpfs", "/dev", "devtmpfs", 0, "")
	_ = syscall.Mount("tmpfs", "/run", "tmpfs", 0, "")
	_ = syscall.Mount("tmpfs", "/tmp", "tmpfs", 0, "")
	_ = syscall.Mount("none", "/sys/fs/cgroup", "cgroup2", 0, "")
}

func cmdlineValue(key string) string {
	raw, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, arg := range strings.Fields(string(raw)) {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func decodeOptional(key string) string {
	value := cmdlineValue(key)
	if value == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		logf("invalid %s encoding: %v", key, err)
		return ""
	}
	return string(decoded)
}

func logf(format string, args ...any) {
	line := fmt.Sprintf("orca-init: "+format+"\n", args...)
	if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
}

func emitCaptured(stream, text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		writeConsole(fmt.Sprintf("orca-%s: %s\n", stream, line))
	}
}

func writeConsole(line string) {
	if f, err := os.OpenFile("/dev/console", os.O_WRONLY, 0); err == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
}

func sync() {
	syscall.Sync()
}

func reboot(exitCode int) {
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	os.Exit(exitCode)
}
