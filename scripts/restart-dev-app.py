"""Restart only the selected app, using normal macOS quit handling; never force-kill."""
import pathlib
import subprocess
import sys
import time

app = pathlib.Path(sys.argv[1]).resolve()
executable = str(app / "Contents/MacOS/OffloadMonitor")


def running_pids():
    output = subprocess.check_output(["ps", "-axo", "pid=,comm="], text=True)
    return {
        int(parts[0])
        for line in output.splitlines()
        if len(parts := line.strip().split(None, 1)) == 2 and parts[1] == executable
    }


old_pids = running_pids()
if old_pids:
    # Pass the path as an argument, not interpolated AppleScript source.
    subprocess.run(
        ["osascript", "-e", 'on run argv\ntell application (item 1 of argv) to quit\nend run', str(app)],
        check=True,
        timeout=65,
    )
    deadline = time.monotonic() + 60
    while running_pids() & old_pids:
        if time.monotonic() >= deadline:
            sys.exit("App kept open or quit was cancelled; installed dev build is ready. No process was forced to stop.")
        time.sleep(0.2)

# A local-update helper may already have relaunched it.
if not running_pids():
    subprocess.run(["open", "-g", str(app)], check=True)
deadline = time.monotonic() + 15
while not (new_pids := running_pids() - old_pids):
    if time.monotonic() >= deadline:
        sys.exit("Dev app was installed, but a new running process could not be verified.")
    time.sleep(0.2)
print("Verified dev App running; PID(s): " + ", ".join(map(str, sorted(new_pids))))
