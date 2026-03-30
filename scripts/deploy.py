#!/usr/bin/env python3
from __future__ import annotations

import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import NoReturn

REPO_ROOT = Path(__file__).resolve().parent.parent
DEPLOY_ROOT = Path("/home/bex/Apps/services/sky-audio")
STAGING_ROOT = DEPLOY_ROOT.parent / ".sky-audio-staging"
BACKUP_ROOT = DEPLOY_ROOT.parent / ".sky-audio-backup"
SYSTEMD_USER_DIR = Path.home() / ".config/systemd/user"
SERVICE_NAME = "sky-audio.service"
SERVICE_PATH = SYSTEMD_USER_DIR / SERVICE_NAME
ENV_DIR = Path.home() / ".config/sky-audio"
ENV_FILE = ENV_DIR / "sky-audio.env"
DEFAULT_PORT = "8585"
ONNX_LIB_DIRS = [
    Path("/usr/local/lib/onnxruntime"),
    Path("/usr/local/lib"),
]
WORKER_DIRS = [
    "python-nemo",
    "python-pocket-tts",
    "python-kyutai-tts",
]
COMMON_PATH_ENTRIES = [
    "%h/.local/share/mise/shims",
    "%h/.local/bin",
    "%h/.cargo/bin",
    "%h/.bun/bin",
    "/usr/local/bin",
    "/usr/bin",
    "/bin",
]

UV_PATH: Path | None = None


def log(message: str) -> None:
    print(f"[deploy] {message}")


def fail(message: str) -> NoReturn:
    raise SystemExit(message)


def run(command: list[str], *, cwd: Path | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
    display_cwd = f" (cwd={cwd})" if cwd else ""
    log(f"$ {' '.join(command)}{display_cwd}")
    result = subprocess.run(
        command,
        cwd=str(cwd) if cwd else None,
        text=True,
        capture_output=True,
        check=False,
    )
    if result.stdout:
        sys.stdout.write(result.stdout)
    if result.stderr:
        sys.stderr.write(result.stderr)
    if check and result.returncode != 0:
        raise subprocess.CalledProcessError(result.returncode, command)
    return result


def resolve_uv_path() -> Path:
    uv_path = shutil.which("uv")
    if not uv_path:
        fail("uv is not available on PATH")
    return Path(uv_path).resolve()


def service_path_env() -> str:
    entries = list(COMMON_PATH_ENTRIES)
    if UV_PATH is not None:
        uv_dir = str(UV_PATH.parent)
        if uv_dir not in entries:
            entries.insert(0, uv_dir)
    return ":".join(entries)


def load_env_file() -> dict[str, str]:
    values: dict[str, str] = {}
    if not ENV_FILE.exists():
        return values
    for raw_line in ENV_FILE.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith("export "):
            line = line[7:].strip()
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip('"').strip("'")
    return values


def resolve_service_port() -> str:
    env = load_env_file()
    raw_port = env.get("PORT", "").strip() or DEFAULT_PORT
    try:
        port = int(raw_port)
    except ValueError:
        fail(f"invalid PORT value in {ENV_FILE}: {raw_port!r}")
    if not (1 <= port <= 65535):
        fail(f"PORT value out of range in {ENV_FILE}: {port}")
    return str(port)


def service_urls() -> tuple[str, str]:
    port = resolve_service_port()
    base_url = f"http://127.0.0.1:{port}"
    return f"{base_url}/health", f"{base_url}/api/tts/voices"


def ensure_prerequisites() -> None:
    required_paths = [
        REPO_ROOT / "gateway/sky-audio",
        REPO_ROOT / "workers/python-pocket-tts/voices.json",
        REPO_ROOT / "workers/python-pocket-tts/voices",
        REPO_ROOT / "workers/python-kyutai-tts/voices.json",
        REPO_ROOT / "workers/python-kyutai-tts/voices",
        REPO_ROOT / "workers/python-nemo/pyproject.toml",
        REPO_ROOT / "workers/python-nemo/uv.lock",
        REPO_ROOT / "workers/python-pocket-tts/pyproject.toml",
        REPO_ROOT / "workers/python-pocket-tts/uv.lock",
        REPO_ROOT / "workers/python-kyutai-tts/pyproject.toml",
        REPO_ROOT / "workers/python-kyutai-tts/uv.lock",
    ]
    for path in required_paths:
        if not path.exists():
            fail(f"missing required deploy artifact: {path}")

    global UV_PATH
    UV_PATH = resolve_uv_path()
    log(f"resolved uv at {UV_PATH}")

    libraries = find_onnx_libraries()
    log(f"found {len(libraries)} ONNX Runtime shared library files")

    run(["systemctl", "--user", "--version"])


def copy_file(src: Path, dest: Path) -> None:
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dest)


