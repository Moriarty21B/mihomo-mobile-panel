"""Catch common accidental private artifacts in the source tree (stdlib only).

This is a guardrail, not a substitute for reviewing configuration and history.
Runtime state and generated binaries are never inputs to the source archive.
"""
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[1]
ignored_parts = {'.git', 'dist', '__pycache__'}
forbidden_names = {
    'config.yaml', 'settings.json', 'devices.json', 'traffic.json',
    'profiles.json', 'profiles-previous.json', 'selectors.json',
    'known_hosts', 'router-password.dat', 'host-key.json',
}
secret_patterns = [
    re.compile(rb'-{5}BEGIN (?:OPENSSH |RSA |EC |ENCRYPTED )?PRIVATE KEY-{5}'),
    re.compile(rb'gh[pousr]_[A-Za-z0-9]{36,}'),
    re.compile(rb'github_pat_[A-Za-z0-9_]{50,}'),
]
mac_pattern = re.compile(r'(?i)(?<![0-9a-f])(?:[0-9a-f]{2}:){5}[0-9a-f]{2}(?![0-9a-f])')
problems = []
checked = 0

for file in sorted(root.rglob('*')):
    relative = file.relative_to(root)
    if ignored_parts.intersection(relative.parts) or relative.parts[:2] == ('installer', 'payload') or relative.as_posix() == 'installer/manifest_windows_amd64.syso':
        continue
    if file.is_symlink():
        problems.append((relative, 'unexpected symlink'))
        continue
    if not file.is_file():
        continue
    if file.name in forbidden_names or file.suffix.lower() in {'.log', '.zip', '.tgz', '.exe', '.pem', '.key', '.pcap'}:
        problems.append((relative, 'runtime/private or generated artifact'))
        continue
    data = file.read_bytes()
    checked += 1
    if any(pattern.search(data) for pattern in secret_patterns):
        problems.append((relative, 'credential-like material'))
    if file.name == 'check-public.py':
        continue
    try:
        text = data.decode('utf8')
    except UnicodeDecodeError:
        problems.append((relative, 'unexpected non-text source file'))
        continue
    if re.search(r'(?i)[A-Z]:[/\\]Users[/\\][^/\\\s]+', text):
        problems.append((relative, 'personal absolute path'))
    if any(not mac.lower().startswith('02:00:00:00:00:') for mac in mac_pattern.findall(text)):
        problems.append((relative, 'non-synthetic device identifier'))

try:
    tracked = subprocess.run(['git', '-C', str(root), 'ls-files', '-z'], capture_output=True, check=True).stdout
except (FileNotFoundError, subprocess.CalledProcessError):
    tracked = b''
for raw in tracked.split(b'\0'):
    name = raw.decode('utf8', errors='replace')
    if (name.startswith(('dist/', 'installer/payload/')) and name != 'installer/payload/.gitkeep') or name.endswith('.syso'):
        problems.append((name, 'generated payload is tracked'))

if problems:
    for path, reason in problems:
        print(str(path) + ': ' + reason, file=sys.stderr)
    raise SystemExit(1)
print('Public source check passed (%d text files); review runtime data separately.' % checked)
