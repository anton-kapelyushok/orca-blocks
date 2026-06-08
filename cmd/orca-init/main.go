//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

var buildTimeUTC = "unknown"

func main() {
	logf("build_time_utc=%s", buildTimeUTC)
	enableChildReaper()
	mountBasics()
	configureContainerIdentity()
	configureLoopback()
	configureNetwork()

	if cmdlineValue("orca.tty") == "1" {
		runTTY()
		return
	}

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
	env := imageEnv()
	credential, username := imageCredential()
	env = imageUserEnv(env, username)

	logf("started image-rootfs command=%q workdir=%q user=%q", command, workdir, username)
	cmd := exec.Command("/bin/sh", "-lc", command)
	cmd.Env = env
	applyCredential(cmd, credential)
	if workdir != "" {
		cmd.Dir = workdir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	reapAdoptedChildren()
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

func runTTY() {
	env := imageEnv()
	env = append(env, "TERM=xterm")
	credential, username := imageCredential()
	env = imageUserEnv(env, username)
	workdir := decodeOptional("orca.workdir_b64")
	command := cmdlineValue("orca.command_b64")
	if command != "" {
		decoded, err := base64.StdEncoding.DecodeString(command)
		if err != nil {
			logf("invalid command encoding: %v", err)
			reboot(2)
			return
		}
		command = string(decoded)
	}
	logf("tty ready")
	console, err := os.OpenFile("/dev/console", os.O_RDWR, 0)
	if err != nil {
		logf("open console failed: %v", err)
		reboot(2)
		return
	}
	defer console.Close()

	var cmd *exec.Cmd
	if command == "" {
		cmd = exec.Command("/bin/sh", "-i")
	} else {
		logf("started env command=%q workdir=%q user=%q", command, workdir, username)
		env = append(env, "ORCA_ENV_COMMAND="+command)
		cmd = exec.Command("/bin/sh", "-lc", `(/bin/sh -lc "$ORCA_ENV_COMMAND" </dev/null 2>&1 | while IFS= read -r line; do printf 'orca-bg: %s\n' "$line"; done) & exec /bin/sh -i`)
	}
	cmd.Env = env
	applyCredential(cmd, credential)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Stdin = console
	cmd.Stdout = console
	cmd.Stderr = console
	err = cmd.Run()
	reapAdoptedChildren()
	exitCode := 0
	if err != nil {
		exitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	sync()
	if exitCode == 0 {
		logf("tty closed exit_code=0")
		logf("image-rootfs ok")
	} else {
		logf("tty closed exit_code=%d", exitCode)
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

func configureContainerIdentity() {
	const hostname = "orca-env"

	_ = syscall.Sethostname([]byte(hostname))
	_ = os.WriteFile("/etc/hostname", []byte(hostname+"\n"), 0o644)
	_ = os.WriteFile("/etc/hosts", []byte(strings.Join([]string{
		"127.0.0.1 localhost",
		"127.0.1.1 " + hostname,
		"",
	}, "\n")), 0o644)
}

func configureLoopback() {
	link, err := netlink.LinkByName("lo")
	if err != nil {
		logf("loopback setup failed: find lo: %v", err)
		return
	}
	addr, err := netlink.ParseAddr("127.0.0.1/8")
	if err != nil {
		logf("loopback setup failed: parse addr: %v", err)
		return
	}
	if err := netlink.AddrAdd(link, addr); err != nil && !strings.Contains(err.Error(), "file exists") {
		logf("loopback setup failed: addr add: %v", err)
		return
	}
	if err := netlink.LinkSetUp(link); err != nil {
		logf("loopback setup failed: link up: %v", err)
		return
	}
}

func configureNetwork() {
	ipCIDR := cmdlineValue("orca.net_ip")
	gateway := cmdlineValue("orca.net_gateway")
	dns := cmdlineValue("orca.net_dns")
	if ipCIDR == "" || gateway == "" {
		return
	}
	var link netlink.Link
	var err error
	for i := 0; i < 20; i++ {
		link, err = netlink.LinkByName("eth0")
		if err == nil {
			break
		}
		sleepMillis(50)
	}
	if err != nil {
		logf("network setup failed: find eth0: %v", err)
		return
	}
	addr, err := netlink.ParseAddr(ipCIDR)
	if err != nil {
		logf("network setup failed: parse addr: %v", err)
		return
	}
	gw := net.ParseIP(gateway)
	if gw == nil {
		logf("network setup failed: parse gateway: %s", gateway)
		return
	}
	if addrs, err := netlink.AddrList(link, netlink.FAMILY_V4); err == nil {
		for _, existing := range addrs {
			_ = netlink.AddrDel(link, &existing)
		}
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		logf("network setup failed: addr add: %v", err)
		return
	}
	if err := netlink.LinkSetUp(link); err != nil {
		logf("network setup failed: link up: %v", err)
		return
	}
	if err := netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: gw}); err != nil && !strings.Contains(err.Error(), "file exists") {
		logf("network setup failed: route add: %v", err)
		return
	}
	if dns != "" {
		_ = os.WriteFile("/etc/resolv.conf", []byte("nameserver "+dns+"\n"), 0o644)
	}
	logf("network ready ip=%s gateway=%s dns=%s", ipCIDR, gateway, dns)
}

func sleepMillis(ms int) {
	tv := syscall.NsecToTimeval(int64(ms) * int64(time.Millisecond))
	_, _ = syscall.Select(0, nil, nil, nil, &tv)
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

func imageEnv() []string {
	env := os.Environ()
	envText := decodeOptional("orca.env_b64")
	if envText == "" {
		return env
	}
	for _, line := range strings.Split(envText, "\n") {
		if line != "" {
			env = append(env, line)
		}
	}
	return env
}

type passwdEntry struct {
	Name string
	UID  uint32
	GID  uint32
	Home string
}

func imageCredential() (*syscall.Credential, string) {
	spec := decodeOptional("orca.user_b64")
	if spec == "" {
		return nil, ""
	}
	entry, gid, err := resolveUserSpec(spec)
	if err != nil {
		logf("user setup failed for %q: %v", spec, err)
		return nil, ""
	}
	groups := supplementaryGroups(entry.Name, gid)
	return &syscall.Credential{Uid: entry.UID, Gid: gid, Groups: groups}, entry.Name
}

func resolveUserSpec(spec string) (passwdEntry, uint32, error) {
	parts := strings.SplitN(spec, ":", 2)
	entry, err := resolvePasswd(parts[0])
	if err != nil {
		return passwdEntry{}, 0, err
	}
	gid := entry.GID
	if len(parts) == 2 && parts[1] != "" {
		gid, err = resolveGroup(parts[1])
		if err != nil {
			return passwdEntry{}, 0, err
		}
	}
	return entry, gid, nil
}

func resolvePasswd(value string) (passwdEntry, error) {
	entries := passwdEntries()
	if id, err := strconv.ParseUint(value, 10, 32); err == nil {
		for _, entry := range entries {
			if entry.UID == uint32(id) {
				return entry, nil
			}
		}
		return passwdEntry{Name: value, UID: uint32(id), GID: uint32(id)}, nil
	}
	for _, entry := range entries {
		if entry.Name == value {
			return entry, nil
		}
	}
	return passwdEntry{}, fmt.Errorf("user not found")
}

func passwdEntries() []passwdEntry {
	raw, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return nil
	}
	var out []passwdEntry
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		uid, uidErr := strconv.ParseUint(fields[2], 10, 32)
		gid, gidErr := strconv.ParseUint(fields[3], 10, 32)
		if uidErr != nil || gidErr != nil {
			continue
		}
		out = append(out, passwdEntry{Name: fields[0], UID: uint32(uid), GID: uint32(gid), Home: fields[5]})
	}
	return out
}

func resolveGroup(value string) (uint32, error) {
	if id, err := strconv.ParseUint(value, 10, 32); err == nil {
		return uint32(id), nil
	}
	raw, err := os.ReadFile("/etc/group")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 3 || fields[0] != value {
			continue
		}
		id, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return 0, err
		}
		return uint32(id), nil
	}
	return 0, fmt.Errorf("group not found")
}