def ignore_worker_files(_: str, names: list[str]) -> set[str]:
    ignored = {".venv", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".pyright"}
    return {name for name in names if name in ignored}


def find_onnx_libraries() -> list[Path]:
    found: dict[str, Path] = {}
    for lib_dir in ONNX_LIB_DIRS:
        if not lib_dir.exists():
            continue
        for candidate in lib_dir.glob("libonnxruntime*.so*"):
            resolved = candidate.resolve()
            found[candidate.name] = candidate
            if resolved.exists():
                found[resolved.name] = resolved
    libraries = sorted(found.values(), key=lambda path: path.name)
    if not libraries:
        searched = ", ".join(str(path) for path in ONNX_LIB_DIRS)
        fail(f"could not find ONNX Runtime shared libraries in: {searched}")
    return libraries


def stage_payload() -> None:
    if STAGING_ROOT.exists():
        shutil.rmtree(STAGING_ROOT)
    STAGING_ROOT.mkdir(parents=True)

    copy_file(REPO_ROOT / "gateway/sky-audio", STAGING_ROOT / "sky-audio")

    staged_workers_root = STAGING_ROOT / "workers"
    staged_workers_root.mkdir(parents=True, exist_ok=True)
    for worker_name in WORKER_DIRS:
        src_dir = REPO_ROOT / "workers" / worker_name
        dest_dir = staged_workers_root / worker_name
        shutil.copytree(src_dir, dest_dir, ignore=ignore_worker_files, dirs_exist_ok=True)

    lib_dir = STAGING_ROOT / "lib"
    lib_dir.mkdir(parents=True, exist_ok=True)
    for library in find_onnx_libraries():
        copy_file(library, lib_dir / library.name)


def sync_worker_envs(root: Path) -> None:
    uv_command = str(UV_PATH) if UV_PATH is not None else "uv"
    for worker_name in WORKER_DIRS:
        worker_dir = root / "workers" / worker_name
        run([uv_command, "sync", "--frozen"], cwd=worker_dir)


def remove_worker_envs(root: Path) -> None:
    for worker_name in WORKER_DIRS:
        venv_dir = root / "workers" / worker_name / ".venv"
        if venv_dir.exists():
            shutil.rmtree(venv_dir)


def render_service() -> str:
    return "\n".join(
        [
            "[Unit]",
            "Description=sky-audio local speech gateway",
            "After=default.target network-online.target",
            "Wants=network-online.target",
            "StartLimitIntervalSec=0",
            "",
            "[Service]",
            "Type=simple",
            f"WorkingDirectory={DEPLOY_ROOT}",
            f"Environment=LD_LIBRARY_PATH={DEPLOY_ROOT}/lib:/usr/local/lib/onnxruntime:/usr/local/lib",
            f"Environment=PATH={service_path_env()}",
            f"EnvironmentFile=-{ENV_FILE}",
            f"ExecStart={DEPLOY_ROOT}/sky-audio",
            "Restart=always",
            "RestartSec=2",
            "TimeoutStopSec=30",
            "SuccessExitStatus=0 143",
            "KillMode=control-group",
            "",
            "[Install]",
            "WantedBy=default.target",
            "",
        ]
    )


def default_env_file_content() -> str:
    return (
        "# Optional sky-audio overrides\n"
        "# PORT=9000\n"
        "# Setting PORT makes the gateway bind to all interfaces; set SKY_AUDIO_API_KEY too.\n"
        "# SKY_AUDIO_API_KEY=replace-me-if-binding-non-loopback\n"
    )


def install_service() -> None:
    SYSTEMD_USER_DIR.mkdir(parents=True, exist_ok=True)
    ENV_DIR.mkdir(parents=True, exist_ok=True)
    current_env = ENV_FILE.read_text(encoding="utf-8") if ENV_FILE.exists() else None
    old_stub = (
        "# Optional sky-audio overrides\n"
        "# PORT=8585\n"
        "# SKY_AUDIO_API_KEY=replace-me-if-binding-non-loopback\n"
    )
    if current_env is None or current_env == old_stub:
        ENV_FILE.write_text(default_env_file_content(), encoding="utf-8")
    SERVICE_PATH.write_text(render_service(), encoding="utf-8")


def stop_service() -> None:
    result = run(["systemctl", "--user", "is-active", SERVICE_NAME], check=False)
    if result.returncode == 0:
        run(["systemctl", "--user", "stop", SERVICE_NAME])
    deadline = time.time() + 30
    while time.time() < deadline:
        state = run(["systemctl", "--user", "is-active", SERVICE_NAME], check=False)
        if state.returncode != 0:
            return
        time.sleep(1)
    fail(f"timed out waiting for {SERVICE_NAME} to stop")


