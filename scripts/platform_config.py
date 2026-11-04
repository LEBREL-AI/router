"""Emit the platform configuration for a routes file: BILLED_MODEL_IDS, INFERENCE_MODEL_ROUTES_JSON
and the PUBLIC_MODEL_RELEASES_JSON entries whose billing hash equals the router's.

    python3 scripts/platform_config.py routes.production.json https://router.lebrel.ai > platform-config.json
"""
from __future__ import annotations

import json
import sys


def release(route: dict) -> dict:
    pricing = route["pricing"]
    entry = {
        "displayName": route.get("display_name") or route["model_id"],
        "baseModel": route.get("base_model") or route["model_id"],
        "trafficOpen": True,
        "routed": True,
        "nativeTokens": route["max_context_tokens"],
        "validatedMaxTokens": route["max_context_tokens"],
        "servedContextTokens": route["max_context_tokens"],
        "servedOutputTokens": route["max_output_tokens"],
        "pricing": {
            "version": pricing["version"],
            "effectiveAt": pricing["effective_at"],
            "inputMicrousdPerMillionTokens": pricing["input_microusd_per_million_tokens"],
            "cachedInputMicrousdPerMillionTokens": pricing["cached_input_microusd_per_million_tokens"],
            "outputMicrousdPerMillionTokens": pricing["output_microusd_per_million_tokens"],
            "longInputMicrousdPerMillionTokens": pricing["input_microusd_per_million_tokens"],
            "longCachedInputMicrousdPerMillionTokens": pricing["cached_input_microusd_per_million_tokens"],
            "longOutputMicrousdPerMillionTokens": pricing["output_microusd_per_million_tokens"],
            "reservationSafetyBps": pricing.get("reservation_bps", 15000),
            "missingUsagePolicy": "reserved",
            "longContextThresholdTokens": route["max_context_tokens"] - 1,
        },
    }
    peak = pricing.get("peak")
    if peak:
        entry["pricing"]["peak"] = {"multiplierPercent": peak["multiplier_percent"], "weekdays": peak.get("weekdays", []),
                                   "windowsUtc": peak["windows_utc"], "holidaysUtc": peak.get("holidays_utc", [])}
    return entry


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    routes = json.load(open(argv[1], encoding="utf-8"))["routes"]
    origin = argv[2].rstrip("/")
    ids = [r["model_id"] for r in routes]
    edge_routes = {r["model_id"]: f"{origin}/m/{r['model_id'].replace('/', '--')}/v1" for r in routes}
    releases = {r["model_id"]: release(r) for r in routes}
    print(json.dumps({"BILLED_MODEL_IDS": ",".join(ids), "INFERENCE_MODEL_ROUTES_JSON": json.dumps(edge_routes, separators=(",", ":")),
                      "PUBLIC_MODEL_RELEASES_JSON": json.dumps(releases, separators=(",", ":"))}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
