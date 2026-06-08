#!/usr/bin/env python3
import argparse
import shlex
import subprocess
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path


DEMO_DIR = Path(__file__).resolve().parent
REMOTE_DIR = DEMO_DIR / "remote"
BOLD = "\033[1m"
DIM = "\033[2m"
REVERSE = "\033[7m"
RESET = "\033[0m"


@dataclass(frozen=True)
class Config:
    node1: str
    node2: str
    registry: str
    node2_registry: str
    repo: str
    base_tag: str
    run_id: str
    derived_tag: str
    touch_path: str
    timeout_seconds: int
    yes: bool
    dry_run: bool

    @property
    def base_image_node1(self) -> str:
        return f"{self.registry}/{self.repo}:{self.base_tag}"

    @property
    def base_image_node2(self) -> str:
        return f"{self.node2_registry}/{self.repo}:{self.base_tag}"

    @property
    def derived_image_node1(self) -> str:
        return f"{self.registry}/{self.repo}:{self.derived_tag}"

    @property
    def derived_image_node2(self) -> str:
        return f"{self.node2_registry}/{self.repo}:{self.derived_tag}"


@dataclass(frozen=True)
class RemoteStep:
    label: str
    target: str
    script_name: str
    extra_env: dict[str, str] | None = None


@dataclass(frozen=True)
class ScenarioStep:
    title: str
    short_title: str
    remote_steps: list[RemoteStep]


def sh(value: str) -> str:
    return shlex.quote(value)


def utc_stamp() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def print_config(config: Config) -> None:
    print(
        f"""Demo configuration:
  node1:              {config.node1}
  node2:              {config.node2}
  registry:           {config.registry}
  node2 registry ref: {config.node2_registry}
  base node1 image:   {config.base_image_node1}
  base node2 image:   {config.base_image_node2}
  derived node1 ref:  {config.derived_image_node1}
  derived node2 ref:  {config.derived_image_node2}
  touched file:       {config.touch_path}
  run id:             {config.run_id}
"""
    )


def base_env(config: Config) -> dict[str, str]:
    return {
        "RUN_ID": config.run_id,
        "REGISTRY_HOST": config.registry,
        "NODE2_REGISTRY_HOST": config.node2_registry,
        "REPO": config.repo,
        "BASE_TAG": config.base_tag,
        "DERIVED_TAG": config.derived_tag,
        "TOUCH_PATH": config.touch_path,
        "TIMEOUT_SECONDS": str(config.timeout_seconds),
    }


def load_remote_script(name: str) -> str:
    path = REMOTE_DIR / name
    return path.read_text(encoding="utf-8")


def extract_demo_commands(script: str) -> list[str]:
    commands = []
    for line in script.splitlines():
        stripped = line.strip()
        if stripped.startswith("# DEMO-CMD:"):
            commands.append(stripped.removeprefix("# DEMO-CMD:").strip())
    return commands


