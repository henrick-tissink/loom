#!/usr/bin/env python3
"""Install the built GUI and a desktop launcher for the current user."""
import argparse
import os
from pathlib import Path
import shutil
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--workspace", default="", help="Project discovery root")
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
home = Path.home()
data = Path(os.environ.get("XDG_DATA_HOME") or home / ".local/share")
binary = home / ".local/bin/loom-gui"
icon = data / "icons/hicolor/scalable/apps/loom.svg"
desktop = data / "applications/loom.desktop"

# Desktop Exec quoting is not shell quoting. Percent signs are field codes.
def quote(value):
    value = str(value).replace("%", "%%")
    for char in ("\\", '"', "`", "$"):
        value = value.replace(char, "\\" + char)
    return '"' + value + '"'

command = quote(binary)
if args.workspace:
    workspace = str(Path(args.workspace).expanduser().resolve())
    command = "/usr/bin/env " + quote("LOOM_WORKSPACE=" + workspace) + " " + command

for path in (binary, icon, desktop):
    path.parent.mkdir(parents=True, exist_ok=True)
# Atomic replacement also works while the previous executable is running.
temporary = binary.with_suffix(".new")
shutil.copy2(root / "loom-gui", temporary)
temporary.chmod(0o755)
temporary.replace(binary)
shutil.copy2(root / "cmd/loom-gui/build/appicon.svg", icon)
desktop.write_text(
    "[Desktop Entry]\nType=Application\nName=Loom\n"
    "Comment=Manage coding agent sessions\n"
    f"Exec={command}\nIcon=loom\nTerminal=false\n"
    "Categories=Development;\nStartupWMClass=loom-gui\n"
    "Keywords=Claude;Agents;Terminal;\n"
)
if shutil.which("update-desktop-database"):
    subprocess.run(["update-desktop-database", str(desktop.parent)], check=True)
print(f"Installed {binary}\nLauncher: {desktop}")
