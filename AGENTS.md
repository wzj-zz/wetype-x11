# AGENTS.md

## 项目概述

微信输入法(WeType)语音转文字的极简 X11 工具：Ctrl+Alt+Z 开始/结束录音，识别结果上屏到当前焦点窗口。Go 单文件实现，静态编译零动态依赖，部署到 `~/.LinuxTools/bin/wetype-x11`。

## 架构关键点

- `main.go`：全部逻辑（协议客户端 + 录音 + 上屏），唯一源码。
- 协议：移植自 xifan2333/vinput-registry 的 `wetype/streaming/entry.py`（ECDH 自定义 128-bit 曲线 / AES-256-ECB / WebSocket / protobuf-wire / snappy），服务器 `wetype.weixin.qq.com`。
- 录音：`pactl` 锁定默认非 monitor 源；录音期间默认临时静音 sink（`WETYPE_X11_MUTE=0` 关闭）防止外放串音。
- 上屏：剪贴板粘贴（保存旧 → 写入 → 模拟粘贴键 → 300ms 后恢复）。普通窗口 Ctrl+V，终端（`xprop` 读 WM_CLASS 精确匹配）Ctrl+Shift+V；无 xclip/xsel 时回退 `xdotool type`。
- 无桌面通知；状态与错误只写 `~/.cache/wetype-x11/wetype-x11.log`。

## 构建与部署

```sh
./build.sh                                 # 首次会自动下载+静态编译 libopus（较慢）
cp wetype-x11 ~/.LinuxTools/bin/wetype-x11 # 部署
```

验证静态性：`LC_ALL=C ldd wetype-x11` 应输出 `not a dynamic executable`。

## 修改约定

- 改终端判定时同步维护 main.go 内的终端 WM_CLASS 列表。
- 部署侧的热键脚本在 `~/.LinuxTools/bin/core/auto.ahk`（改它需同步同目录 `auto.md`）。
- 发版：`git tag vX.Y.Z && git push origin vX.Y.Z`，GitHub Actions 自动构建并发布 Release；push main 自动刷新 latest 预发布。Release 仅作备份，日常部署走上面的本地 cp。
