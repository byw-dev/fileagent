"""Strip machine/environment fingerprints from evidence files before publishing.

Run on tmp/ outputs before copying them into results/ (the repo is public):
    python3 redact.py tmp/probe tmp/bench ...      # default: results/

Replaces, without changing what the evidence shows:
  - private IPv4 addresses (Docker / OrbStack bridge)   -> 192.0.2.x (RFC 5737 documentation range)
  - client User-Agent OS/arch, e.g. "(darwin; arm64)"   -> "(<os>; <arch>)"
  - MinIO deployment IDs                                 -> all-zero UUID
  - truncated STS access-key prefixes in probe logs      -> "<redacted>"
  - MinIO x-amz-id-2 (SHA-256 of the node name)          -> 64 zeros
Gzip files are rewritten in place. Prints a per-file count of replacements.
"""
import gzip
import os
import re
import sys

PRIVATE_IP = re.compile(r"(?<![\d.])(192\.168|10\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3}(?![\d.])")
RULES = [
    (re.compile(r"\((darwin|linux|windows|freebsd); (arm64|amd64|x86_64|aarch64)\)"), "(<os>; <arch>)"),
    (re.compile(r'(x-minio-deployment-id"\s*:\s*")(?!0{8}-0{4})[0-9a-f-]{36}'), r"\g<1>00000000-0000-0000-0000-000000000000"),
    # MinIO sets x-amz-id-2 to SHA-256(local node name): a stable host fingerprint.
    (re.compile(r'("x-amz-id-2"\s*:\s*")(?!0{64})[0-9a-f]{64}'), "\\g<1>" + "0" * 64),
    # mixed case (SeaweedFS / JuiceFS keys) and leftovers of an earlier, narrower rule
    (re.compile(r"\bAK (?:<redacted>…)?[A-Za-z0-9]+(?:…|\.\.\.)?"), "AK <redacted>…"),
]
TEXT_EXT = (".json", ".jsonl", ".log", ".md", ".txt")


def redact(text, ip_map):
    """Return the redacted text and the number of replacements made."""
    n = 0

    def ip_sub(m):
        nonlocal n
        n += 1
        if m.group(0) not in ip_map:
            ip_map[m.group(0)] = f"192.0.2.{len(ip_map) + 1}"
        return ip_map[m.group(0)]

    text = PRIVATE_IP.sub(ip_sub, text)
    for pat, rep in RULES:
        text, k = pat.subn(rep, text)
        n += k
    return text, n


def main(paths):
    ip_map = {}
    for root in paths:
        for d, _, files in os.walk(root):
            for f in sorted(files):
                p = os.path.join(d, f)
                if f.endswith(".gz"):
                    with gzip.open(p, "rt") as fh:
                        text = fh.read()
                elif f.endswith(TEXT_EXT):
                    with open(p, encoding="utf-8") as fh:
                        text = fh.read()
                else:
                    continue
                new, n = redact(text, ip_map)
                if n:
                    if f.endswith(".gz"):
                        with gzip.open(p, "wt") as fh:
                            fh.write(new)
                    else:
                        with open(p, "w", encoding="utf-8") as fh:
                            fh.write(new)
                    print(f"{n:6} {p}")
    if ip_map:
        print("IP mapping:", ", ".join(f"{k} -> {v}" for k, v in ip_map.items()))


if __name__ == "__main__":
    main(sys.argv[1:] or ["results"])
