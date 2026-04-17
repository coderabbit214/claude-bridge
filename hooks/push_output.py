#!/usr/bin/env python3
"""
Claude Code hook: push_output.py
Placed in .claude/hooks/ and triggered by Claude Code's Hook system.

Reads the hook event from stdin, formats it, and writes to the named pipe
that the Go bridge is reading. The Go bridge then forwards it to WeChat.

Usage in .claude/settings.json:
{
  "hooks": {
    "PostToolUse": [
      {"matcher": ".*", "hooks": [{"type": "command", "command": "python3 ~/.claude/hooks/push_output.py"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "python3 ~/.claude/hooks/push_output.py --final"}]}
    ]
  }
}
"""

import sys
import json
import os
import time
import argparse
from typing import Any, Iterable
from pathlib import Path

PIPE      = os.environ.get("CLAUDE_HOOK_PIPE", "")
SID       = os.environ.get("CLAUDE_SESSION_ID", "")
MAX_LEN   = 3500  # stay under WeChat's 4000-char limit per chunk
STATE_DIR = Path("/tmp/claude-bridge-state")
ELICIT_DIR = Path("/tmp/claude-bridge-elicit")
PERM_DIR   = Path("/tmp/claude-bridge-perm")
INTERACT_DIR = Path("/tmp/claude-bridge-interact")
CONTROL_PIPE = Path("/tmp/claude-bridge-control.pipe")
REPLACED_INTERACTION_TOKEN = "__claude_bridge_replaced__"

# When not launched by the bridge, derive PIPE/SID from the parent PID.
# Pipes, manifest, and the input daemon are created in the SessionStart hook
# and torn down in the SessionEnd hook — not here on every invocation.
if not PIPE:
    _ppid = os.getppid()
    SID  = f"local-{_ppid}"
    PIPE = f"/tmp/claude-bridge-hooks/{_ppid}.pipe"
elif not SID:
    SID = "?"


def _ambient_session_setup() -> None:
    """Called from the SessionStart hook for sessions not launched by the bridge.

    Creates the output pipe, input pipe, and manifest so the bridge can discover
    the session and open the input pipe for bidirectional interaction.
    Also starts the long-lived input-injection daemon (once per Claude process).
    """
    import json as _json
    import subprocess as _sp

    ppid = os.getppid()
    sid  = SID  # already derived at module level

    os.makedirs("/tmp/claude-bridge-hooks", exist_ok=True)
    os.makedirs("/tmp/claude-bridge-sessions", exist_ok=True)

    # Output pipe
    if not os.path.exists(PIPE):
        try:
            os.mkfifo(PIPE, 0o600)
        except FileExistsError:
            pass

    # Input pipe
    input_pipe = f"/tmp/claude-input-{sid}.pipe"
    if not os.path.exists(input_pipe):
        try:
            os.mkfifo(input_pipe, 0o600)
        except FileExistsError:
            pass

    # Manifest (bridge polls this directory to discover new sessions)
    manifest_path = f"/tmp/claude-bridge-sessions/{sid}.json"
    if not os.path.exists(manifest_path):
        cwd = os.getcwd()
        try:
            out = _sp.check_output(
                ["lsof", "-p", str(ppid), "-a", "-d", "cwd", "-Fn"],
                stderr=_sp.DEVNULL,
            )
            for line in out.decode().split("\n"):
                if line.startswith("n"):
                    cwd = line[1:]
                    break
        except Exception:
            pass
        try:
            with open(manifest_path, "w") as f:
                _json.dump(
                    {"id": sid, "pid": ppid, "work_dir": cwd,
                     "input_pipe": input_pipe, "output_pipe": PIPE},
                    f, ensure_ascii=False,
                )
        except Exception:
            pass

    _write_control_command({
        "action": "register",
        "id": sid,
        "pid": ppid,
        "work_dir": os.getcwd(),
        "input_pipe": input_pipe,
        "output_pipe": PIPE,
    })

    # Start input daemon (once per Claude process).
    # The daemon reads from input_pipe and injects keystrokes via TIOCSTI.
    daemon_pid_file = f"/tmp/claude-bridge-daemon-{ppid}.pid"
    need_daemon = True
    if os.path.exists(daemon_pid_file):
        try:
            dpid = int(Path(daemon_pid_file).read_text().strip())
            os.kill(dpid, 0)
            need_daemon = False
        except (OSError, ValueError, FileNotFoundError):
            pass
    if need_daemon:
        d = _sp.Popen(
            [sys.executable, __file__,
             "--input-daemon", input_pipe, manifest_path, PIPE],
            stdin=_sp.DEVNULL, stdout=_sp.DEVNULL, stderr=_sp.DEVNULL,
        )
        try:
            Path(daemon_pid_file).write_text(str(d.pid))
        except Exception:
            pass


