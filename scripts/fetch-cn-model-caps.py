#!/usr/bin/env python3
"""Fetch the CN model capability table from the live upstream and diff it against
what codebuddy2api currently ships.

Why this exists: `/v3/config` is the only authoritative source for two things
that silently break requests —

  reasoning.supportedEfforts   the exact set of reasoning_effort values a model
                               accepts. Sending anything else is 11150
                               ("not supported by the current model"), and
                               *omitting* the field is NOT the same as sending a
                               supported value (glm-5.1 is 11150 with no effort
                               and 200 with any of low/medium/high/max).

  presence in the catalog     a model missing here is 11102 ("model [x] service
                               info not found"). glm-5.1 is in neither the CLI
                               nor the IDE catalog anymore, yet our shipped
                               default model list still leads with it.

Usage (needs a live CN credential; this script never prints tokens):

    CK=$(ssh nuoapi-hk 'sudo -n ...select api_key from accounts where id=1')
    COD_BUDDY_KEY="$CK" python3 scripts/fetch-cn-model-caps.py

Output is a JSON table you can paste into internal/app/modelcaps.go.
"""
from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request

BASE = "https://copilot.tencent.com"
# /v3/config refuses requests whose UA does not look like a coding client
# (code 12403 "check ua, get coding copilot version error"), so both probes carry
# a real client UA. The IDE UA returns the richer catalog (37 models vs 26).
IDE_UA = "CodeBuddyIDE/5.9.3"
CLI_UA = "CLI/2.149.0 CodeBuddy/2.149.0"

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def probe(key: str, ua: str) -> dict:
    request = urllib.request.Request(BASE + "/v3/config", method="GET")
    request.add_header("X-Api-Key", key)
    request.add_header("Accept", "application/json")
    request.add_header("X-Domain", "copilot.tencent.com")
    request.add_header("X-Product", "SaaS")
    request.add_header("User-Agent", ua)
    try:
        with OPENER.open(request, timeout=30) as response:
            payload = json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise SystemExit(f"/v3/config {error.code}: {error.read()[:200]!r}") from None
    if payload.get("code") != 0:
        raise SystemExit(f"/v3/config code {payload.get('code')}: {payload.get('msg')}")
    return payload.get("data") or {}


def capabilities(data: dict) -> dict:
    out = {}
    for model in data.get("models") or []:
        model_id = model.get("id")
        if not model_id:
            continue
        reasoning = model.get("reasoning") or {}
        out[model_id] = {
            "efforts": reasoning.get("supportedEfforts") or [],
            "default_effort": reasoning.get("effort") or reasoning.get("defaultEffort"),
            "can_disable_thinking": reasoning.get("canDisableThinking"),
        }
    return out


def main() -> int:
    key = (os.environ.get("COD_BUDDY_KEY") or "").strip()
    if not key:
        print("COD_BUDDY_KEY is not set", file=sys.stderr)
        return 2
    merged = {}
    for ua in (IDE_UA, CLI_UA):
        try:
            caps = capabilities(probe(key, ua))
        except SystemExit as error:
            print(f"probe with {ua!r} failed: {error}", file=sys.stderr)
            continue
        print(f"# {ua}: {len(caps)} models", file=sys.stderr)
        for model_id, cap in caps.items():
            previous = merged.get(model_id)
            if previous is None or (not previous["efforts"] and cap["efforts"]):
                merged[model_id] = cap
    print(json.dumps(dict(sorted(merged.items())), indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
