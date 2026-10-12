# Secret Redaction HTTP Hook Server

A lightweight HTTP hook server that redacts secrets from tool output. Designed for use with Claude Code `PostToolUse` hooks and the Torana `hooks` plugin (resolves issue [#105](https://github.com/torana-edge/torana-plugins/issues/105)).

Implemented purely with the Python standard library with **zero external dependencies**.

---

## Features

- Listens on `127.0.0.1:8099` and handles `POST /post-tool-use`.
- Detects common secret patterns:
  - OpenAI / Anthropic API keys (`sk-...`, `sk_test_...`)
  - AWS access keys (`AKIA...`)
  - GitHub personal access tokens (`ghp_...`, `gho_...`)
  - PEM private keys (`-----BEGIN ... PRIVATE KEY-----`)
- Preserves the exact structure of tool outputs while replacing credentials with `[REDACTED]`.
- Returns `200` with an empty response when no secrets are present.

---

## Configuration in Claude Code

Add the following to your Claude Code `settings.json`:

```json
{
  "hooks": {
    "PostToolUse": [
      {
        "type": "http",
        "url": "http://127.0.0.1:8099/post-tool-use"
      }
    ]
  }
}
```

---

## Running the Server

```bash
# Start the hook server
python3 server.py
```

---

## Running the Tests

```bash
python3 -m unittest test_server.py
```

---

## Verified Before / After Example

### Incoming Tool Payload
```json
{
  "tool_name": "Read",
  "tool_response": {
    "path": "config.env",
    "content": "API_KEY=sk_test_not_a_real_key_1234567890\nDB_PASS=secret"
  }
}
```

### Outgoing Redacted Response
```json
{
  "hookSpecificOutput": {
    "hookEventName": "PostToolUse",
    "updatedToolOutput": {
      "path": "config.env",
      "content": "API_KEY=[REDACTED]\nDB_PASS=secret"
    }
  }
}
```