def _ambient_session_teardown() -> None:
    """Called from the SessionEnd hook for sessions not launched by the bridge.

    Removes the manifest, which signals the bridge to drop the session from /list.
    The daemon detects the manifest removal and cleans up the pipes.
    """
    ppid = os.getppid()
    sid  = SID
    _write_control_command({"action": "unregister", "id": sid})
    for path in (
        f"/tmp/claude-bridge-sessions/{sid}.json",  # manifest → signals bridge
        f"/tmp/claude-bridge-daemon-{ppid}.pid",    # daemon PID file
    ):
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass


def _write_control_command(payload: dict[str, Any]) -> None:
    if not CONTROL_PIPE.exists():
        return
    try:
        fd = os.open(str(CONTROL_PIPE), os.O_WRONLY | os.O_NONBLOCK)
    except OSError:
        return
    try:
        with os.fdopen(fd, "w") as f:
            f.write(json.dumps(payload, ensure_ascii=False))
            f.write("\n")
    except Exception:
        pass


def run_input_daemon(input_pipe: str, manifest_path: str, output_pipe: str) -> None:
    """Read from input_pipe, inject each line into Claude's terminal via TIOCSTI.

    Runs as a long-lived background process spawned on the first hook invocation.
    Cleans up all three files when the session ends (manifest or input_pipe removed).

    Uses O_RDONLY|O_NONBLOCK so the pipe is immediately registered as "open for
    reading" in the kernel.  This lets the bridge's O_WRONLY|O_NONBLOCK open
    succeed even when the bridge starts after this daemon — avoiding the deadlock
    where both sides wait for the other to open first.
    """
    import fcntl
    import select as _sel
    import termios

    tty_fd: int | None = None
    try:
        tty_fd = os.open("/dev/tty", os.O_RDWR)
    except OSError:
        pass  # No controlling terminal — input injection unavailable; output still works.

    try:
        while os.path.exists(input_pipe) and os.path.exists(manifest_path):
            # Open with O_NONBLOCK: pipe is immediately "open for reading" so the
            # bridge can connect with O_WRONLY|O_NONBLOCK without a deadlock.
            try:
                fd = os.open(input_pipe, os.O_RDONLY | os.O_NONBLOCK)
            except OSError:
                break

            try:
                while os.path.exists(manifest_path):
                    # Wait up to 1 s for data; also serves as exit-condition poll.
                    r, _, _ = _sel.select([fd], [], [], 1.0)
                    if not r:
                        continue
                    try:
                        data = os.read(fd, 4096)
                    except OSError:
                        data = b""
                    if not data:
                        # EOF: the bridge closed its write end (stopped/restarted).
                        # Break to reopen the pipe and wait for reconnection.
                        break
                    # Replace \n with \r: terminals in raw mode use CR (not LF) as
                    # the Enter signal.  Without this, text appears in the input box
                    # but is never submitted — same substitution the mux script does.
                    data = data.replace(b"\n", b"\r")
                    if tty_fd is not None:
                        try:
                            for byte in data:
                                fcntl.ioctl(tty_fd, termios.TIOCSTI, bytes([byte]))
                        except OSError:
                            pass  # TIOCSTI restricted on this macOS version — skip silently
            finally:
                try:
                    os.close(fd)
                except OSError:
                    pass

            # Brief pause before reopening to avoid busy-looping when no writer
            # is present (macOS may immediately report EOF on a pipe with no writer).
            time.sleep(0.2)
    finally:
        if tty_fd is not None:
            try:
                os.close(tty_fd)
            except OSError:
                pass
        for path in (input_pipe, manifest_path, output_pipe):
            try:
                os.unlink(path)
            except Exception:
                pass


