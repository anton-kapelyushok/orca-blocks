package main

import "testing"

func TestDockerImageConfigExtractsRuntimeMetadata(t *testing.T) {
	inspect := []byte(`[
	  {
	    "Config": {
	      "Env": ["PATH=/usr/local/bin:/usr/bin", "APP_ENV=prod"],
	      "WorkingDir": "/workspace",
	      "Entrypoint": ["/entrypoint.sh"],
	      "Cmd": ["serve", "--port", "8080"],
	      "User": "1000:1000"
	    }
	  }
	]`)

	cfg := dockerImageConfig(inspect)
	if cfg.WorkingDir != "/workspace" {
		t.Fatalf("working dir = %q", cfg.WorkingDir)
	}
	if cfg.User != "1000:1000" {
		t.Fatalf("user = %q", cfg.User)
	}
	if got, want := len(cfg.Env), 2; got != want {
		t.Fatalf("env len = %d, want %d", got, want)
	}
	if got, want := cfg.Entrypoint[0], "/entrypoint.sh"; got != want {
		t.Fatalf("entrypoint[0] = %q, want %q", got, want)
	}
	if got, want := cfg.Cmd[1], "--port"; got != want {
		t.Fatalf("cmd[1] = %q, want %q", got, want)
	}
}
