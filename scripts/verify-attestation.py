#!/usr/bin/env python3
"""Check a running Lebrel router's hardware attestation, from the outside, with nothing but Python.

    scripts/verify-attestation.py https://<router host> --compose-hash <hex from RELEASES.md>
        [--app-id <hex>] [--public-key <hex>] [--os-image-hash <hex>] [--reference <measurements.json>]
        [--nonce <hex>] [--allow-unverified-chain] [--json]

What it checks, in order:
  1. The document at /.well-known/attestation answers, with a fresh nonce of our choosing.
  2. The TDX quote's report data equals SHA-512("lebrel-router-attestation-v2" || public_key ||
     encryption_public_key || nonce): the quote was made for this receipt-signing key, for the key
     clients encrypt requests to, and for our nonce, now (v1 documents, without an encryption key,
     used the v1 domain and no encryption key).
  3. Every runtime event (RTMR3) in the event log is recomputed from its type, name and payload
     (digest = SHA-384(le32(type) || ":" || name || ":" || payload)) and the whole log replays into
     the RTMRs the quote carries (RTMR[i] = SHA-384(RTMR[i] || digest) from 48 zero bytes). Only
     then are the payloads believed: compose-hash, os-image-hash, app-id, key-provider are read from
     the verified log, never from the document's prose.
  4. The verified compose-hash equals RELEASES.md's row; app-id and the receipt key equal what you
     expect when given; the document's own statements equal the verified log.
  5. With --reference, the quote's mr_td and rtmr0-2 (the OS image, measured before any app event)
     equal the reference measurements in the file. Today those are values OBSERVED on Phala's
     machines, not an independent rebuild of the image (dstack publishes a different tdx_mrtd for
     mkosi-os-v0.6.1, not yet reconciled): until reproduced, the OS identity rests on Phala.
  5b. mr_config_id is recomputed from the verified app id, compose hash, instance id and the KMS
     identity in --reference (0x03 || SHA-256("dstack-mr-config-v3:" || 0x00 || JCS(...))): a
     hardware-measured record that binds app, compose, instance and KMS without the event log.
  5c. With --compose-file, the measured app manifest (info.tcb_info.app_compose) hashes to the
     measured compose hash and its docker_compose_file equals the file in the repository; the
     pre-launch script's hash, allowed_envs and the deployment flags are printed.
  6. Intel's signature chain over the quote and the platform's TCB status, through `dcap-qvl`
     (cargo install dcap-qvl-cli). Without it the result is NOT "verified" unless you pass
     --allow-unverified-chain: steps 1-5 prove the binding, step 6 proves the hardware.

Exit code 0 only when every check passed (2 when the chain could not be checked and you did not
allow that); a skipped step is reported, never hidden.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import secrets
import shutil
import struct
import subprocess
import sys
import tempfile
import urllib.request

DOMAIN_V1 = b"lebrel-router-attestation-v1"
DOMAIN_V2 = b"lebrel-router-attestation-v2"
QUOTE_HEADER = 48
TD_REPORT_OFFSETS = {
    "tee_tcb_svn": (0, 16), "mr_seam": (16, 48), "mr_signer_seam": (64, 48), "seam_attributes": (112, 8),
    "td_attributes": (120, 8), "xfam": (128, 8), "mr_td": (136, 48), "mr_config_id": (184, 48),
    "mr_owner": (232, 48), "mr_owner_config": (280, 48), "rtmr0": (328, 48), "rtmr1": (376, 48),
    "rtmr2": (424, 48), "rtmr3": (472, 48), "report_data": (520, 64),
}
TD_REPORT10_SIZE = 584
TEE_TYPE_TDX = 0x81


class Unavailable(Exception):
    """The document could not be obtained (a 429 that did not clear, a network failure): not a
    verification failure, the machine was not reached."""


def fetch(base: str, nonce: bytes) -> dict:
    """Fetches the document; a 429 (the machine is quoting for others) is retried after Retry-After,
    up to three times."""
    url = base.rstrip("/") + "/.well-known/attestation?nonce=" + nonce.hex()
    import time
    import urllib.error
    for attempt in range(4):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers={"Accept": "application/json"}), timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code == 429 and attempt < 3:
                time.sleep(min(float(error.headers.get("Retry-After") or 5), 30))
                continue
            raise Unavailable(f"HTTP {error.code} from the router") from error
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            raise Unavailable(str(error)) from error
    raise Unavailable("the router kept answering 429")


def td_report(quote: bytes) -> dict[str, bytes]:
    """The TD report body of a TDX quote: version 4 carries it right after the 48-byte header;
    version 5 puts a 2-byte body type and a 4-byte body size first (TD report 1.0 = type 2,
    TD report 1.5 = type 3, whose first 584 bytes are the same fields)."""
    if len(quote) < QUOTE_HEADER + 6:
        raise ValueError(f"quote is {len(quote)} bytes, shorter than a TDX quote header")
    version = int.from_bytes(quote[0:2], "little")
    tee_type = int.from_bytes(quote[4:8], "little")
    if tee_type != TEE_TYPE_TDX:
        raise ValueError(f"not a TDX quote: tee type {tee_type:#x}")
    if version == 4:
        start = QUOTE_HEADER
    elif version == 5:
        body_type = int.from_bytes(quote[QUOTE_HEADER:QUOTE_HEADER + 2], "little")
        if body_type not in (2, 3):
            raise ValueError(f"quote v5 body type {body_type} is not a TD report")
        start = QUOTE_HEADER + 6
    else:
        raise ValueError(f"unsupported quote version {version}")
    body = quote[start:start + TD_REPORT10_SIZE]
    if len(body) < TD_REPORT10_SIZE:
        raise ValueError("quote is truncated before the end of the TD report")
    return {name: body[offset:offset + size] for name, (offset, size) in TD_REPORT_OFFSETS.items()}


def event_digest(event: dict) -> bytes:
    """dstack's runtime (RTMR3) events: digest = SHA-384(le32(event_type) || ":" || event || ":" ||
    payload), the payload being the bytes the hex in the log encodes (or the text itself when it is
    not hex)."""
    event_type = int(event.get("event_type", 0))
    name = str(event.get("event", ""))
    payload = str(event.get("event_payload", ""))
    try:
        payload_bytes = bytes.fromhex(payload)
    except ValueError:
        payload_bytes = payload.encode()
    return hashlib.sha384(struct.pack("<I", event_type) + b":" + name.encode() + b":" + payload_bytes).digest()


def replay(event_log: list[dict]) -> tuple[dict[int, bytes], list[str]]:
    """Replays the log into RTMR0-3 and returns the names of the RTMR3 events whose digest does not
    match their own type, name and payload: such an event's payload cannot be believed."""
    rtmrs = {i: bytes(48) for i in range(4)}
    unverified: list[str] = []
    for event in event_log:
        imr = int(event.get("imr", -1))
        digest = bytes.fromhex(str(event.get("digest", "")))
        if imr not in rtmrs or len(digest) != 48:
            raise ValueError(f"event log entry malformed: {event}")
        if imr == 3 and event_digest(event) != digest:
            unverified.append(str(event.get("event", "?")))
        rtmrs[imr] = hashlib.sha384(rtmrs[imr] + digest).digest()
    return rtmrs, unverified


