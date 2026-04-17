# claude-bridge

Remotely control multiple Claude Code sessions on a Mac via personal WeChat.

## Installation

```bash
brew tap coderabbit214/claude-bridge https://github.com/coderabbit214/claude-bridge
brew install claude-bridge
```

## Setup

```bash
claude-bridge install-hooks
claude-bridge login
claude-bridge
```

After installing hooks, merge the contents of `~/.claude-bridge/hooks/settings.json` into `~/.claude/settings.json`.

To start automatically with your Mac:

```bash
brew services start claude-bridge
```

## Commands

```bash
claude-bridge             # Start bridge in background (default)
claude-bridge start       # Same as above
claude-bridge status      # Show bridge status
claude-bridge stop        # Stop bridge
claude-bridge list        # List discoverable sessions
claude-bridge logs        # View logs
claude-bridge logs -f     # Follow logs
claude-bridge login       # Scan QR to log in
claude-bridge install-hooks  # Install Claude hooks
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

## Proxy

`claude-bridge` and the Claude sessions it spawns inherit the following environment variables from the shell:

- `HTTP_PROXY`
- `HTTPS_PROXY`
- `ALL_PROXY`
- `NO_PROXY`

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