def write_to_pipe(text: str) -> None:
    if not PIPE or not os.path.exists(PIPE):
        return
    # Use O_NONBLOCK so we never hang when the bridge isn't reading yet.
    # Retry for up to ~3 s to give the ambient watcher time to pick up new pipes.
    fd = None
    for _ in range(7):
        try:
            fd = os.open(PIPE, os.O_WRONLY | os.O_NONBLOCK)
            break
        except OSError:
            time.sleep(0.5)
    if fd is None:
        return  # bridge not running — skip silently
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text)
            if not text.endswith("\n"):
                f.write("\n")
            f.write("---END---\n")
    except Exception as e:
        print(f"[hook] pipe write error: {e}", file=sys.stderr)


def truncate(s: str, n: int) -> str:
    if len(s) <= n:
        return s
    return s[:n] + f"\n…(truncated, {len(s)} chars total)"


def iter_text(value: Any) -> Iterable[str]:
    if value is None:
        return

    if isinstance(value, str):
        text = value.strip()
        if text:
            yield text
        return

    if isinstance(value, (int, float, bool)):
        yield str(value)
        return

    if isinstance(value, list):
        for item in value:
            yield from iter_text(item)
        return

    if not isinstance(value, dict):
        return

    block_type = value.get("type")
    if block_type in {"text", "output_text"}:
        text = value.get("text")
        if isinstance(text, str) and text.strip():
            yield text.strip()
        return

    if block_type in {"tool_result", "tool_output"}:
        for key in ("output", "stdout", "stderr", "error", "result", "content"):
            yield from iter_text(value.get(key))
        return

    preferred_keys = (
        "stdout",
        "stderr",
        "output",
        "error",
        "result",
        "message",
        "content",
        "text",
        "details",
    )
    for key in preferred_keys:
        yield from iter_text(value.get(key))


def unique_lines(parts: Iterable[str]) -> str:
    seen = set()
    ordered = []
    for part in parts:
        normalized = part.strip()
        if not normalized or normalized in seen:
            continue
        seen.add(normalized)
        ordered.append(normalized)
    return "\n\n".join(ordered)


def session_key(event: dict[str, Any]) -> str:
    session_id = str(event.get("sessionId") or event.get("session_id") or "").strip()
    if session_id:
        return session_id
    return SID or "default"


def state_path(event: dict[str, Any]) -> Path:
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    return STATE_DIR / f"{session_key(event)}.json"


def load_state(event: dict[str, Any]) -> dict[str, Any]:
    path = state_path(event)
    if not path.exists():
        return {}
    try:
        return json.loads(path.read_text())
    except Exception:
        return {}


def save_state(event: dict[str, Any], data: dict[str, Any]) -> None:
    try:
        state_path(event).write_text(json.dumps(data, ensure_ascii=False))
    except Exception:
        pass


def extract_final_text(event: dict[str, Any]) -> str:
    message = event.get("message", {})
    parts = []
    if isinstance(message, dict):
        parts.extend(iter_text(message.get("content")))
    parts.extend(iter_text(event.get("content")))
    text = unique_lines(parts)
    if text:
        save_state(event, {
            "assistant_uuid": str(event.get("uuid") or ""),
            "assistant_text": text,
            "assistant_timestamp": str(event.get("timestamp") or ""),
        })
        return text
    return load_transcript_delta(event, wait_for_update=True)