def replace_deploy_root() -> None:
    if BACKUP_ROOT.exists():
        shutil.rmtree(BACKUP_ROOT)
    if DEPLOY_ROOT.exists():
        DEPLOY_ROOT.rename(BACKUP_ROOT)
    try:
        STAGING_ROOT.rename(DEPLOY_ROOT)
    except BaseException:
        if BACKUP_ROOT.exists() and not DEPLOY_ROOT.exists():
            BACKUP_ROOT.rename(DEPLOY_ROOT)
        raise


def cleanup_backup() -> None:
    if BACKUP_ROOT.exists():
        shutil.rmtree(BACKUP_ROOT)


def restore_previous_deploy() -> bool:
    if not BACKUP_ROOT.exists():
        return False
    if DEPLOY_ROOT.exists():
        shutil.rmtree(DEPLOY_ROOT)
    BACKUP_ROOT.rename(DEPLOY_ROOT)
    return True


def start_service() -> None:
    run(["systemctl", "--user", "daemon-reload"])
    run(["systemctl", "--user", "enable", SERVICE_NAME])
    run(["systemctl", "--user", "restart", SERVICE_NAME])


def wait_for_service_active() -> None:
    deadline = time.time() + 60
    while time.time() < deadline:
        active = run(["systemctl", "--user", "is-active", SERVICE_NAME], check=False)
        if active.returncode == 0:
            return
        failed = run(["systemctl", "--user", "is-failed", SERVICE_NAME], check=False)
        if failed.returncode == 0:
            fail(f"{SERVICE_NAME} entered failed state during startup")
        time.sleep(1)
    fail(f"timed out waiting for {SERVICE_NAME} to become active")


def http_get(url: str, *, headers: dict[str, str] | None = None) -> str:
    request = urllib.request.Request(url, headers=headers or {})
    with urllib.request.urlopen(request, timeout=5) as response:
        return response.read().decode("utf-8", errors="replace")


def wait_for_http_health() -> None:
    env = load_env_file()
    health_url, voices_url = service_urls()
    headers: dict[str, str] = {}
    api_key = env.get("SKY_AUDIO_API_KEY", "")
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"

    deadline = time.time() + 90
    last_error = "unknown error"
    while time.time() < deadline:
        try:
            body = http_get(health_url).strip()
            if body != "ok":
                last_error = f"unexpected health body: {body!r}"
            else:
                voices = http_get(voices_url, headers=headers)
                if '"pocket"' not in voices or '"kyutai"' not in voices:
                    last_error = "voice catalog endpoint did not return expected engines"
                else:
                    log(f"health verification passed via {health_url} and {voices_url}")
                    return
        except (urllib.error.URLError, TimeoutError, OSError) as exc:
            last_error = str(exc)
        time.sleep(2)
    fail(f"timed out waiting for HTTP health: {last_error}")


def dump_failure_context() -> None:
    run(["systemctl", "--user", "status", "--no-pager", SERVICE_NAME], check=False)
    run(["journalctl", "--user", "-u", SERVICE_NAME, "-n", "100", "--no-pager"], check=False)


def cleanup_staging() -> None:
    if STAGING_ROOT.exists():
        shutil.rmtree(STAGING_ROOT)


def main() -> None:
    deployment_healthy = False
    service_stopped = False
    try:
        ensure_prerequisites()
        stage_payload()
        sync_worker_envs(STAGING_ROOT)
        remove_worker_envs(STAGING_ROOT)
        stop_service()
        service_stopped = True
        replace_deploy_root()
        sync_worker_envs(DEPLOY_ROOT)
        install_service()
        start_service()
        wait_for_service_active()
        wait_for_http_health()
        deployment_healthy = True
        log(f"deployment complete: {DEPLOY_ROOT}")
    except BaseException as exc:
        if isinstance(exc, KeyboardInterrupt):
            log("deployment interrupted")
        else:
            log(f"deployment failed: {exc}")
            dump_failure_context()

        restarted = False
        restored = restore_previous_deploy()
        if restored:
            log("restored previous deployment after failure")
            restarted = True
        elif service_stopped and DEPLOY_ROOT.exists():
            log("restarting existing deployment after failure")
            restarted = True

        if restarted:
            try:
                start_service()
                wait_for_service_active()
                wait_for_http_health()
                log("deployment rollback restart succeeded")
            except BaseException as rollback_exc:
                log(f"rollback restart failed: {rollback_exc}")
                dump_failure_context()
        raise
    finally:
        cleanup_staging()
        if deployment_healthy:
            try:
                cleanup_backup()
            except OSError as cleanup_exc:
                log(f"backup cleanup failed: {cleanup_exc}")


if __name__ == "__main__":
    main()
