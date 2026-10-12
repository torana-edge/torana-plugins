#!/usr/bin/env python3
"""Example HTTP hook server that redacts secrets from tool output for Claude Code.

Complies with torana-plugins issue #104 / #105:
- Listens on 127.0.0.1:8099
- Handles POST /post-tool-use
- Matches secret patterns (sk-, AKIA, ghp_, PRIVATE KEY)
- Replaces secrets with [REDACTED] preserving exact output shape
- Uses only Python standard library
"""

from __future__ import annotations

import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

HOST = "127.0.0.1"
PORT = 8099

SECRET_PATTERNS = [
    re.compile(r"sk-[A-Za-z0-9_-]{20,}"),
    re.compile(r"sk_test_[A-Za-z0-9_-]{10,}"),
    re.compile(r"AKIA[0-9A-Z]{16}"),
    re.compile(r"ghp_[A-Za-z0-9]{36}"),
    re.compile(r"gho_[A-Za-z0-9]{36}"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----"),
]


def redact_string(value: str) -> tuple[str, bool]:
    """Redact known secret patterns from a string value."""
    modified = False
    result = value
    for pattern in SECRET_PATTERNS:
        new_result, count = pattern.subn("[REDACTED]", result)
        if count > 0:
            modified = True
            result = new_result
    return result, modified


def recursively_redact(data: Any) -> tuple[Any, bool]:
    """Recursively traverse arbitrary JSON structures and redact secrets in strings."""
    if isinstance(data, str):
        return redact_string(data)
    elif isinstance(data, dict):
        new_dict: dict[str, Any] = {}
        any_modified = False
        for k, v in data.items():
            redacted_v, modified = recursively_redact(v)
            if modified:
                any_modified = True
            new_dict[k] = redacted_v
        return new_dict, any_modified
    elif isinstance(data, list):
        new_list: list[Any] = []
        any_modified = False
        for item in data:
            redacted_item, modified = recursively_redact(item)
            if modified:
                any_modified = True
            new_list.append(redacted_item)
        return new_list, any_modified
    return data, False


class RedactHookHandler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        if self.path != "/post-tool-use":
            self.send_response(404)
            self.end_headers()
            return

        content_length = int(self.headers.get("Content-Length", 0))
        post_data = self.rfile.read(content_length)

        try:
            payload = json.loads(post_data.decode("utf-8")) if post_data else {}
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            return

        tool_output = payload.get("tool_response") or payload.get("tool_output")
        if tool_output is None:
            self.send_response(200)
            self.end_headers()
            return

        redacted_output, was_redacted = recursively_redact(tool_output)

        if was_redacted:
            response_body = {
                "hookSpecificOutput": {
                    "hookEventName": "PostToolUse",
                    "updatedToolOutput": redacted_output,
                }
            }
            body_bytes = json.dumps(response_body).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body_bytes)))
            self.end_headers()
            self.wfile.write(body_bytes)
        else:
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()

    def log_message(self, format: str, *args: Any) -> None:
        # Standard silent logging or uncomment for debugging
        pass


def run_server(host: str = HOST, port: int = PORT) -> None:
    server_address = (host, port)
    httpd = HTTPServer(server_address, RedactHookHandler)
    print(f"Redaction hook server running at http://{host}:{port}/post-tool-use")
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down hook server...")
        httpd.server_close()


if __name__ == "__main__":
    run_server()