# Tools whose raw output is noise — return empty to skip them.
_SILENT_TOOLS = {
    "Read", "Glob", "Grep", "LS",
    "TodoRead", "TodoWrite",
    "Task", "TaskGet", "TaskList", "TaskOutput",
    "ExitPlanMode", "EnterPlanMode",
}


def _get_resp_str(resp: Any, *keys: str) -> str:
    if isinstance(resp, dict):
        for k in keys:
            v = resp.get(k)
            if v and isinstance(v, str):
                return v.strip()
    return ""


def format_tool_output(event: dict[str, Any]) -> str:
    """Return a concise, WeChat-friendly summary for a PostToolUse event."""
    tool = str(event.get("tool_name") or "").strip()
    inp = event.get("tool_input") or {}
    resp = event.get("tool_response") or event.get("result") or {}

    if tool in _SILENT_TOOLS:
        return ""

    if tool == "Write":
        path = inp.get("file_path") or inp.get("path") or "?"
        return f"written {path}"

    if tool == "Edit":
        path = inp.get("file_path") or inp.get("path") or "?"
        return f"edited {path}"

    if tool == "NotebookEdit":
        path = inp.get("notebook_path") or inp.get("path") or "?"
        return f"edited {path}"

    if tool == "Bash":
        cmd = str(inp.get("command") or "").strip()
        stdout = _get_resp_str(resp, "stdout", "output")
        stderr = _get_resp_str(resp, "stderr", "error")
        out = stdout or stderr
        header = f"$ {cmd[:150]}"
        if out:
            # Keep only meaningful lines (skip blank/duplicate)
            lines = [l for l in out.splitlines() if l.strip()][:20]
            return header + "\n" + "\n".join(lines) if lines else header
        return header

    if tool in ("WebFetch",):
        url = str(inp.get("url") or "").strip()
        return f"fetched {url}"

    if tool == "WebSearch":
        query = str(inp.get("query") or "").strip()
        return f"searched: {query}"

    if tool == "Agent":
        desc = str(inp.get("description") or inp.get("prompt") or "").strip()
        return f"subtask: {desc[:100]}" if desc else "subtask started"

    # Fallback: extract raw text from response
    parts = list(iter_text(resp))
    if not parts:
        parts.extend(iter_text(event.get("content")))
    return unique_lines(parts)


def candidate_transcript_paths(event: dict[str, Any]) -> list[Path]:
    home = Path.home()
    base = home / ".claude" / "projects"
    session_id = str(event.get("sessionId") or event.get("session_id") or "").strip()
    cwd = str(event.get("cwd") or "").strip()
    transcript_path = str(event.get("transcript_path") or "").strip()

    paths: list[Path] = []
    if transcript_path:
        paths.append(Path(transcript_path))
    if session_id and cwd:
        encoded = cwd.replace("/", "-")
        paths.append(base / encoded / f"{session_id}.jsonl")
    if session_id:
        paths.extend(base.glob(f"*/{session_id}.jsonl"))
    return paths


def _make_reply_pipe() -> Path | None:
    INTERACT_DIR.mkdir(parents=True, exist_ok=True)
    path = INTERACT_DIR / f"{SID}-{os.getpid()}-{time.time_ns()}.pipe"
    try:
        os.mkfifo(path, 0o600)
        return path
    except FileExistsError:
        return path
    except OSError:
        return None


def _poll_response(reply_pipe: Path) -> str | None:
    """Wait for one line from a reply FIFO and return it."""
    import select as _sel

    try:
        fd = os.open(str(reply_pipe), os.O_RDWR | os.O_NONBLOCK)
    except OSError:
        return None

    buf = b""
    try:
        while True:
            r, _, _ = _sel.select([fd], [], [], 0.5)
            if not r:
                continue
            try:
                chunk = os.read(fd, 4096)
            except OSError:
                continue
            if not chunk:
                continue
            buf += chunk
            if b"\n" in buf:
                line, _, _ = buf.partition(b"\n")
                return line.decode("utf-8", errors="ignore").strip()
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
        reply_pipe.unlink(missing_ok=True)


