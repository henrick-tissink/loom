#!/usr/bin/env python3
"""Install the built GUI and a desktop launcher for the current user."""
import argparse
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import fcntl
import hashlib
import json

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--workspace", default="", help="Project discovery root")
parser.add_argument("--uninstall", action="store_true", help="Remove application files; preserve state and agent sessions")
parser.add_argument("--prefix", type=Path, default=Path.home() / ".local", help="Installation prefix")
parser.add_argument("--data-dir", type=Path, help="Override XDG data destination")
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
home = Path.home()
args.prefix = args.prefix.expanduser().resolve()
data = (args.data_dir or Path(os.environ.get("XDG_DATA_HOME") or args.prefix / "share")).expanduser().resolve()
binary = args.prefix / "bin/loom-gui"
icon = data / "icons/hicolor/scalable/apps/loom.svg"
desktop = data / "applications/loom.desktop"
receipt = args.prefix / "share/loom/install-receipt.json"

# Desktop Exec quoting is not shell quoting. Percent signs are field codes.
def quote(value):
    value = str(value).replace("%", "%%")
    if "\n" in value or "\r" in value:
        raise SystemExit("Desktop launcher paths cannot contain line breaks")
    for char in ("\\", '"', "`", "$"):
        value = value.replace(char, "\\" + char)
    return '"' + value + '"'

command = quote(binary)
if args.workspace:
    workspace = str(Path(args.workspace).expanduser().resolve())
    if not Path(workspace).is_dir():
        raise SystemExit("Workspace must be an existing directory")
    command = "/usr/bin/env " + quote("LOOM_WORKSPACE=" + workspace) + " " + command

binary.parent.mkdir(parents=True, exist_ok=True)
install_lock = (binary.parent / ".loom-install.lock").open("w")
fcntl.flock(install_lock, fcntl.LOCK_EX)

if args.uninstall:
    if not receipt.is_file():
        raise SystemExit("No Loom install receipt; reinstall before uninstalling")
    records = json.loads(receipt.read_text())
    for path in (binary, icon, desktop):
        if path.exists() and hashlib.sha256(path.read_bytes()).hexdigest() != records.get(str(path)):
            raise SystemExit(f"Refusing to remove modified or unowned file: {path}")
    for path in (binary, icon, desktop):
        path.unlink(missing_ok=True)
    receipt.unlink()
    print("Removed Loom application files; settings, history and sessions retained.")
else:
    source = root / "loom-gui"
    if not source.is_file() or not os.access(source, os.X_OK):
        raise SystemExit("Build loom-gui before installing")
    icon_source = root / "cmd/loom-gui/build/appicon.svg"
    icon_bytes = icon_source.read_bytes()
    for path in (binary, icon, desktop, receipt):
        path.parent.mkdir(parents=True, exist_ok=True)
    # Serialize installers and use unique temporary files for atomic replacement.
    def replace_bytes(path, content, mode):
        fd, tmp = tempfile.mkstemp(prefix=".loom-", dir=path.parent)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(content)
                stream.flush()
                os.fsync(stream.fileno())
            os.chmod(tmp, mode)
            os.replace(tmp, path)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)
    replace_bytes(binary, source.read_bytes(), 0o755)
    replace_bytes(icon, icon_bytes, 0o644)
    replace_bytes(desktop, (
        "[Desktop Entry]\nType=Application\nName=Loom\n"
        "Comment=Manage coding agent sessions\n"
        f"Exec={command}\nIcon=loom\nTerminal=false\n"
        "Categories=Development;\nStartupWMClass=loom-gui\n"
        "Keywords=Claude;Agents;Terminal;\n"
    ).encode(), 0o644)
    replace_bytes(receipt, json.dumps({str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in (binary, icon, desktop)}, indent=2).encode(), 0o600)
    print(f"Installed {binary}\nLauncher: {desktop}")
if shutil.which("update-desktop-database") and desktop.parent.exists():
    subprocess.run(["update-desktop-database", str(desktop.parent)], check=True)
