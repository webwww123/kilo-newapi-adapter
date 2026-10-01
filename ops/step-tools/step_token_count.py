"""Offline official Step tokenizer study, never a production budget override.

Only public, pinned tokenizer/template assets are loaded. Customer payloads
remain in memory. Counts describe the official text template, not proof of an
upstream's tokenizer, hidden prompt, media accounting, or accepted capacity.
"""
from __future__ import annotations

import hashlib
import importlib.metadata
import json
from pathlib import Path
import time

REVISION = "5f6244077ac62e04eec3f320501ff8c2b293373a"
ASSETS = {
    "tokenizer_config.json": "78202af487f4d4360e8d15cb0506d60718f8599770599c6f28f7f1fa045a591f",
    "tokenizer.json": "b564c620eb77fa11d0926011c2202347d6cfc358d79724ee04ae7007e13636f0",
    "chat_template.jinja": "f428623fc81c940c35be3509fbffc086b4b4360d8800e46103e6f34d02891633",
    "special_tokens_map.json": "d47424bda11df4cedc3f9458915c465a28e601d3b7df0e78f6dff4d7727006c4",
}


def validate_text_payload(payload):
    """Refuse structures the official text renderer would silently omit."""
    if not isinstance(payload, dict) or not isinstance(payload.get("messages"), list) or not payload["messages"]:
        raise ValueError("text_messages_required")
    for message in payload["messages"]:
        if not isinstance(message, dict) or message.get("role") not in {"system", "user", "assistant", "tool"}:
            raise ValueError("unsupported_official_template_role")
        content = message.get("content")
        if content is not None and not isinstance(content, str):
            if not isinstance(content, list) or any(
                not isinstance(block, dict) or block.get("type") != "text"
                or not isinstance(block.get("value", block.get("text")), str)
                for block in content
            ):
                raise ValueError("text_only_study")
        if "function_call" in message or "thinking" in message or "signature" in message:
            raise ValueError("unsupported_opaque_or_legacy_content")
        for call in message.get("tool_calls") or []:
            fn = call.get("function") if isinstance(call, dict) else None
            if not isinstance(fn, dict) or not isinstance(fn.get("name"), str):
                raise ValueError("unsupported_tool_call")
            args = fn.get("arguments", {})
            if isinstance(args, str):
                try:
                    args = json.loads(args)
                except ValueError:
                    raise ValueError("invalid_tool_arguments") from None
            if not isinstance(args, dict):
                raise ValueError("invalid_tool_arguments")
    tools = payload.get("tools") or []
    if not isinstance(tools, list) or any(
        not isinstance(tool, dict) or tool.get("type") != "function"
        or not isinstance(tool.get("function"), dict)
        for tool in tools
    ):
        raise ValueError("function_tools_required")


def verify_assets(folder):
    folder = Path(folder)
    manifest = json.loads((folder / "artifact-manifest.json").read_text())
    if manifest.get("revision") != REVISION or manifest.get("repository") != "stepfun-ai/Step-3.7-Flash":
        raise ValueError("public_revision_drift")
    for name, digest in ASSETS.items():
        if hashlib.sha256((folder / name).read_bytes()).hexdigest() != digest:
            raise ValueError("public_asset_checksum_mismatch")
    return manifest


class OfficialStepCounter:
    def __init__(self, folder):
        self.folder = Path(folder)
        self.manifest = verify_assets(self.folder)
        if importlib.metadata.version("tokenizers") != "0.22.2" or importlib.metadata.version("jinja2") != "3.1.6":
            raise ValueError("counting_dependency_drift")
        from tokenizers import Tokenizer
        from jinja2.sandbox import ImmutableSandboxedEnvironment

        started = time.perf_counter()
        self.tokenizer = Tokenizer.from_file(str(self.folder / "tokenizer.json"))
        if self.tokenizer.truncation is not None or self.tokenizer.padding is not None:
            raise ValueError("tokenizer_must_not_truncate_or_pad")
        self.config = json.loads((self.folder / "tokenizer_config.json").read_text())
        environment = ImmutableSandboxedEnvironment(trim_blocks=True, lstrip_blocks=True)
        environment.filters["tojson"] = lambda value, **kwargs: json.dumps(value, **kwargs)
        environment.filters["fromjson"] = json.loads
        self.template = environment.from_string((self.folder / "chat_template.jinja").read_text())
        self.load_ms = (time.perf_counter() - started) * 1000

    def count_text(self, text):
        return len(self.tokenizer.encode(text, add_special_tokens=False).ids)

    def count(self, payload):
        validate_text_payload(payload)
        arguments = {"messages": payload["messages"], "tools": payload.get("tools") or [],
                     "bos_token": self.config["bos_token"], "eos_token": self.config["eos_token"],
                     "add_generation_prompt": True}
        if "reasoning_effort" in payload:
            arguments["reasoning_effort"] = payload["reasoning_effort"]
        started = time.perf_counter()
        rendered = self.template.render(**arguments)
        tokens = self.count_text(rendered)
        return {"official_template_tokens": tokens, "rendered_bytes": len(rendered.encode()),
                "count_wall_ms": (time.perf_counter() - started) * 1000}

    def tools_overhead(self, tools):
        # Difference of the same empty, text-only request isolates the schema
        # and official tools wrapper. It does not isolate provider hidden text.
        empty = {"messages": [{"role": "user", "content": ""}]}
        with_tools = self.count({**empty, "tools": tools})
        without_tools = self.count(empty)
        return {"official_tools_and_wrapper_tokens": with_tools["official_template_tokens"] - without_tools["official_template_tokens"],
                "tools_canonical_json_tokens": self.count_text(json.dumps(tools, ensure_ascii=False, sort_keys=True, separators=(",", ":"))),
                "tools_count_wall_ms": with_tools["count_wall_ms"] + without_tools["count_wall_ms"]}

    def provenance(self):
        return {"official_revision": REVISION, "public_assets_sha256": ASSETS,
                "tokenizers_version": "0.22.2", "jinja2_version": "3.1.6", "load_ms": self.load_ms,
                "tokenizer_config_model_max_length": self.config.get("model_max_length"),
                "truncation_enabled": False, "runtime_model_identity_verified": False,
                "hidden_provider_prompt_known": False, "production_budget_changed": False,
                "remote_model_code_executed": False, "customer_text_saved": False}