def _wait_for_valid_reply(reply_pipe: Path | None, validate: callable, invalid_hint: str) -> tuple[str, str | None]:
    """Block until validate(choice) succeeds or the interaction is replaced."""
    if reply_pipe is None:
        return "error", None
    while True:
        choice = _poll_response(reply_pipe)
        if choice is None:
            return "error", None
        if choice == REPLACED_INTERACTION_TOKEN:
            return "replaced", None
        if validate(choice):
            return "ok", choice
        write_to_pipe(f"⚙ invalid input\n{invalid_hint}")


def handle_permission_request(event: dict[str, Any]) -> None:
    """Send numbered options to WeChat, block until user replies 1/2, return decision JSON."""
    tool_name = str(event.get("tool_name") or event.get("toolName") or "tool").strip()
    tool_input = event.get("tool_input") or event.get("toolInput") or {}

    parts = [f"⚙ permission request", f"tool: {tool_name}"]
    for key in ("command", "file_path", "path", "url", "pattern"):
        val = tool_input.get(key)
        if val and isinstance(val, str):
            parts.append(f"{key}: {val[:200]}")
            break
    parts += ["", "1. allow", "2. deny", "", "reply with a number"]
    write_to_pipe("\n".join(parts))

    reply_pipe = _make_reply_pipe()
    if reply_pipe is not None:
        _write_control_command({
            "action": "interaction_start",
            "id": SID,
            "kind": "permission",
            "reply_pipe": str(reply_pipe),
        })
    status, choice = _wait_for_valid_reply(
        reply_pipe,
        lambda v: v in {"1", "2"},
        "reply 1 to allow or 2 to deny",
    )
    _write_control_command({"action": "interaction_end", "id": SID, "reply_pipe": str(reply_pipe or "")})
    if status == "replaced":
        return
    if status != "ok":
        write_to_pipe(f"⚙ permission request ended: {tool_name}")
        return

    behavior = "allow" if choice == "1" else "deny"
    label = "allowed" if behavior == "allow" else "denied"
    write_to_pipe(f"⚙ {label}: {tool_name}")
    print(json.dumps({
        "hookSpecificOutput": {
            "hookEventName": "PermissionRequest",
            "decision": {"behavior": behavior},
        }
    }))


def extract_permission_denied_text(event: dict[str, Any]) -> str:
    tool_name = str(event.get("tool_name") or event.get("toolName") or "tool").strip()
    reason = str(event.get("reason") or event.get("message") or "").strip()
    parts = [f"⚙ permission denied: {tool_name}"]
    if reason:
        parts.append(reason)
    return "\n".join(parts)


def extract_notification_text(event: dict[str, Any]) -> str:
    subtype = str(event.get("subtype") or event.get("notification_type") or "notification").strip()
    title = str(event.get("title") or "").strip()
    message = str(event.get("message") or event.get("text") or "").strip()
    parts = [f"⚙ notification: {subtype}"]
    if title:
        parts.append(title)
    if message:
        parts.append(message)
    if subtype == "elicitation_dialog":
        parts.append("Claude is waiting for your confirmation on Mac.")
    return "\n".join(parts)


def _build_enum_options(schema: dict[str, Any]) -> list[tuple[str, dict[str, Any]]]:
    """Return [(display_label, result_dict), ...] from a JSON schema."""
    options: list[tuple[str, dict[str, Any]]] = []
    props = schema.get("properties") or {}
    for field, spec in props.items():
        if not isinstance(spec, dict):
            continue
        enum = spec.get("enum")
        field_type = spec.get("type")
        if enum:
            for val in enum:
                desc = ""
                if isinstance(spec.get("enumDescriptions"), dict):
                    desc = spec["enumDescriptions"].get(str(val), "")
                label = f"{val}" + (f" — {desc}" if desc else "")
                options.append((label, {field: val}))
            break
        if field_type == "boolean":
            options.append(("yes (true)", {field: True}))
            options.append(("no (false)", {field: False}))
            break
    return options