def interpolate_command(command: str, env: dict[str, str]) -> str:
    result = command
    derived = {
        "CTR": "/opt/overlaybd/snapshotter/ctr",
        "NS": "moby",
        "SNAPSHOTTER": "overlaybd",
        "CONFIG": "/etc/overlaybd-snapshotter/config.json",
        "SNAPSHOT_ROOT": "/var/lib/containerd/io.containerd.snapshotter.v1.overlaybd",
        "WORK": f"/root/orca-overlaybd-demo-{env['RUN_ID']}",
        "COMMIT_DIR": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit",
        "ENV_FILE": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/mutable.env",
        "DIFF_TAR": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit/demo-touch-upperdir-diff.tar",
        "APPLY_DATA": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit/demo-touch-writable-data",
        "APPLY_INDEX": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit/demo-touch-writable-index",
        "APPLY_CONFIG": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit/demo-touch-apply-config.v1.json",
        "APPLY_RESULT": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit/demo-touch-apply-result.log",
        "COMMIT_OBD": f"/root/orca-overlaybd-demo-{env['RUN_ID']}/commit/demo-touch-commit.obd",
        "REGISTRY_URL": f"http://{env['REGISTRY_HOST']}",
        "BASE_REF": f"{env['REGISTRY_HOST']}/{env['REPO']}:{env['BASE_TAG']}",
        "DERIVED_REF": f"{env['REGISTRY_HOST']}/{env['REPO']}:{env['DERIVED_TAG']}",
        "TOUCH_DIR": str(Path(env["TOUCH_PATH"]).parent),
    }
    if "CONTAINER_NAME" in env:
        derived["NAME"] = env["CONTAINER_NAME"]
        derived["fifo"] = f"/tmp/{env['CONTAINER_NAME']}.fifo"
        derived["LOG"] = f"/root/orca-overlaybd-demo-{env['RUN_ID']}/{env['CONTAINER_NAME']}.log"
    display_env = {**env, **derived}
    for key in sorted(display_env, key=len, reverse=True):
        value = display_env[key]
        result = result.replace(f'"${key}"', sh(value))
        result = result.replace(f"'${key}'", sh(value))
        result = result.replace(f"${{{key}}}", value)
        result = result.replace(f"${key}", value)
    return result


def render_env(env: dict[str, str]) -> str:
    return " ".join(f"{key}={sh(value)}" for key, value in env.items())


def remote_shell(target: str, env_prefix: str) -> str:
    if target.startswith("root@"):
        return f"{env_prefix} bash -se"
    return f"sudo env {env_prefix} bash -se"


def build_remote(config: Config, step: RemoteStep) -> tuple[dict[str, str], str, str]:
    env = base_env(config)
    if step.extra_env:
        env.update(step.extra_env)
    env_prefix = render_env(env)
    script = load_remote_script(step.script_name).strip()
    return env, env_prefix, script


def preview_remote(config: Config, step: RemoteStep) -> None:
    env, env_prefix, script = build_remote(config, step)
    shell = remote_shell(step.target, env_prefix)
    print(f"\nWill execute {step.label}: ssh {step.target} '{shell}' < {REMOTE_DIR / step.script_name}")
    demo_commands = extract_demo_commands(script)
    if demo_commands:
        print("Main commands:")
        for command in demo_commands:
            print(f"{step.label}$ {interpolate_command(command, env)}")


def execute_remote(config: Config, step: RemoteStep) -> None:
    env, env_prefix, script = build_remote(config, step)
    shell = remote_shell(step.target, env_prefix)
    print(f"\nExecuting {step.label}: ssh {step.target} '{shell}' < {REMOTE_DIR / step.script_name}")
    print("Logs follow from the remote command output.\n")

    if config.dry_run:
        print("dry-run: skipped")
        return

    command = [
        "ssh",
        "-o",
        "StrictHostKeyChecking=no",
        step.target,
        shell,
    ]
    subprocess.run(command, input=script + "\n", text=True, check=True)


def print_step_overview(scenario: list[ScenarioStep], current_index: int) -> None:
    print("Scenario:")
    for index, step in enumerate(scenario, start=1):
        if index == current_index:
            print(f"{BOLD}> {index}. {step.short_title} <{RESET}")
        else:
            print(f"{DIM}- {index}. {step.short_title}{RESET}")


def run_step(
    config: Config,
    scenario: list[ScenarioStep],
    current_index: int,
) -> None:
    step_def = scenario[current_index - 1]
    message = f"Step {current_index}/{len(scenario)}: {step_def.title}"
    print(f"\n{BOLD}{REVERSE}[{utc_stamp()}] {message}{RESET}")
    print_step_overview(scenario, current_index)
    for step in step_def.remote_steps:
        preview_remote(config, step)
    if config.yes:
        print("--yes set, continuing")
    else:
        input("Press Enter to execute this step, or Ctrl-C to stop: ")
    start = time.monotonic()
    for step in step_def.remote_steps:
        execute_remote(config, step)
    elapsed_ms = int((time.monotonic() - start) * 1000)
    print(f"\n{DIM}local step wall time, including SSH and remote cleanup: {elapsed_ms} ms{RESET}")


