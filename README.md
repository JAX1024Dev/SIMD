# SIMD · SIM 短信管理服务

Go 实现的 Linux 常驻服务，通过 UART0 管理多个 ML307R，提供收发短信、SQLite 保存、HTTP API 和 Bark 自动转发。无需外部数据库或消息队列。

## 下载与 Orange Pi Zero 3

首个版本为 **v0.1.0**，下载见 [GitHub Releases](https://github.com/JAX1024Dev/SIMD/releases/tag/v0.1.0)。Orange Pi Zero 3 使用 `simd_v0.1.0_linux_arm64.tar.gz`，适配 64 位 Debian / Ubuntu / Armbian。压缩包内含二进制、配置、systemd 文件与文档，目标机器无需安装 Go。

按照 [Orange Pi Zero 3 部署指南](docs/orangepi-zero3.md)确认 `aarch64`、校验 SHA-256、启用实际 UART 并安装服务。另提供 Linux amd64 包。`simd -version` 可直接查看版本信息。

## 从源码快速开始

需要 Go 1.24 或更新版本。运行目标为 Linux amd64/arm64，macOS 支持开发测试和串口探测。

```sh
make build
export SIMD_API_TOKEN="$(openssl rand -hex 24)"
export SIMD_BARK_IPHONE_KEY='你的 Bark device key'
cp configs/simd.example.yaml configs/simd.yaml
# 编辑串口路径、模组名称与 Bark 目标
./bin/simd -config configs/simd.yaml -check-config
./bin/simd -config configs/simd.yaml
```

配置仅从 YAML 和所引用的环境变量读取，修改后重启生效。相对数据库路径以进程工作目录为基准。数据库必须放在独立的私有目录（0700）中；默认自动创建 `./data`。Token 至少 24 字节。

**首次正常启动会归档当前 SIM 的已有短信，并在数据库提交后清理模组对应存储位置，不推送这些历史短信。** 后续新短信入库并转发。启动前需要保留模组上的短信时，先使用下面的只读探测，不要启动完整 daemon。

## 硬件接入与只读探测

连接 UART0，模组原生电平为 **1.8V**，接入 3.3V TTL/RS232 必须做电平转换。默认 115200、8N1；RTS/CTS 仅在实际接线时启用。主机自行联网，daemon 不负责拨号、PIN 解锁或模组休眠。

```sh
./bin/simd -probe-port /dev/ttyS0
./bin/simd -probe-port /dev/ttyS0 -probe-sms
```

探测只执行 AT、型号、固件及可选的 SIM/短信能力查询；不初始化短信配置，不读取、删除或发送短信，输出隐藏 ICCID。串口需有读写权限，且不能被 ModemManager、串口控制台或其他进程占用。运行时使用文件锁及 TIOCEXCL 独占串口。

当前实机识别及待完成验收见 [硬件与测试报告](docs/validation.md)。ML307R-DL 不支持本方案要求的短信指令，驱动拒绝启用。

## HTTP API

默认监听 `127.0.0.1:8080`。`/health/live` 与 `/health/ready` 不需要鉴权，只返回最小状态；业务接口必须携带 Bearer Token。

```sh
curl http://127.0.0.1:8080/health/ready
curl -H "Authorization: Bearer $SIMD_API_TOKEN" \
  http://127.0.0.1:8080/api/v1/devices
curl -H "Authorization: Bearer $SIMD_API_TOKEN" \
  'http://127.0.0.1:8080/api/v1/messages?direction=inbound&limit=50'
```

发送短信会产生实际运营商费用。每次发送必须提供新的幂等键；查询、重试同一请求时复用原键。

```sh
curl -X POST http://127.0.0.1:8080/api/v1/messages \
  -H "Authorization: Bearer $SIMD_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: send-20261024-001' \
  -d '{"device_id":"sim-01","number":"10086","body":"测试"}'
```

返回 `202 {message_id,status,created}`，通过 `GET /api/v1/messages/{id}` 查询状态。同键不同参数返回 409。幂等键保留期限与其短信记录一致。

`submitted` 表示模组确认提交；`unknown` 表示已经尝试提交但结果不确定。`failed`、`unknown` 不会自动重发。超长短信和 UCS2 无法表示的字符（例如 emoji）返回 422。

详细接口及字段见 [OpenAPI](docs/openapi.yaml)；需求与业务语义见 [需求文档](docs/requirements.md)。

## Bark 通知样式

首个版本采用已确认的简洁格式：

```text
[Apple Messages icon]
New Message
10086 · 2026-10-25 00:15:04

【模拟短信】您的验证码为 123456，5 分钟内有效。
```

- 所有短信使用同一 Apple Messages 绿色白气泡图标，适配通知中的圆形展示。
- 标题为 `New Message`；副标题只显示来信号码和一个 `YYYY-MM-DD HH:mm:ss` 时间戳；正文保持短信原文。
- 时间显示固定为 **Asia/Shanghai（UTC+08:00）**，优先使用短信中心时间，缺失时使用平台接收时间，与板卡操作系统时区无关。
- 不显示模组、设备或 SIM 别名，不设置 Markdown、验证码大字、复制弹窗、强制提示音、打断级别或可见的模组分组；声音和归档遵循 Bark 客户端设置。
- 长通知仅在标题标注 `1/n`；缺片标题为 `Incomplete Message`，无法解码时为 `Message Unavailable`，使用简短英文状态提示。
- 内部稳定 ID 用于重试与对应分段补全更新，不显示给用户。展示快照与任务一起持久化；旧任务沿用已有快照，手动补发使用新 ID。
- 完整展示 JSON 控制在 3000 字节内，给 APNs 包装留出余量，超长短信无损分段。

图标来自 Apple 官方 Messages 的远程 App Store 图片，客户端可能缓存；源地址不可用时由 Bark 使用默认图标。图形版权归 Apple。接口字段参考 [Bark 官方参数](https://github.com/Finb/Bark/blob/master/docs/en-us/params.md)。

默认 `path_mode: push` 使用 `POST /push` 并在 JSON 中传入密钥。自建服务若只开放带 key 路径，可显式设置：

```yaml
bark_targets:
  - id: iphone
    url: https://your-bark-server.example
    key_env: SIMD_BARK_IPHONE_KEY
    path_mode: key
```

正文始终通过 HTTPS JSON 传输，不因失败自动切换其他路径。

## 运行语义

- 各模组独立调度，同一模组的 AT 命令串行；命令与异步短信通知分别处理。
- 启动、重连、新短信通知以及默认每 60 秒执行完整扫描，包含已读和未读记录，同时覆盖 SM/ME 存储。
- 短信和 Bark 任务同一事务提交，提交后复核 SIM 身份及存储内容，再删除模组短信。数据库失败时不清理模组存储。
- Bark 目标及推送分段独立执行，网络故障、408/429/5xx 指数退避重试，默认最长 24 小时。自动补偿期限从任务创建时计算。
- Bark 调用成功只表示其接口返回成功。崩溃恢复可能重复推送；不承诺手机已收到或已读。
- 长短信分片存入数据库，默认 5 分钟未收齐时推送不完整内容；后续收齐补发一次完整通知。硬件分片读取能力仍需实机验收。
- 无法解码的短信保留原始 PDU，推送解码失败提示，不生成虚假正文。二进制、压缩和国家语言移位编码首期不做文本解释。
- 默认保存 90 天，每日清理；未完成转发、排队发送、正在合并的短信和未确认清理的模组记录受到保护。
- 新创建的补发任务及近期执行历史也受到保留期保护；空 `bark_targets` 列表表示该模组仅归档、不转发。
- 同一数据库仅允许一个 daemon。异常退出时，将 `sending` 恢复为 `unknown`，将未完成的转发恢复为待执行。

## Linux 部署

```sh
make release
sudo useradd --system --home /var/lib/simd --shell /usr/sbin/nologin simd
sudo usermod -aG dialout simd
sudo install -m 755 bin/simd-linux-arm64 /usr/local/bin/simd
sudo install -d -m 700 -o simd -g simd /var/lib/simd
sudo install -d -m 750 /etc/simd
sudo install -m 640 -o root -g simd configs/simd.example.yaml /etc/simd/config.yaml
sudo install -m 600 deploy/simd.env.example /etc/simd/simd.env
sudo install -m 644 deploy/simd.service /etc/systemd/system/simd.service
# 编辑 config.yaml 与 simd.env，选择正确的架构、串口和密钥。
sudo systemctl daemon-reload
sudo systemctl enable --now simd
journalctl -u simd -f
```

示例配置使用相对路径，在服务工作目录下保存为 `/var/lib/simd/data/simd.db`。也可显式设置绝对路径。不同发行版的串口组可能是 `uucp`，按实际情况调整 `SupplementaryGroups`。系统串口控制台或其他模组管理器须避免占用所选端口。

远程访问需明确修改监听地址并配置 HTTPS 反向代理；首期仅有单管理员令牌。日志不包含短信正文、完整号码或密钥。SQLite 正文未加密，Bark 推送仅使用 HTTPS，内容加密属于后续功能。

## 备份、恢复与升级

1. `sudo systemctl stop simd`，确认进程已退出并释放数据库锁。
2. 备份整个数据目录（包括存在的数据库、WAL/SHM 和锁文件）以及配置；凭证单独安全保管。不要在运行中仅复制主数据库文件。
3. 恢复时保持停机，把整个数据目录恢复到同一时间点，修复目录 0700、文件 0600 及服务用户所有权后启动。
4. 数据库启动时执行完整性快速检查和事务式 schema 迁移；不支持较新 schema 时拒绝启动，不会用空库覆盖异常数据库。

恢复旧备份无法撤回已提交短信或已发送推送。备份中仍标记为排队的任务可能在有效期内执行；恢复前应核对备份时间及任务状态。

## 开发验证

```sh
go vet ./...
go test -race ./...
go test ./internal/sms -fuzz=FuzzDecodeNeverPanics -fuzztime=10s
make release
make dist VERSION=v0.1.0  # 从已提交且干净的源码生成 dist/ 压缩包和校验值
```

真实硬件收发、长短信能力、两实机并行和 24 小时稳定性观察需按 [验收清单](docs/validation.md)完成；模拟测试不替代这些验收。

## 发布与维护

`make release` 生成静态 Linux 二进制；`make dist VERSION=v0.1.0` 额外生成压缩包与 `dist/SHA256SUMS`。使用 `GO=/path/to/go` 指定工具链，`ARCHES=arm64` 可只构建 ARM64。发布包包含固定源码提交和构建时间，ARM64 指令集固定为 ARMv8.0；原始厂商资料及其虚拟环境不进入 Git 或发行包。

发布流程：运行测试，提交源码，在干净的提交上生成发行包，等待 Linux amd64/ARM64 CI 通过，创建同名版本标签，再上传压缩包及 SHA256SUMS 到 GitHub Release。构建时间默认来自源码提交时间，支持 `SOURCE_DATE_EPOCH`；不覆盖已有远端标签或发行资产。
