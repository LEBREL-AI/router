#!/usr/bin/env python3
"""Check a running Lebrel router's hardware attestation, from the outside, with nothing but Python.

    scripts/verify-attestation.py https://<router host> [--compose-hash <hex from RELEASES.md>]
                                  [--os-image-hash <hex>] [--nonce <hex>] [--json]

What it checks, in order:
  1. The document at /.well-known/attestation answers, with a fresh nonce of our choosing.
  2. The TDX quote's report data equals SHA-512("lebrel-router-attestation-v1" || public_key ||
     nonce): the quote was made for this receipt-signing key and for our nonce, now.
  3. The event log replays into the RTMRs the quote carries (RTMR[i] = SHA-384(RTMR[i] || digest),
     from 48 zero bytes), so the log is the one the hardware measured.
  4. The event log's compose-hash and os-image-hash equal what the document states and, when given,
     what RELEASES.md states for the served version.
  5. Intel's signature chain over the quote, through `dcap-qvl` when it is installed (cargo install
     dcap-qvl-cli). Without it this script says so: steps 1-4 prove the binding, step 5 proves the
     hardware; both are needed for "don't trust, verify".

Exit code 0 only when every check it could run passed; a skipped step 5 is reported, not hidden.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import secrets
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request

DOMAIN = b"lebrel-router-attestation-v1"
QUOTE_HEADER = 48
TD_REPORT_OFFSETS = {
    "tee_tcb_svn": (0, 16), "mr_seam": (16, 48), "mr_signer_seam": (64, 48), "seam_attributes": (112, 8),
    "td_attributes": (120, 8), "xfam": (128, 8), "mr_td": (136, 48), "mr_config_id": (184, 48),
    "mr_owner": (232, 48), "mr_owner_config": (280, 48), "rtmr0": (328, 48), "rtmr1": (376, 48),
    "rtmr2": (424, 48), "rtmr3": (472, 48), "report_data": (520, 64),
}
TD_REPORT_SIZE = 584


def fetch(base: str, nonce: bytes) -> dict:
    url = base.rstrip("/") + "/.well-known/attestation?nonce=" + nonce.hex()
    with urllib.request.urlopen(urllib.request.Request(url, headers={"Accept": "application/json"}), timeout=30) as response:
        return json.load(response)


def td_report(quote: bytes) -> dict[str, bytes]:
    if len(quote) < QUOTE_HEADER + TD_REPORT_SIZE:
        raise ValueError(f"quote is {len(quote)} bytes, shorter than a TDX quote header plus TD report")
    version = int.from_bytes(quote[0:2], "little")
    tee_type = int.from_bytes(quote[4:8], "little")
    if version not in (4, 5) or tee_type != 0x81:
        raise ValueError(f"not a TDX quote: version {version}, tee type {tee_type:#x}")
    body = quote[QUOTE_HEADER:QUOTE_HEADER + TD_REPORT_SIZE]
    return {name: body[offset:offset + size] for name, (offset, size) in TD_REPORT_OFFSETS.items()}


def replay(event_log: list[dict]) -> dict[int, bytes]:
    rtmrs = {i: bytes(48) for i in range(4)}
    for event in event_log:
        imr = int(event.get("imr", -1))
        digest = bytes.fromhex(event.get("digest", ""))
        if imr not in rtmrs or len(digest) != 48:
            raise ValueError(f"event log entry malformed: {event}")
        rtmrs[imr] = hashlib.sha384(rtmrs[imr] + digest).digest()
    return rtmrs


def event_payload(event_log: list[dict], name: str) -> str | None:
    for event in event_log:
        if event.get("event") == name:
            return str(event.get("event_payload", ""))
    return None


def dcap_qvl(quote: bytes) -> tuple[str, str]:
    """('passed'|'failed'|'skipped', detail)"""
    binary = shutil.which("dcap-qvl")
    if not binary:
        return "skipped", "dcap-qvl is not installed (cargo install dcap-qvl-cli); Intel's chain was not checked here"
    with tempfile.NamedTemporaryFile(suffix=".bin", delete=False) as handle:
        handle.write(quote)
        path = handle.name
    try:
        run = subprocess.run([binary, "verify", path], capture_output=True, text=True, timeout=120)
    finally:
        os.unlink(path)
    output = (run.stdout + run.stderr).strip()
    return ("passed" if run.returncode == 0 else "failed"), output[-400:]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("base_url")
    parser.add_argument("--compose-hash", help="the compose hash RELEASES.md lists for the served version")
    parser.add_argument("--os-image-hash", help="the dstack OS image hash expected")
    parser.add_argument("--nonce", help="hex nonce (default: 32 random bytes)")
    parser.add_argument("--json", action="store_true", help="print the checks as JSON")
    args = parser.parse_args()

    nonce = bytes.fromhex(args.nonce) if args.nonce else secrets.token_bytes(32)
    checks: list[dict] = []

    def check(name: str, ok: bool | None, detail: str) -> None:
        checks.append({"check": name, "result": "passed" if ok else ("skipped" if ok is None else "failed"), "detail": detail})

    try:
        document = fetch(args.base_url, nonce)
        check("document", True, f"kind={document.get('kind')} instance={document.get('instance_id')} issued_at={document.get('issued_at')}")
    except Exception as error:  # noqa: BLE001
        check("document", False, str(error))
        return finish(checks, args.json)

    try:
        public_key = bytes.fromhex(document["public_key"])
        quote = bytes.fromhex(document["quote"])
        report = td_report(quote)
        expected = hashlib.sha512(DOMAIN + public_key + nonce).digest()
        check("report_data binds the receipt key and our nonce", report["report_data"] == expected and document.get("report_data") == expected.hex(),
              f"public_key={public_key.hex()[:16]}… nonce={nonce.hex()[:16]}…")
    except Exception as error:  # noqa: BLE001
        check("report_data binds the receipt key and our nonce", False, str(error))
        return finish(checks, args.json)

    event_log = document.get("event_log")
    if isinstance(event_log, str):
        try:
            event_log = json.loads(event_log)
        except json.JSONDecodeError:
            event_log = None
    if isinstance(event_log, list):
        try:
            replayed = replay(event_log)
            matches = all(replayed[i] == report[f"rtmr{i}"] for i in range(4))
            check("event log replays into the quote's RTMRs", matches, "rtmr3=" + report["rtmr3"].hex()[:24] + "…")
        except Exception as error:  # noqa: BLE001
            check("event log replays into the quote's RTMRs", False, str(error))
        info = document.get("info") or {}
        compose = event_payload(event_log, "compose-hash")
        os_image = event_payload(event_log, "os-image-hash")
        check("compose hash in the measured log equals the stated one", bool(compose) and compose == info.get("compose_hash"), f"compose_hash={compose}")
        if args.compose_hash:
            check("compose hash equals RELEASES.md", compose == args.compose_hash.lower(), f"expected={args.compose_hash}")
        check("os image hash in the measured log equals the stated one", bool(os_image) and os_image == info.get("os_image_hash"), f"os_image_hash={os_image}")
        if args.os_image_hash:
            check("os image hash equals the expected dstack image", os_image == args.os_image_hash.lower(), f"expected={args.os_image_hash}")
    else:
        check("event log replays into the quote's RTMRs", False, "the document carries no event log")

    status, detail = dcap_qvl(quote)
    check("Intel signature chain over the quote (dcap-qvl)", True if status == "passed" else (None if status == "skipped" else False), detail)
    check("mr_td", True, report["mr_td"].hex())
    return finish(checks, args.json)


def finish(checks: list[dict], as_json: bool) -> int:
    failed = [c for c in checks if c["result"] == "failed"]
    skipped = [c for c in checks if c["result"] == "skipped"]
    if as_json:
        print(json.dumps({"checks": checks, "verified": not failed, "skipped": len(skipped)}, indent=2))
    else:
        for c in checks:
            mark = {"passed": "ok  ", "failed": "FAIL", "skipped": "skip"}[c["result"]]
            print(f"[{mark}] {c['check']}: {c['detail']}")
        print("verified" if not failed else "NOT VERIFIED", f"({len(skipped)} check(s) skipped)" if skipped else "")
    return 0 if not failed else 1


if __name__ == "__main__":
    sys.exit(main())
