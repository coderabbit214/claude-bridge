# claude-bridge

用个人微信远程控制一台 Mac 上的多个 Claude Code 会话。

## 架构

```text
手机微信
  ↕  iLink Bot API
bin/claude-bridge
  ├── 平台适配层（当前为 iLink）
  ├── 会话管理器
  │     └── Terminal.app -> claude
  │           ↕ named pipe
  └── hooks/push_output.py
```

当前默认平台是微信/iLink，设计前提是单个微信账号对应单个使用者。主程序默认在后台启动，日志写到 `~/.claude-bridge/bridge.log`。

## 快速开始

```bash
make all

./bin/claude-bridge install-hooks
./bin/claude-bridge login
./bin/claude-bridge
```

安装 hook 后，还需要把 [hooks/settings.json](hooks/settings.json) 的内容合并到 `~/.claude/settings.json`。

## Homebrew

仓库里已经带了一个 Homebrew formula 模板：[Formula/claude-bridge.rb](Formula/claude-bridge.rb)。
完整发布步骤见：[RELEASING.md](RELEASING.md)。

推荐做法：

1. 发布一个 GitHub Release
2. 计算 release tarball 的 `sha256`
3. 把 formula 里的 `url` 和 `sha256` 改成真实值
4. 放到你的 tap 仓库，例如 `homebrew-claude-bridge`

用户安装后可以这样用：

```bash
brew install claude-bridge
claude-bridge install-hooks
claude-bridge login
claude-bridge
```

如果你想用 `brew services`：

```bash
brew services start claude-bridge
```

这时 formula 会使用前台常驻命令 `claude-bridge serve`。

## 本地命令

```bash
./bin/claude-bridge           # 后台启动 bridge（默认命令）
./bin/claude-bridge start     # 同上
./bin/claude-bridge status    # 查看 bridge 状态
./bin/claude-bridge stop      # 停止 bridge
./bin/claude-bridge list      # 查看当前可发现的会话
./bin/claude-bridge logs      # 查看日志
./bin/claude-bridge logs -f   # 持续跟随日志
./bin/claude-bridge login     # 扫码登录
./bin/claude-bridge install-hooks  # 安装 Claude hooks
```

## 微信命令

| 发送内容 | 效果 |
|---|---|
| `#l` | 列出活跃会话 |
| `#n ~/my-project` | 在 Mac 上打开一个新的 Claude 会话 |
| `#<sid> hello` | 向指定会话发送消息 |
| `#<sid>` | 将该会话设为默认会话 |
| `#r` | 清除默认会话 |
| `普通文本` | 发给当前默认会话 |

说明：

- `#n` 创建成功后，会自动把新会话设为默认会话。
- 如果当前只有一个会话正在等待权限/选项回复，普通文本也会自动提交给它。
- `#l` 里看到的 `local-xxxx` 表示本地 Claude 会话的输出已接入 bridge。
- 不区分多个微信用户；bridge 会把当前这个微信账号视为唯一消息目标。

## 输出与交互

- Claude 的工具输出和最终回复会通过 hook 推回微信。
- 权限请求、拒绝、通知、选项类事件也会推到微信。
- 推送是按事件分段增量发送，不是 token 级流式。
- 手机上 `#n` 创建的会话会在 Mac 上打开一个可见的 Terminal 窗口。

## 文件与状态

```text
~/.claude-bridge/
  credentials.json   # 登录凭证
  context-tokens.json
  cursor.txt
  ambient-user.txt   # 当前唯一消息目标
  bridge.log
  bridge.pid
```

## 代理

`claude-bridge` 和它启动出来的 Claude 会话都会继承当前 shell 的这些环境变量：

- `HTTP_PROXY`
- `HTTPS_PROXY`
- `ALL_PROXY`
- `NO_PROXY`

例如：

```bash
export HTTPS_PROXY=http://127.0.0.1:7890
export HTTP_PROXY=http://127.0.0.1:7890
./bin/claude-bridge
```

如果登录也要走代理：

```bash
./bin/claude-bridge login
```

## 验证

```bash
./bin/claude-bridge
./bin/claude-bridge logs -f
```

然后在微信发送：

```text
#n .
```

再发送：

```text
直接帮我总结这个项目
```

因为 `#n` 后该会话会被设为默认会话，所以这条普通文本会直接进入刚创建的会话。正常情况下你会在日志里看到：

```text
INFO rx ...
INFO session output ...
INFO sending to user ...
```

## 依赖

- Go 1.22+
- Python 3.8+
- Claude Code CLI（`claude` 命令可用）

## TODO
- [X] 在命令启动前启动的会话无法被发现
- [ ] 启动后无法给用户发送消息
  - [X] 发送一次消息后可以了
  - [ ] 第一次使用无法使用，等待官方支持
- [X] 修改命令前缀
- [X] 简化命令
- [X] 简化选择会话的流程，目前名称太长了
- [X] 优化输出
  - [X] 区分 user 和 assistant 输出
- [ ] 支持更多平台
  - [X] 微信
  
