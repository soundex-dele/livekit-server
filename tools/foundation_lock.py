"""Lock/check the actual sibling Foundation Go source, without invented releases."""
import argparse
import hashlib
import json
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--check', action='store_true')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
foundation = root.parent.parent / 'platform-foundation' / 'platform-foundation-server'
paths = sorted(p for p in foundation.rglob('*') if p.is_file() and (p.suffix == '.go' or p.name in ('go.mod', 'go.sum') or p.relative_to(foundation).as_posix() == 'contracts/openapi.json'))
if not paths:
    raise SystemExit('Foundation source checkout is missing')
# Git checkouts may use CRLF on Windows. All locked files are text; pin their
# content independently of checkout newline conversion.
files = {p.relative_to(foundation).as_posix(): hashlib.sha256(p.read_bytes().replace(b'\r\n', b'\n')).hexdigest() for p in paths}
lock = {'module': 'platform-foundation/server', 'source': '../../platform-foundation/platform-foundation-server', 'files': files}
target = root / 'foundation.lock.json'
if args.check:
    if not target.exists() or json.loads(target.read_text(encoding='utf-8')) != lock:
        raise SystemExit('Foundation source drift: review the changes and update the lock explicitly')
    print('Foundation source matches lock')
else:
    target.write_text(json.dumps(lock, indent=2) + '\n', encoding='utf-8')
    print('Locked Foundation source files:', len(files))