def verified_payload(event_log: list[dict], name: str) -> str | None:
    """The payload of the first RTMR3 event called name, only if its digest recomputes."""
    for event in event_log:
        if int(event.get("imr", -1)) == 3 and event.get("event") == name:
            digest = bytes.fromhex(str(event.get("digest", "")))
            return str(event.get("event_payload", "")) if event_digest(event) == digest else None
    return None


def dcap_qvl(quote: bytes) -> tuple[str, str]:
    """('passed'|'failed'|'skipped', detail). Passed only when the signature chain verifies AND the
    platform TCB status is UpToDate (an OutOfDate or Revoked platform is a failure, not a pass)."""
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
    if run.returncode != 0:
        return "failed", output[-400:]
    status = None
    statuses_not_up_to_date: dict[str, str] = {}
    advisories: list = []
    for line in output.splitlines():
        line = line.strip()
        if line.startswith("{"):
            try:
                report = json.loads(line)
            except json.JSONDecodeError:
                continue
            found = {"overall": report.get("status"), "platform": (report.get("platform_status") or {}).get("status"), "qe": (report.get("qe_status") or {}).get("status")}
            found = {k: v for k, v in found.items() if v}
            if found:
                status = "seen"
                statuses_not_up_to_date = {k: v for k, v in found.items() if v != "UpToDate"}
                advisories = (report.get("platform_status") or {}).get("advisory_ids") or report.get("advisory_ids") or []
    if status is None:
        return "failed", "dcap-qvl verified the signature but reported no TCB status: " + output[-200:]
    if statuses_not_up_to_date:
        return "failed", f"signature chain ok but TCB status is not UpToDate: {statuses_not_up_to_date} (advisories {advisories})"
    return "passed", "signature chain ok, TCB UpToDate (platform, QE and overall)"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("base_url")
    parser.add_argument("--compose-hash", required=True, help="the compose hash RELEASES.md lists for the served version")
    parser.add_argument("--app-id", help="the dstack app id the router is expected to run as")
    parser.add_argument("--public-key", help="the receipt-signing public key (hex) you expect, e.g. from a receipt's manifest")
    parser.add_argument("--os-image-hash", help="the dstack OS image hash expected")
    parser.add_argument("--reference", help="JSON file with reference measurements: mr_td, rtmr0, rtmr1, rtmr2 (hex) and kms_id (the KMS's key_provider_id)")
    parser.add_argument("--key-id", help="the receipt key id (hex) RELEASES.md lists for the row")
    parser.add_argument("--compose-file", help="path to the compose file of the row: the measured app manifest must carry it byte for byte")
    parser.add_argument("--nonce", help="hex nonce (default: 32 random bytes)")
    parser.add_argument("--allow-unverified-chain", action="store_true", help="count the result as verified even when dcap-qvl is not installed")
    parser.add_argument("--json", action="store_true", help="print the checks as JSON")
    args = parser.parse_args()

    nonce = bytes.fromhex(args.nonce) if args.nonce else secrets.token_bytes(32)
    checks: list[dict] = []

    def check(name: str, ok: bool | None, detail: str) -> None:
        checks.append({"check": name, "result": "passed" if ok else ("skipped" if ok is None else "failed"), "detail": detail})

    try:
        document = fetch(args.base_url, nonce)
        check("document", True, f"kind={document.get('kind')} instance={document.get('instance_id')} issued_at={document.get('issued_at')} key_source={document.get('key_source')}")
    except Unavailable as error:
        print(f"NOT OBTAINED: {error}")
        return 3
    except Exception as error:  # noqa: BLE001
        check("document", False, str(error))
        return finish(checks, args)

    try:
        public_key = bytes.fromhex(document["public_key"])
        quote = bytes.fromhex(document["quote"])
        report = td_report(quote)
        scheme = str(document.get("scheme", ""))
        encryption_key = bytes.fromhex(document.get("encryption_public_key") or "")
        if "attestation-v2" in scheme:
            expected = hashlib.sha512(DOMAIN_V2 + public_key + encryption_key + nonce).digest()
            what = "report_data binds the receipt key, the encryption key and our nonce"
        else:
            expected = hashlib.sha512(DOMAIN_V1 + public_key + nonce).digest()
            what = "report_data binds the receipt key and our nonce (v1 document, no encryption key)"
        check(what, report["report_data"] == expected and document.get("report_data") == expected.hex(),
              f"public_key={public_key.hex()} encryption_key={encryption_key.hex() or '(none)'} nonce={nonce.hex()[:16]}…")
        if args.public_key:
            check("receipt key equals the expected one", public_key.hex() == args.public_key.lower(), f"expected={args.public_key}")
        if args.key_id:
            check("receipt key id equals RELEASES.md", str(document.get("signing_key_id", "")).lower() == args.key_id.lower(), f"document={document.get('signing_key_id')}")
    except Exception as error:  # noqa: BLE001
        check("report_data binds the receipt key and our nonce", False, str(error))
        return finish(checks, args)

    event_log = document.get("event_log")
    if isinstance(event_log, str):
        try:
            event_log = json.loads(event_log)
        except json.JSONDecodeError:
            event_log = None
    if not isinstance(event_log, list):
        check("event log replays into the quote's RTMRs", False, "the document carries no event log")
        return finish(checks, args)

    try:
        replayed, unverified = replay(event_log)
        matches = all(replayed[i] == report[f"rtmr{i}"] for i in range(4))
        check("event log replays into the quote's RTMRs", matches, "rtmr3=" + report["rtmr3"].hex()[:24] + "…")
        check("every runtime event's digest recomputes from its payload", not unverified, "unverified: " + ", ".join(unverified) if unverified else f"{sum(1 for e in event_log if int(e.get('imr', -1)) == 3)} RTMR3 events")
    except Exception as error:  # noqa: BLE001
        check("event log replays into the quote's RTMRs", False, str(error))
        return finish(checks, args)

    info = document.get("info") or {}
    compose = verified_payload(event_log, "compose-hash")
    os_image = verified_payload(event_log, "os-image-hash")
    app_id = verified_payload(event_log, "app-id")
    key_provider = verified_payload(event_log, "key-provider")
    check("compose hash equals RELEASES.md (from the verified log)", compose == args.compose_hash.lower(), f"measured={compose} expected={args.compose_hash}")
    check("document states the measured compose hash", compose is not None and info.get("compose_hash") == compose, f"stated={info.get('compose_hash')}")
    check("document states the measured app id", app_id is not None and info.get("app_id") == app_id, f"measured app_id={app_id}")
    if args.app_id:
        check("app id equals the expected one", app_id == args.app_id.lower(), f"expected={args.app_id}")
    check("key provider is the KMS (measured)", bool(key_provider) and '"kms"' in bytes.fromhex(key_provider).decode(errors="replace"), f"key-provider={bytes.fromhex(key_provider).decode(errors='replace')[:60] if key_provider else None}")
    check("document states the measured os image hash", os_image is not None and info.get("os_image_hash") == os_image, f"measured={os_image}")
    if args.os_image_hash:
        check("os image hash equals the expected dstack image", os_image == args.os_image_hash.lower(), f"expected={args.os_image_hash}")

    instance_id = verified_payload(event_log, "instance-id")
    if args.reference:
        try:
            reference = json.load(open(args.reference))
            for field in ("mr_td", "rtmr0", "rtmr1", "rtmr2"):
                want = str(reference.get(field, "")).lower()
                check(f"{field} equals the reference measurement (observed, see --reference)", bool(want) and report[field].hex() == want, f"quote={report[field].hex()[:24]}… reference={want[:24]}…")
            kms_id = str(reference.get("kms_id", "")).lower()
            if kms_id and key_provider:
                provider = json.loads(bytes.fromhex(key_provider).decode())
                check("key provider id equals the reference KMS", str(provider.get("id", "")).lower() == kms_id, f"measured id={str(provider.get('id',''))[:24]}…")
                if app_id and compose and instance_id:
                    record = {"app_id": app_id, "compose_hash": compose, "init_script_hashes": [], "instance_id": instance_id,
                              "key_provider": provider.get("name"), "key_provider_id": provider.get("id"), "version": 3}
                    jcs = json.dumps(record, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
                    expected_config = (b"\x03" + hashlib.sha256(b"dstack-mr-config-v3:" + b"\x00" + jcs).digest()).ljust(48, b"\x00")
                    check("mr_config_id recomputes from app id, compose, instance and KMS", report["mr_config_id"] == expected_config, f"mr_config_id={report['mr_config_id'].hex()[:24]}…")
        except Exception as error:  # noqa: BLE001
            check("reference measurements", False, str(error))
    else:
        check("mr_td / rtmr0-2 against reference measurements", None, f"no --reference given; mr_td={report['mr_td'].hex()}")

    if args.compose_file:
        try:
            tcb = json.loads(info.get("tcb_info") or "{}")
            manifest_text = tcb.get("app_compose") or ""
            measured = hashlib.sha256(manifest_text.encode()).hexdigest()
            check("measured app manifest hashes to the measured compose hash", bool(manifest_text) and measured == compose, f"sha256(app_compose)={measured[:24]}…")
            manifest = json.loads(manifest_text) if manifest_text else {}
            expected_compose = open(args.compose_file, encoding="utf-8").read()
            # Trailing whitespace is not part of a compose: a dashboard paste drops the final newline.
            check("the manifest's docker_compose_file equals the repository file", str(manifest.get("docker_compose_file", "")).rstrip() == expected_compose.rstrip(), f"file={args.compose_file}")
            prelaunch = hashlib.sha256(str(manifest.get("pre_launch_script", "")).encode()).hexdigest()
            flags = {k: manifest.get(k) for k in ("public_logs", "public_sysinfo", "public_tcbinfo", "key_provider", "kms_enabled", "secure_time", "gateway_enabled")}
            check("manifest details", True, f"pre_launch_script sha256={prelaunch[:16]}… allowed_envs={manifest.get('allowed_envs')} flags={flags}")
        except Exception as error:  # noqa: BLE001
            check("measured app manifest", False, str(error))

    status, detail = dcap_qvl(quote)
    check("Intel signature chain and TCB status (dcap-qvl)", True if status == "passed" else (None if status == "skipped" else False), detail)
    return finish(checks, args)


def finish(checks: list[dict], args: argparse.Namespace) -> int:
    failed = [c for c in checks if c["result"] == "failed"]
    chain_skipped = any(c["result"] == "skipped" and c["check"].startswith("Intel signature chain") for c in checks)
    skipped = [c for c in checks if c["result"] == "skipped"]
    verified = not failed and (not chain_skipped or args.allow_unverified_chain)
    if args.json:
        print(json.dumps({"checks": checks, "verified": verified, "chain_checked": not chain_skipped}, indent=2))
    else:
        for c in checks:
            mark = {"passed": "ok  ", "failed": "FAIL", "skipped": "skip"}[c["result"]]
            print(f"[{mark}] {c['check']}: {c['detail']}")
        if verified:
            print("verified" + (" (Intel's chain not checked here, allowed by flag)" if chain_skipped else ""))
        elif failed:
            print("NOT VERIFIED")
        else:
            print("NOT VERIFIED: Intel's chain was not checked (install dcap-qvl or pass --allow-unverified-chain)")
    if failed:
        return 1
    if chain_skipped and not args.allow_unverified_chain:
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
