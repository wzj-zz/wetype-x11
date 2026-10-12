# wetype-x11

微信输入法(WeType)流式语音转文字小工具。按一次快捷键开始录音,再按一次结束,
识别结果直接输入到当前焦点窗口。

不依赖 fcitx5 / 输入法框架,不需要微信账号,不需要 API key。

## 形态

Go 单文件源码, 静态编译为**零动态依赖**的单文件二进制(`./build.sh` 一键构建)。

## 运行依赖

- 录音命令任一:`pw-record`(PipeWire)/ `parec`(PulseAudio)/ `arecord`(ALSA)
- `xdotool`(上屏,X11/XWayland)
- `xclip` 或 `xsel`(上屏,剪贴板方案;两者操作同一 X11 CLIPBOARD 选区,自动探测)
- `pactl`(可选,麦克风源锁定与录音期间静音)

```bash
./wetype-x11 --check       # 依赖检查
./wetype-x11 --handshake   # 测试与 wetype.weixin.qq.com 连通性
```

## 构建

需要 `go`(>=1.22)、`gcc`、`curl`。`build.sh` 自动下载并静态编译 libopus:

```bash
./build.sh        # 产出 wetype-x11, ldd 显示 "not a dynamic executable"
```

## 热键绑定

本仓库通过 ahk_x11 绑定(二进制可部署到任意 PATH 目录):

```ahk
^!z::  ; 开始/结束录音并上屏 (Ctrl+Shift+Z 亦可)
Run, /path/to/wetype-x11
return
```

其他热键工具(sxhkd、KDE 自定义快捷键等)同理,直接执行二进制即可。

## 说明

## 自动更新(可选)

开机时比对 GitHub 最新 release 的 sha256, 不同则原子替换本地二进制:

```sh
#!/bin/sh
bin=/path/to/wetype-x11
base=https://github.com/<owner>/wetype-x11/releases/latest/download
remote=$(curl -sfL --max-time 15 "$base/wetype-x11.sha256" | cut -d' ' -f1) || exit 0
[ -n "$remote" ] || exit 0
[ -f "$bin" ] && [ "$(sha256sum "$bin" | cut -d' ' -f1)" = "$remote" ] && exit 0
curl -sfL --max-time 120 "$base/wetype-x11" -o "$bin.new" || { rm -f "$bin.new"; exit 1; }
echo "$remote  $bin.new" | sha256sum -c - >/dev/null 2>&1 || { rm -f "$bin.new"; exit 1; }
chmod +x "$bin.new" && mv "$bin.new" "$bin" && notify-send "wetype-x11 已更新" 2>/dev/null || true
```

网络失败时静默退出, 不影响开机。可挂在任意自启动位置(ahk_x11 自启动段 / systemd user service / cron @reboot)。

## 说明

- 录音源锁定为默认麦克风, 不会录到 monitor(系统声音)
- `WETYPE_X11_MUTE=0`: 关闭录音期间的临时静音(默认开启, 防止外放声音被麦克风物理收录; 录音结束立即恢复, 且只恢复由本程序静音的状态)
- 上屏: 全部走剪贴板粘贴(保存→写入→模拟粘贴键→恢复旧剪贴板, 不丢字): 普通窗口 Ctrl+V, 终端窗口(按 WM_CLASS 精确识别) Ctrl+Shift+V; 无 xclip 时回退 xdotool 模拟按键。`WETYPE_X11_TYPE=type` 可强制只用模拟按键; `--wclass` 可查看当前窗口的判定结果
- 设备身份缓存于 `~/.cache/wetype-x11/identity.json`(0600),首次运行自动注册
- 日志: `~/.cache/wetype-x11/wetype-x11.log`
- 录音最长 `WETYPE_X11_MAX_SECS` 秒(默认 300),超时自动结束上屏
- 协议实现源自 [vinput-registry](https://github.com/xifan2333/vinput-registry)
  的 `wetype/streaming/entry.py`(非官方协议,参考
  [VoiceKey](https://github.com/J3n5en/voicekey),客户端参数 `2.2.3(657)`)
- 录音会发送至微信输入法云端识别服务;服务端协议可能变化
