#!/usr/bin/env python3
"""Prepare the external GPL driver tree; never vendor it into Open Vault."""
from pathlib import Path
import subprocess
import sys

source = Path(sys.argv[1]).resolve(strict=True)
project = Path(__file__).resolve().parents[2]
if (project / "go.mod").is_file() and (source == project or project in source.parents):
    raise SystemExit("External driver source must be outside the Open Vault checkout")
commit = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
if commit != "68bf21b8a9e74d1c8aac967238f81e189963c0a1":
    raise SystemExit("Unexpected external driver revision")
if subprocess.check_output(["git", "-C", str(source), "status", "--porcelain"], text=True).strip():
    raise SystemExit("External driver source must be pristine before preparation")

# Upstream's feature test generates HAVE_ENUM_REQ_OP_WRITE_ZEROES, whereas
# bio_helper.c tests a different, never-generated identifier on Linux 6.x.
# Correct only that identifier, preserving upstream's zero-write COW handling.
for relative, old, new in [
    ("src/bio_helper.c", "HAVE_ENUM_REQ_OPF_WRITE_ZEROES", "HAVE_ENUM_REQ_OP_WRITE_ZEROES"),
    ("src/dattobd.h", 'DATTOBD_VERSION "0.12.0"', 'DATTOBD_VERSION "0.12.0-ov1"'),
]:
    path = source / relative
    content = path.read_text()
    if content.count(old) != 1:
        raise SystemExit("Unexpected upstream content in " + relative)
    path.write_text(content.replace(old, new))

print("Prepared external GPL dattobd 0.12.0-ov1; its upstream license still applies.")