def handle_elicitation(event: dict[str, Any]) -> None:
    """Send numbered choices to WeChat, block until user replies, return result JSON."""
    schema = event.get("schema") or event.get("input_schema") or {}
    prompt = str(event.get("prompt") or event.get("message") or "").strip()
    options = _build_enum_options(schema) if isinstance(schema, dict) else []

    parts = ["⚙ input required"]
    if prompt:
        parts.append(prompt)

    if options:
        parts.append("")
        for i, (label, _) in enumerate(options, 1):
            parts.append(f"{i}. {label}")
        parts.append("\nreply with a number")
    else:
        # Free-text field — describe what's expected
        props = (schema.get("properties") or {}) if isinstance(schema, dict) else {}
        for field, spec in props.items():
            if isinstance(spec, dict):
                desc = spec.get("description") or spec.get("title") or field
                parts.append(f"\nreply with: {desc}")
            break

    write_to_pipe("\n".join(parts))

    reply_pipe = _make_reply_pipe()
    if reply_pipe is not None:
        _write_control_command({
            "action": "interaction_start",
            "id": SID,
            "kind": "elicitation",
            "reply_pipe": str(reply_pipe),
        })
    if options:
        normalized_labels = [label.lower() for label, _ in options]

        def _valid_choice(v: str) -> bool:
            v = v.strip()
            if not v:
                return False
            try:
                idx = int(v) - 1
                return 0 <= idx < len(options)
            except ValueError:
                lower = v.lower()
                return any(lower in label for label in normalized_labels)

        status, choice = _wait_for_valid_reply(
            reply_pipe,
            _valid_choice,
            "enter a valid option number or a keyword from the options",
        )
    else:
        status, choice = _wait_for_valid_reply(
            reply_pipe,
            lambda v: bool(v.strip()),
            "enter non-empty content",
        )
    _write_control_command({"action": "interaction_end", "id": SID, "reply_pipe": str(reply_pipe or "")})
    if status == "replaced":
        return
    if status != "ok":
        write_to_pipe("⚙ input request ended")
        return

    result: dict[str, Any] = {}
    if options and choice is not None:
        try:
            idx = int(choice) - 1
            if 0 <= idx < len(options):
                result = options[idx][1]
        except ValueError:
            # Match by label substring
            for label, val in options:
                if choice.lower() in label.lower():
                    result = val
                    break
    elif choice is not None:
        # Free-text: put in first field
        props = (schema.get("properties") or {}) if isinstance(schema, dict) else {}
        if props:
            result = {next(iter(props)): choice}

    write_to_pipe(f"⚙ submitted: {choice or '(not submitted)'}")
    print(json.dumps({"result": result}))


def extract_elicitation_result_text(event: dict[str, Any]) -> str:
    result = event.get("result")
    if result:
        try:
            rendered = json.dumps(result, ensure_ascii=False)
        except Exception:
            rendered = str(result)
        return f"⚙ option result: {rendered}"
    return ""


def latest_assistant_entry(path: Path) -> dict[str, str]:
    latest: dict[str, str] = {}
    with path.open() as f:
        for line in f:
            try:
                item = json.loads(line)
            except Exception:
                continue
            message = item.get("message", {})
            if item.get("type") != "assistant" and message.get("role") != "assistant":
                continue
            text = unique_lines(iter_text(message.get("content")))
            if not text:
                continue
            latest = {
                "uuid": str(item.get("uuid") or ""),
                "text": text,
                "timestamp": str(item.get("timestamp") or ""),
            }
    return latest


