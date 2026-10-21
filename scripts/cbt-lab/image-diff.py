#!/usr/bin/env python3
"""Bounded diagnostics for synthetic guest images; differences still fail the lab."""
import hashlib
import json
from pathlib import Path
import sys

oracle, restored, output = map(Path, sys.argv[1:])
report = {"block_size": 4096, "different_blocks": 0, "examples": []}
with oracle.open("rb") as expected, restored.open("rb") as actual:
    offset = 0
    while True:
        left, right = expected.read(1 << 20), actual.read(1 << 20)
        if not left and not right:
            break
        for start in range(0, max(len(left), len(right)), 4096):
            a, b = left[start:start + 4096], right[start:start + 4096]
            if a != b:
                report["different_blocks"] += 1
                if len(report["examples"]) < 256:
                    report["examples"].append({
                        "offset": offset + start,
                        "oracle_sha256": hashlib.sha256(a).hexdigest(),
                        "restored_sha256": hashlib.sha256(b).hexdigest(),
                    })
        offset += max(len(left), len(right))
output.write_text(json.dumps(report, indent=2) + "\n")
print("Differing 4 KiB blocks:", report["different_blocks"])
