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
    master: str
    slave: str
    registry: str
    master_registry: str
    repo: str
    base_tag: str
    run_id: str
    derived_tag: str
    project_dir: str
    timeout_seconds: int
    yes: bool
    dry_run: bool
    show_commands: bool

    @property
    def base_image_master(self) -> str:
        return f"{self.master_registry}/{self.repo}:{self.base_tag}"

    @property
    def base_image_slave(self) -> str:
        return f"{self.registry}/{self.repo}:{self.base_tag}"

    @property
    def derived_image_master(self) -> str:
        return f"{self.master_registry}/{self.repo}:{self.derived_tag}"

    @property
    def derived_image_slave(self) -> str:
        return f"{self.registry}/{self.repo}:{self.derived_tag}"

    @property
    def jar_path(self) -> str:
        return f"{self.project_dir}/target/spring-petclinic-4.0.0-SNAPSHOT.jar"

    @property
    def tar_path(self) -> str:
        return f"{self.project_dir}.tar.gz"


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
  master:             {config.master}
  slave:              {config.slave}
  registry:           {config.registry}
  master registry ref:{config.master_registry}
  base master image:  {config.base_image_master}
  base slave image:   {config.base_image_slave}
  derived master ref: {config.derived_image_master}
  derived slave ref:  {config.derived_image_slave}
  project dir:        {config.project_dir}
  jar path:           {config.jar_path}
  tar path:           {config.tar_path}
  run id:             {config.run_id}