def compute_delta(prev: dict[str, Any], current: dict[str, str]) -> tuple[str, dict[str, Any]]:
    if not current:
        return "", prev

    prev_uuid = str(prev.get("assistant_uuid") or "")
    prev_text = str(prev.get("assistant_text") or "")
    cur_uuid = current.get("uuid", "")
    cur_text = current.get("text", "")

    if cur_uuid and cur_uuid == prev_uuid and cur_text.startswith(prev_text):
        delta = cur_text[len(prev_text):].strip()
    elif cur_text == prev_text:
        delta = ""
    else:
        delta = cur_text

    new_state = {
        "assistant_uuid": cur_uuid,
        "assistant_text": cur_text,
        "assistant_timestamp": current.get("timestamp", ""),
    }
    return delta, new_state


def load_transcript_delta(event: dict[str, Any], wait_for_update: bool = False) -> str:
    prev = load_state(event)
    attempts = 10 if wait_for_update else 1

    for i in range(attempts):
        for path in candidate_transcript_paths(event):
            if not path.exists():
                continue
            try:
                current = latest_assistant_entry(path)
            except Exception:
                continue
            delta, new_state = compute_delta(prev, current)
            if delta:
                save_state(event, new_state)
                return delta
        if i + 1 < attempts:
            time.sleep(0.2)
    return ""


def extract_stop_failure_text(event: dict[str, Any]) -> str:
    error = str(event.get("error") or event.get("message") or "unknown error").strip()
    parts = ["⚙ Claude execution failed"]
    if error:
        parts.append(error)
    return "\n".join(parts)


def extract_tool_failure_text(event: dict[str, Any]) -> str:
    tool_name = str(event.get("tool_name") or event.get("toolName") or "tool").strip()
    inp = event.get("tool_input") or {}
    error = str(event.get("error") or event.get("message") or "").strip()
    hint = ""
    for key in ("file_path", "path", "command", "url", "query", "notebook_path"):
        val = inp.get(key)
        if val and isinstance(val, str):
            hint = f"`{val[:100]}`"
            break
    subject = f"{tool_name} {hint}".strip()
    parts = [f"⚙ tool failed: {subject}"]
    if error:
        parts.append(error[:300])
    return "\n".join(parts)


def extract_pre_tool_text(event: dict[str, Any]) -> str:
    tool_name = str(event.get("tool_name") or "tool").strip()
    tool_input = event.get("tool_input") or {}
    parts = [f"⚙ {tool_name}"]
    if isinstance(tool_input, dict):
        for key in ("command", "path", "file_path", "url", "pattern", "query", "description"):
            val = tool_input.get(key)
            if val and isinstance(val, str):
                parts.append(f"{key}: {val[:200]}")
                break
    return "\n".join(parts)


def extract_session_event_text(event: dict[str, Any], mode: str) -> str:
    if mode == "start":
        startup_type = str(event.get("startup_type") or event.get("type") or "startup").strip()
        labels = {"startup": "startup", "resume": "resume", "clear": "reset", "compact": "compaction restore"}
        label = labels.get(startup_type, startup_type)
        return f"⚙ session {label}"
    return "⚙ session ended"


def extract_compact_text(event: dict[str, Any], mode: str) -> str:
    if mode == "pre":
        return "⚙ compacting context..."
    return "⚙ context compaction complete"


def extract_subagent_text(event: dict[str, Any], mode: str) -> str:
    agent_type = str(event.get("agent_type") or "subagent").strip()
    if mode == "start":
        return f"⚙ subagent started: {agent_type}"
    return f"⚙ subagent done: {agent_type}"


