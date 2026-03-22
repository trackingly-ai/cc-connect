from __future__ import annotations

import argparse
import os
from pathlib import Path


def _repo_root() -> Path:
    return Path(__file__).resolve().parents[2]


def _load_env_file(path: Path | None) -> dict[str, str]:
    if path is None or not path.exists():
        return {}
    loaded: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        key, sep, value = line.partition("=")
        if not sep:
            raise ValueError(f"invalid env line: {raw_line!r}")
        loaded[key.strip()] = value.strip()
    return loaded


def _render_template(template_path: Path, values: dict[str, str]) -> str:
    text = template_path.read_text(encoding="utf-8")
    for key, value in values.items():
        text = text.replace(f"{{{{{key}}}}}", value)
    return text


def _write_executable(path: Path, content: str) -> None:
    path.write_text(content, encoding="utf-8")
    path.chmod(0o755)


def _default_launchd_path(home: Path) -> str:
    ordered: list[str] = []

    def add(path: str) -> None:
        if path and path not in ordered:
            ordered.append(path)

    add(str(home / ".local" / "bin"))
    add("/opt/homebrew/opt/node@22/bin")
    add("/opt/homebrew/bin")
    add("/opt/homebrew/sbin")
    add("/usr/local/bin")
    add("/usr/local/sbin")
    add("/usr/bin")
    add("/bin")
    add("/usr/sbin")
    add("/sbin")

    for path in os.environ.get("PATH", "").split(os.pathsep):
        add(path)

    return ":".join(ordered)


def _build_values(env_file: Path | None) -> dict[str, str]:
    repo_root = _repo_root()
    home = Path.home()
    env = _load_env_file(env_file)

    def get(name: str, default: str) -> str:
        return env.get(name) or os.environ.get(name) or default

    deploy_base_dir = Path(
        get("DEPLOY_BASE_DIR", str(home / ".local" / "share" / "echo-single-host"))
    )
    systemd_user_dir = Path(
        get("SYSTEMD_USER_DIR", str(home / ".config" / "systemd" / "user"))
    )
    launchd_agent_dir = Path(
        get("LAUNCHD_AGENT_DIR", str(home / "Library" / "LaunchAgents"))
    )
    echo_repo_dir = Path(get("ECHO_REPO_DIR", str(repo_root.parent / "echo")))
    cc_connect_repo_dir = Path(get("CC_CONNECT_REPO_DIR", str(repo_root)))
    rendered_dir = deploy_base_dir / "rendered" / "cc-connect"
    config_dir = deploy_base_dir / "config" / "cc-connect"
    bin_dir = deploy_base_dir / "bin"
    log_dir = deploy_base_dir / "log"
    cc_connect_config_path = config_dir / "config.toml"
    deploy_env_path = config_dir / "deploy.env"
    run_cc_connect_script = bin_dir / "run-cc-connect.sh"
    cc_connect_binary = bin_dir / "cc-connect"
    cc_connect_unit_path = rendered_dir / "cc-connect.service"
    cc_connect_plist_path = rendered_dir / "cc-connect.plist"

    values = {
        "ECHO_REPO_DIR": str(echo_repo_dir),
        "CC_CONNECT_REPO_DIR": str(cc_connect_repo_dir),
        "DEPLOY_BASE_DIR": str(deploy_base_dir),
        "SYSTEMD_USER_DIR": str(systemd_user_dir),
        "LAUNCHD_AGENT_DIR": str(launchd_agent_dir),
        "RENDERED_DIR": str(rendered_dir),
        "CONFIG_DIR": str(config_dir),
        "BIN_DIR": str(bin_dir),
        "LOG_DIR": str(log_dir),
        "CC_CONNECT_CONFIG_PATH": str(cc_connect_config_path),
        "DEPLOY_ENV_PATH": str(deploy_env_path),
        "RUN_CC_CONNECT_SCRIPT": str(run_cc_connect_script),
        "CC_CONNECT_BINARY": str(cc_connect_binary),
        "CC_CONNECT_UNIT_PATH": str(cc_connect_unit_path),
        "CC_CONNECT_PLIST_PATH": str(cc_connect_plist_path),
        "ECHO_SERVER_URL": get("ECHO_SERVER_URL", "http://127.0.0.1:8000"),
        "ECHO_WORKER_TOKEN": get("ECHO_WORKER_TOKEN", get("ECHO_WORKER_GATEWAY_TOKEN", "")),
        "CC_HOST_ID": get("CC_HOST_ID", "host-local"),
        "CC_HOST_LABEL": get("CC_HOST_LABEL", "Local Host"),
        "CLAUDE_MANAGER_MODE": get("CLAUDE_MANAGER_MODE", "bypassPermissions"),
        "CODEX_ARCHITECT_MODE": get("CODEX_ARCHITECT_MODE", "full-auto"),
        "CODEX_CODER_MODE": get("CODEX_CODER_MODE", "yolo"),
        "QODER_REVIEWER_MODE": get("QODER_REVIEWER_MODE", "yolo"),
        "CODEX_LANDER_MODE": get("CODEX_LANDER_MODE", "yolo"),
        "LAUNCHD_PATH": get(
            "LAUNCHD_PATH",
            _default_launchd_path(home),
        ),
        "LAUNCHD_CC_CONNECT_LABEL": "com.echo.cc_connect",
    }
    return values


