# claude-bridge

用个人微信远程控制一台 Mac 上的多个 Claude Code 会话。

## 安装

```bash
brew tap coderabbit214/claude-bridge https://github.com/coderabbit214/claude-bridge
brew install claude-bridge
```

## 初始化

```bash
claude-bridge install-hooks
claude-bridge login
claude-bridge
```

安装 hook 后，还需要把 `~/.claude-bridge/hooks/settings.json` 的内容合并到 `~/.claude/settings.json`。

如需开机自动启动：

```bash
brew services start claude-bridge
```

## 更新

```bash
claude-bridge stop
brew update
brew upgrade claude-bridge
claude-bridge start
```

## 本地命令

```bash
claude-bridge             # 后台启动 bridge（默认命令）
claude-bridge start       # 同上
claude-bridge status      # 查看 bridge 状态
claude-bridge stop        # 停止 bridge
claude-bridge list        # 查看当前可发现的会话
claude-bridge logs        # 查看日志
claude-bridge logs -f     # 持续跟随日志
claude-bridge login       # 扫码登录
claude-bridge install-hooks  # 安装 Claude hooks
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

## 代理

`claude-bridge` 和它启动出来的 Claude 会话都会继承当前 shell 的这些环境变量：

- `HTTP_PROXY`
- `HTTPS_PROXY`
- `ALL_PROXY`
- `NO_PROXY`