def extract_user_prompt_text(event: dict[str, Any]) -> str:
    # UserPromptSubmit event — forward the user's message to WeChat so
    # input typed on the Mac is visible on the phone.
    prompt = str(
        event.get("prompt") or
        event.get("message") or
        event.get("text") or ""
    ).strip()
    if not prompt:
        msg = event.get("message") or {}
        if isinstance(msg, dict):
            prompt = str(msg.get("content") or msg.get("text") or "").strip()
    if not prompt:
        return ""
    return f"👤 {prompt}"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--input-daemon", nargs=3,
        metavar=("INPUT_PIPE", "MANIFEST_PATH", "OUTPUT_PIPE"),
        help="Run as background input-injection daemon (internal use)",
    )
    parser.add_argument("--final", action="store_true",
                        help="Called on Stop event (final assistant reply)")
    parser.add_argument("--permission-request", action="store_true")
    parser.add_argument("--permission-denied", action="store_true")
    parser.add_argument("--notification", action="store_true")
    parser.add_argument("--elicitation", action="store_true")
    parser.add_argument("--elicitation-result", action="store_true")
    parser.add_argument("--stop-failure", action="store_true")
    parser.add_argument("--post-tool-failure", action="store_true")
    parser.add_argument("--pre-tool-use", action="store_true")
    parser.add_argument("--session-start", action="store_true")
    parser.add_argument("--session-end", action="store_true")
    parser.add_argument("--pre-compact", action="store_true")
    parser.add_argument("--post-compact", action="store_true")
    parser.add_argument("--subagent-start", action="store_true")
    parser.add_argument("--subagent-stop", action="store_true")
    parser.add_argument("--user-prompt", action="store_true")
    args = parser.parse_args()

    if args.input_daemon:
        run_input_daemon(*args.input_daemon)
        return

    try:
        event = json.load(sys.stdin)
    except Exception:
        sys.exit(0)

    if args.permission_request:
        handle_permission_request(event)
        return
    elif args.permission_denied:
        output = truncate(extract_permission_denied_text(event), MAX_LEN)
    elif args.notification:
        output = truncate(extract_notification_text(event), MAX_LEN)
    elif args.elicitation:
        handle_elicitation(event)
        return
    elif args.elicitation_result:
        output = truncate(extract_elicitation_result_text(event), MAX_LEN)
        if not output:
            sys.exit(0)
    elif args.stop_failure:
        output = truncate(extract_stop_failure_text(event), MAX_LEN)
    elif args.post_tool_failure:
        output = truncate(extract_tool_failure_text(event), MAX_LEN)
    elif args.pre_tool_use:
        output = truncate(extract_pre_tool_text(event), MAX_LEN)
    elif args.session_start:
        if not os.environ.get("CLAUDE_HOOK_PIPE"):
            _ambient_session_setup()
        output = truncate(extract_session_event_text(event, "start"), MAX_LEN)
    elif args.session_end:
        output = truncate(extract_session_event_text(event, "end"), MAX_LEN)
        write_to_pipe(output)
        if not os.environ.get("CLAUDE_HOOK_PIPE"):
            _ambient_session_teardown()
        return  # already wrote above
    elif args.pre_compact:
        output = truncate(extract_compact_text(event, "pre"), MAX_LEN)
    elif args.post_compact:
        output = truncate(extract_compact_text(event, "post"), MAX_LEN)
    elif args.subagent_start:
        output = truncate(extract_subagent_text(event, "start"), MAX_LEN)
    elif args.subagent_stop:
        output = truncate(extract_subagent_text(event, "stop"), MAX_LEN)
    elif args.user_prompt:
        output = truncate(extract_user_prompt_text(event), MAX_LEN)
        if not output:
            sys.exit(0)
    elif args.final:
        text = extract_final_text(event)
        if not text:
            sys.exit(0)
        output = "🤖 " + truncate(text, MAX_LEN)
    else:
        tool_summary = format_tool_output(event)
        transcript_delta = load_transcript_delta(event, wait_for_update=False)
        pieces = []
        if transcript_delta:
            pieces.append("🤖 " + truncate(transcript_delta, MAX_LEN))
        if tool_summary:
            pieces.append("⚙ " + truncate(tool_summary, MAX_LEN))
        if not pieces:
            sys.exit(0)
        output = "\n\n".join(pieces)

    write_to_pipe(output)


if __name__ == "__main__":
    main()