"""
    )


def base_env(config: Config) -> dict[str, str]:
    return {
        "RUN_ID": config.run_id,
        "REGISTRY_HOST": config.registry,
        "MASTER_REGISTRY_HOST": config.master_registry,
        "NODE2_REGISTRY_HOST": config.master_registry,
        "REPO": config.repo,
        "BASE_TAG": config.base_tag,
        "DERIVED_TAG": config.derived_tag,
        "TOUCH_PATH": config.jar_path,
        "PROJECT_DIR": config.project_dir,
        "TAR_PATH": config.tar_path,
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
        "PROJECT_PARENT": str(Path(env["PROJECT_DIR"]).parent),
        "PROJECT_NAME": Path(env["PROJECT_DIR"]).name,
        "JAR_PATH": f"{env['PROJECT_DIR']}/target/spring-petclinic-4.0.0-SNAPSHOT.jar",
        "TAR_PATH": f"{env['PROJECT_DIR']}.tar.gz",
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
    print(f"\nWill run {step.script_name} on {step.label} ({step.target})")
    if not config.show_commands:
        return

    print(f"SSH: ssh {step.target} '{shell}' < {REMOTE_DIR / step.script_name}")
    demo_commands = extract_demo_commands(script)
    if demo_commands:
        print("Main commands:")
        for command in demo_commands:
            print(f"{step.label}$ {interpolate_command(command, env)}")


def execute_remote(config: Config, step: RemoteStep) -> None:
    env, env_prefix, script = build_remote(config, step)
    shell = remote_shell(step.target, env_prefix)
    if config.show_commands:
        print(f"\nExecuting {step.label}: ssh {step.target} '{shell}' < {REMOTE_DIR / step.script_name}")
    else:
        print(f"\nExecuting {step.script_name} on {step.label} ({step.target})")
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
    parser = argparse.ArgumentParser(description="Master/slave OverlayBD JB Workspace demo")
    parser.add_argument("--master", default="root@178.128.247.74")
    parser.add_argument("--slave", default="anton.kapeliushok@34.76.115.252")
    parser.add_argument("--registry", default="178.128.247.74:5000")
    parser.add_argument("--master-registry", default="127.0.0.1:5000")
    parser.add_argument("--repo", default="orca/overlaybd-jb-real")
    parser.add_argument("--base-tag", default="obd-jb-real-sysbox-20260608T171059Z")
    parser.add_argument("--run-id", default=run_id)
    parser.add_argument("--derived-tag")
    parser.add_argument("--project-dir", default="/home/workspace-agent/spring-petclinic")
    parser.add_argument("--timeout-seconds", type=int, default=900)
    parser.add_argument("--yes", action="store_true", help="do not pause between steps")
    parser.add_argument("--dry-run", action="store_true", help="walk the scenario without running SSH")
    parser.add_argument(
        "--show-commands",
        action="store_true",
        help="print expanded SSH and main remote commands before each confirmation",
    )
    args = parser.parse_args()
    derived_tag = args.derived_tag or f"petclinic-build-{args.run_id}"
    return Config(
        master=args.master,
        slave=args.slave,
        registry=args.registry,
        master_registry=args.master_registry,
        repo=args.repo,
        base_tag=args.base_tag,
        run_id=args.run_id,
        derived_tag=derived_tag,
        project_dir=args.project_dir,
        timeout_seconds=args.timeout_seconds,
        yes=args.yes,
        dry_run=args.dry_run,
        show_commands=args.show_commands,
    )


def main() -> int:
    config = parse_args()
    print_config(config)
    scenario = [
        ScenarioStep(
            "clean up demo environments on master and slave",
            "cleanup",
            [
                RemoteStep("master", config.master, "cleanup.sh"),
                RemoteStep("slave", config.slave, "cleanup.sh"),
            ],
        ),
        ScenarioStep(
            "run JB workspace on master until Join URL",
            "master workspace cold",
            [
                RemoteStep(
                    "master",
                    config.master,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.base_image_master,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-master-workspace-cold-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "run JB workspace on slave until Join URL",
            "slave workspace cold",
            [
                RemoteStep(
                    "slave",
                    config.slave,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.base_image_slave,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-slave-workspace-cold-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "warm-run JB workspace on slave until Join URL",
            "slave workspace hot",
            [
                RemoteStep(
                    "slave",
                    config.slave,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.base_image_slave,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-slave-workspace-hot-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "clone and build Spring Petclinic on slave with warm workspace layers",
            "slave clone+build coldish",
            [
                RemoteStep(
                    "slave",
                    config.slave,
                    "petclinic-build-mutable.sh",
                    {
                        "IMAGE_REF": config.base_image_slave,
                        "CONTAINER_NAME": f"demo-jb-slave-petclinic-build-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "export slave overlay upperdir and push derived image",
            "commit derived image",
            [RemoteStep("slave", config.slave, "commit-snapshot.sh")],
        ),
        ScenarioStep(
            "run JB workspace on slave from derived image until Join URL",
            "slave derived workspace coldish",
            [
                RemoteStep(
                    "slave",
                    config.slave,
                    "run-workspace-until-join.sh",
                    {
                        "IMAGE_REF": config.derived_image_slave,
                        "KEEP_AFTER_JOIN": "0",
                        "CONTAINER_NAME": f"demo-jb-slave-derived-workspace-coldish-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "grep Petclinic state and read tarball on slave from first-use derived image",
            "slave grep+read coldish",
            [
                RemoteStep(
                    "slave",
                    config.slave,
                    "petclinic-read-artifact.sh",
                    {
                        "IMAGE_REF": config.derived_image_slave,
                        "CONTAINER_NAME": f"demo-jb-slave-petclinic-read-coldish-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "grep Petclinic state and read tarball on master from first-use derived image",
            "master grep+read coldish",
            [
                RemoteStep(
                    "master",
                    config.master,
                    "petclinic-read-artifact.sh",
                    {
                        "IMAGE_REF": config.derived_image_master,
                        "CONTAINER_NAME": f"demo-jb-master-petclinic-read-coldish-{config.run_id}",
                    },
                )
            ],
        ),
        ScenarioStep(
            "warm-run grep Petclinic state and read tarball on master from derived image",
            "master grep+read warm",
            [
                RemoteStep(
                    "master",
                    config.master,
                    "petclinic-read-artifact.sh",
                    {
                        "IMAGE_REF": config.derived_image_master,
                        "CONTAINER_NAME": f"demo-jb-master-petclinic-read-warm-{config.run_id}",
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
    print(f"  master/local: {config.derived_image_master}")
    print(f"  slave/global: {config.derived_image_slave}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