func supplementaryGroups(username string, primaryGID uint32) []uint32 {
	seen := map[uint32]struct{}{primaryGID: {}}
	groups := []uint32{primaryGID}
	raw, err := os.ReadFile("/etc/group")
	if err != nil || username == "" {
		return groups
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		members := strings.Split(fields[3], ",")
		found := false
		for _, member := range members {
			if member == username {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		id, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			continue
		}
		gid := uint32(id)
		if _, ok := seen[gid]; ok {
			continue
		}
		seen[gid] = struct{}{}
		groups = append(groups, gid)
	}
	return groups
}

func imageUserEnv(env []string, username string) []string {
	if username == "" {
		return env
	}
	env = appendEnvIfMissing(env, "USER", username)
	env = appendEnvIfMissing(env, "LOGNAME", username)
	return env
}

func appendEnvIfMissing(env []string, key, value string) []string {
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return env
		}
	}
	return append(env, prefix+value)
}

func applyCredential(cmd *exec.Cmd, credential *syscall.Credential) {
	if credential == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
}

func enableChildReaper() {
	const prSetChildSubreaper = 36
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, uintptr(prSetChildSubreaper), 1, 0, 0, 0, 0)
	if errno != 0 {
		logf("child reaper setup failed: %v", errno)
		return
	}
	logf("child reaper ready")
}

func reapAdoptedChildren() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 {
			if err != nil && err != syscall.ECHILD {
				logf("child reaper wait failed: %v", err)
			}
			return
		}
		logf("reaped child pid=%d status=%s", pid, waitStatusString(status))
	}
}

func waitStatusString(status syscall.WaitStatus) string {
	switch {
	case status.Exited():
		return fmt.Sprintf("exit:%d", status.ExitStatus())
	case status.Signaled():
		return fmt.Sprintf("signal:%d", status.Signal())
	case status.Stopped():
		return fmt.Sprintf("stopped:%d", status.StopSignal())
	default:
		return fmt.Sprintf("raw:%d", status)
	}
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