def parse_args() -> Config:
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    parser = argparse.ArgumentParser(description="Two-node OverlayBD JB Workspace demo")
    parser.add_argument("--node1", default="anton.kapeliushok@104.155.88.61")
    parser.add_argument("--node2", default="root@178.128.247.74")
    parser.add_argument("--registry", default="178.128.247.74:5000")
    parser.add_argument("--node2-registry", default="127.0.0.1:5000")
    parser.add_argument("--repo", default="orca/overlaybd-jb-real")
    parser.add_argument("--base-tag", default="obd-jb-real-sysbox-20260608T171059Z")
    parser.add_argument("--run-id", default=run_id)
    parser.add_argument("--derived-tag")
    parser.add_argument("--touch-path", default="/home/workspace-agent/poupa-demo.txt")
    parser.add_argument("--timeout-seconds", type=int, default=180)
    parser.add_argument("--yes", action="store_true", help="do not pause between steps")
    parser.add_argument("--dry-run", action="store_true", help="print commands without running SSH")
    args = parser.parse_args()
    derived_tag = args.derived_tag or f"demo-touch-{args.run_id}"
    return Config(
        node1=args.node1,
        node2=args.node2,
        registry=args.registry,
        node2_registry=args.node2_registry,
        repo=args.repo,
        base_tag=args.base_tag,
        run_id=args.run_id,
        derived_tag=derived_tag,
        touch_path=args.touch_path,
        timeout_seconds=args.timeout_seconds,
        yes=args.yes,
        dry_run=args.dry_run,
    )


def main() -> int:
    config = parse_args()
    print_config(config)
    scenario = [
        ScenarioStep(
            "clean up demo environments on both nodes",
            "cleanup",
            [
                RemoteStep("node1", config.node1, "cleanup.sh"),
                RemoteStep("node2", config.node2, "cleanup.sh"),
            ],
        ),
        ScenarioStep(
            "run JB workspace on node1 until Join URL",
            "node1 base run",
            [
                RemoteStep(
                    "node1",
                    config.node1,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.base_image_node1,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-node1-base-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "run JB workspace on node2 until Join URL",
            "node2 base run",
            [
                RemoteStep(
                    "node2",
                    config.node2,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.base_image_node2,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-node2-base-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "warm-run JB workspace on node1 until Join URL",
            "node1 warm base run",
            [
                RemoteStep(
                    "node1",
                    config.node1,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.base_image_node1,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-node1-warm-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "touch a file in a node1 overlayfs upperdir run",
            "node1 touch file",
            [
                RemoteStep(
                    "node1",
                    config.node1,
                    "mutable-touch.sh",
                    {
                        "IMAGE_REF": config.base_image_node1,
                        "CONTAINER_NAME": f"demo-jb-node1-mutable-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "export node1 overlay upperdir and push derived image",
            "commit derived image",
            [RemoteStep("node1", config.node1, "commit-snapshot.sh")],
        ),
        ScenarioStep(
            "run new committed image on node2 until Join URL",
            "node2 derived run",
            [
                RemoteStep(
                    "node2",
                    config.node2,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.derived_image_node2,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-node2-derived-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "verify touched file is present in node2 committed image",
            "verify touched file",
            [
                RemoteStep(
                    "node2",
                    config.node2,
                    "verify-touch.sh",
                    {
                        "IMAGE_REF": config.derived_image_node2,
                        "CONTAINER_NAME": f"demo-jb-node2-verify-{config.run_id}",
                    },
                )
            ],
        ),
    ]

    try:
        for current_index in range(1, len(scenario) + 1):
            run_step(config, scenario, current_index)

    except KeyboardInterrupt:
        print("\nInterrupted by user.", file=sys.stderr)
        return 130

    print("\nDemo complete")
    print("Derived image:")
    print(f"  node1/global: {config.derived_image_node1}")
    print(f"  node2/local:  {config.derived_image_node2}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
