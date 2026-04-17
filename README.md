# claude-bridge

Remotely control multiple Claude Code sessions on a Mac via personal WeChat.

## Architecture

```text
WeChat (mobile)
  ↕  iLink Bot API
bin/claude-bridge
  ├── platform adapter (currently iLink)
  ├── session manager
  │     └── Terminal.app -> claude
  │           ↕ named pipe
  └── hooks/push_output.py
```

The default platform is WeChat/iLink, designed for a single WeChat account with a single user. The main process starts in the background by default and writes logs to `~/.claude-bridge/bridge.log`.

## Quick Start

```bash
make all

./bin/claude-bridge install-hooks
./bin/claude-bridge login
./bin/claude-bridge
```

After installing the hook, merge the contents of [hooks/settings.json](hooks/settings.json) into `~/.claude/settings.json`.

## Homebrew

A Homebrew formula template is included: [Formula/claude-bridge.rb](Formula/claude-bridge.rb).
Full release steps: [RELEASING.md](RELEASING.md).

Recommended steps:

1. Publish a GitHub Release
2. Compute the `sha256` of the release tarball
3. Update `url` and `sha256` in the formula
4. Host it in your tap repo, e.g. `homebrew-claude-bridge`

Users can then install with:

```bash
brew install claude-bridge
claude-bridge install-hooks
claude-bridge login
claude-bridge
```

To use with `brew services`:

```bash
brew services start claude-bridge
```

The formula uses the foreground command `claude-bridge serve` for this mode.

## Local Commands

```bash
./bin/claude-bridge           # Start bridge in background (default)
./bin/claude-bridge start     # Same as above
./bin/claude-bridge status    # Show bridge status
./bin/claude-bridge stop      # Stop bridge
./bin/claude-bridge list      # List discoverable sessions
./bin/claude-bridge logs      # View logs
./bin/claude-bridge logs -f   # Follow logs
./bin/claude-bridge login     # Scan QR to log in
./bin/claude-bridge install-hooks  # Install Claude hooks
```

## WeChat Commands

| Message | Action |
|---|---|
| `#l` | List active sessions |
| `#n ~/my-project` | Open a new Claude session on Mac |
| `#<sid> hello` | Send message to a named session |
| `#<sid>` | Set that session as the default |
| `#r` | Clear the default session |
| plain text | Send to the current default session |

Notes:

- After `#n`, the new session is automatically set as the default.
- If exactly one session is waiting for a permission/option reply, plain text is automatically routed to it.
- `local-xxxx` entries in `#l` indicate locally-started Claude sessions whose output is bridged.
- Multiple WeChat users are not distinguished; the bridge treats the current WeChat account as the sole message target.

## Output and Interaction

- Claude's tool output and final replies are pushed back to WeChat via hooks.
- Permission requests, denials, notifications, and option prompts are also forwarded.
- Output is sent in event-based incremental chunks, not token-level streaming.
- Sessions created via `#n` on mobile open a visible Terminal window on Mac.

## State Files

```text
~/.claude-bridge/
  credentials.json   # login credentials
  context-tokens.json
  cursor.txt
  ambient-user.txt   # current sole message target
  bridge.log
  bridge.pid
```

## Proxy

`claude-bridge` and the Claude sessions it spawns inherit the following environment variables from the shell:

- `HTTP_PROXY`
- `HTTPS_PROXY`
- `ALL_PROXY`
- `NO_PROXY`

Example:

```bash
./bin/claude-bridge
```

To proxy the login step as well:

```bash
./bin/claude-bridge login
```

## Verification

```bash
./bin/claude-bridge
./bin/claude-bridge logs -f
```

Then send from WeChat:

```text
#n .
```

Then send:

```text
summarize this project for me
```

Because `#n` sets the new session as the default, the plain text message goes directly into it. In the logs you should see:

```text
INFO rx ...
INFO session output ...
INFO sending to user ...
```

## Dependencies

- Go 1.22+
- Python 3.8+
- Claude Code CLI (`claude` command available)

## TODO
- [X] Sessions started before the bridge cannot be discovered
- [ ] Cannot send messages to user after startup
  - [X] Works after receiving one message
  - [ ] Cannot be used on first run; awaiting official support
- [X] Change command prefix
- [X] Simplify commands
- [X] Simplify session selection; names were too long
- [X] Improve output
  - [X] Distinguish user and assistant output
- [ ] Support more platforms
  - [X] WeChat
