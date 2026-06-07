package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestImageRootFSBootArgsIncludesEnvAndWorkdir(t *testing.T) {
	network := &firecrackerNetwork{
		GuestCIDR: "172.30.1.2/30",
		GatewayIP: "172.30.1.1",
		DNS:       "1.1.1.1",
	}
	args := imageRootFSBootArgs("env | sort", false, true, "/workspace", []string{
		"APP_ENV=prod",
		"PATH=/usr/local/bin:/usr/bin",
	}, "workspace-agent", network)

	if !strings.Contains(args, "orca.tty=1") {
		t.Fatalf("boot args missing tty flag: %s", args)
	}
	if !strings.Contains(args, "orca.workdir_b64="+base64.StdEncoding.EncodeToString([]byte("/workspace"))) {
		t.Fatalf("boot args missing encoded workdir: %s", args)
	}
	envText := "APP_ENV=prod\nPATH=/usr/local/bin:/usr/bin"
	if !strings.Contains(args, "orca.env_b64="+base64.StdEncoding.EncodeToString([]byte(envText))) {
		t.Fatalf("boot args missing encoded env: %s", args)
	}
	if !strings.Contains(args, "orca.user_b64="+base64.StdEncoding.EncodeToString([]byte("workspace-agent"))) {
		t.Fatalf("boot args missing encoded user: %s", args)
	}
	if !strings.Contains(args, "orca.net_ip=172.30.1.2/30") ||
		!strings.Contains(args, "orca.net_gateway=172.30.1.1") ||
		!strings.Contains(args, "orca.net_dns=1.1.1.1") {
		t.Fatalf("boot args missing network config: %s", args)
	}
}