def render(env_file: Path | None) -> dict[str, str]:
    repo_root = _repo_root()
    values = _build_values(env_file)

    rendered_dir = Path(values["RENDERED_DIR"])
    config_dir = Path(values["CONFIG_DIR"])
    bin_dir = Path(values["BIN_DIR"])
    log_dir = Path(values["LOG_DIR"])
    rendered_dir.mkdir(parents=True, exist_ok=True)
    config_dir.mkdir(parents=True, exist_ok=True)
    bin_dir.mkdir(parents=True, exist_ok=True)
    log_dir.mkdir(parents=True, exist_ok=True)

    template_root = repo_root / "deploy"
    Path(values["CC_CONNECT_UNIT_PATH"]).write_text(
        _render_template(template_root / "systemd" / "cc-connect.service.tmpl", values),
        encoding="utf-8",
    )
    Path(values["CC_CONNECT_PLIST_PATH"]).write_text(
        _render_template(template_root / "launchd" / "cc-connect.plist.tmpl", values),
        encoding="utf-8",
    )
    Path(values["CC_CONNECT_CONFIG_PATH"]).write_text(
        _render_template(
            template_root / "templates" / "echo-projects.toml.tmpl", values
        ),
        encoding="utf-8",
    )
    Path(values["DEPLOY_ENV_PATH"]).write_text(
        "\n".join(f"{key}={value}" for key, value in sorted(values.items())) + "\n",
        encoding="utf-8",
    )
    launchd_path_export = f'export PATH="{values["LAUNCHD_PATH"]}:${{PATH}}"'
    _write_executable(
        Path(values["RUN_CC_CONNECT_SCRIPT"]),
        "\n".join(
            [
                "#!/usr/bin/env bash",
                "set -euo pipefail",
                launchd_path_export,
                f"cd {values['CC_CONNECT_REPO_DIR']}",
                (
                    f"exec {values['CC_CONNECT_BINARY']} "
                    f"-config {values['CC_CONNECT_CONFIG_PATH']}"
                ),
                "",
            ]
        ),
    )
    return values


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Render single-host Echo worker deployment assets for cc-connect.",
    )
    parser.add_argument(
        "--env-file",
        type=Path,
        default=None,
        help="Optional env override file. Defaults are derived automatically.",
    )
    args = parser.parse_args()
    values = render(args.env_file)
    print(f"Rendered cc-connect deployment assets to {values['RENDERED_DIR']}")
    print(f"cc-connect deploy env: {values['DEPLOY_ENV_PATH']}")
    print(f"cc-connect config: {values['CC_CONNECT_CONFIG_PATH']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
