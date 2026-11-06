"""Repeatable headless backend checks, including the pinned Foundation source."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

parser = argparse.ArgumentParser()
parser.add_argument('--go', default=os.environ.get('GO_BINARY', 'go'))
parser.add_argument('--write-contract', action='store_true')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
binary = str(Path(args.go).resolve()) if '/' in args.go or '\\' in args.go else args.go

def run(command, cwd=root, capture=False):
    return subprocess.run(command, cwd=cwd, check=True, capture_output=capture)

run([sys.executable, 'tools/foundation_lock.py', '--check'])
run([binary, 'vet', './...'])
run([binary, 'test', './...', '-count=1', '-timeout', '180s'])
foundation = root.parent.parent / 'platform-foundation/platform-foundation-server'
run([binary, 'vet', './...'], foundation)
run([binary, 'test', './...', '-count=1', '-timeout', '180s'], foundation)
raw = run([binary, 'run', './cmd/business-api', 'openapi'], capture=True).stdout
contract = root / 'api/openapi.json'
if args.write_contract:
    contract.write_bytes(raw)
elif not contract.exists() or json.loads(contract.read_bytes()) != json.loads(raw):
    raise SystemExit('OpenAPI drift: run verify.py --write-contract and review the contract')
# The sibling checkout can be edited concurrently. A match before tests alone
# cannot establish that the resulting binary used the pinned final source.
run([sys.executable, 'tools/foundation_lock.py', '--check'])
print('Backend, Foundation source lock, and combined contract checks passed')
