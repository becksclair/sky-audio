#!/usr/bin/env python3
from __future__ import annotations

import re
import sys
from pathlib import Path

if len(sys.argv) < 2:
    raise SystemExit("usage: normalize-python-protobuf.py <pb2.py> [...]")

for raw_path in sys.argv[1:]:
    path = Path(raw_path)
    text = path.read_text()
    text = text.replace("from google.protobuf import runtime_version as _runtime_version\n", "")
    text = re.sub(
        r"_runtime_version\.ValidateProtobufRuntimeVersion\([\s\S]*?'audio_engine\.proto'\n\)\n",
        "",
        text,
        count=1,
    )
    text = text.replace("# Protobuf Python Version: 6.31.1\n", "")
    path.write_text(text)
