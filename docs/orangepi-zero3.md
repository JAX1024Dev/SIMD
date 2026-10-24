# Orange Pi Zero 3 部署指南

## 适用环境

使用 Orange Pi Zero 3 的 **64 位 Debian、Ubuntu 或 Armbian**，并使用 systemd 管理服务。该板基于 Allwinner H618 / Cortex-A53；发行包采用 `linux/arm64`、`GOARM64=v8.0`、`CGO_ENABLED=0`，不依赖目标机器的 Go、编译器或 glibc 动态库。

先确认系统与用户空间均为 64 位：

```sh
uname -m          # 预期 aarch64
getconf LONG_BIT # 预期 64
```

本项目已配置原生 Linux ARM64 CI。CI 不等于真实 Zero 3 板卡和运营商短信验收；实机未完成项目见 [validation.md](validation.md)。板卡标识可参考 [Linux 上游设备树](https://github.com/torvalds/linux/blob/master/arch/arm64/boot/dts/allwinner/sun50i-h618-orangepi-zero3.dts)。

## 下载和校验

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl openssl
curl -fLO https://github.com/JAX1024Dev/SIMD/releases/download/v0.1.0/simd_v0.1.0_linux_arm64.tar.gz
curl -fLO https://github.com/JAX1024Dev/SIMD/releases/download/v0.1.0/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf simd_v0.1.0_linux_arm64.tar.gz
cd simd_v0.1.0_linux_arm64
./simd -version
```

仅在校验显示对应 ARM64 压缩包 `OK` 后继续。`-version` 不需要配置密钥，也不会打开串口。

## 接线与串口

ML307R 的 AT 通信使用 **UART0、115200、8N1**。模组原生 UART 为 **1.8V**，需匹配板卡/转接板电平。使用裸模组时，通过适当的电平转换连接 TX、RX 和 GND；不能直接将普通 3.3V/5V TTL 或 RS232 接到模组引脚。已带转换电路的开发板按其硬件说明接线。

可选择板载 UART 或 USB-to-UART 转接器：

- 板载 UART：根据所用系统镜像的配置工具或设备树 overlay 启用目标 UART，再根据 `dmesg` 确认设备名；物理针脚以官方引脚表为准。
- USB-to-UART：优先使用 `/dev/serial/by-id/...` 稳定路径，避免重启后端口编号变化。

```sh
ls -l /dev/ttyS* /dev/ttyUSB* /dev/serial/by-id/* 2>/dev/null
dmesg | tail -50
```

配置示例的 `/dev/ttyS5` 只是示例，不保证所有镜像都使用该名称。不要与调试控制台、serial-getty 或 ModemManager 共用同一串口。仅在实际接好 RTS/CTS 时启用流控。多模组部署需为每个模组配置独立串口和设备 ID。

## 安装服务

```sh
sudo useradd --system --home /var/lib/simd --shell /usr/sbin/nologin simd
sudo usermod -aG dialout simd
sudo install -m 755 simd /usr/local/bin/simd
sudo install -d -m 700 -o simd -g simd /var/lib/simd
sudo install -d -m 750 /etc/simd
sudo install -m 640 -o root -g simd configs/simd.orangepi-zero3.yaml /etc/simd/config.yaml
sudo install -m 600 deploy/simd.env.example /etc/simd/simd.env
sudo install -m 644 deploy/simd.service /etc/systemd/system/simd.service
```

若 `simd` 用户已存在，可跳过创建命令。部分发行版串口组为 `uucp`；据实际设备组修改服务文件的 `SupplementaryGroups`。

编辑 `/etc/simd/config.yaml`，设置实际串口、Bark 基础 URL 和路径模式。编辑 `/etc/simd/simd.env`，填入：

```text
SIMD_API_TOKEN=<随机 API 令牌，至少 24 字节>
SIMD_BARK_IPHONE_KEY=<Bark device key>
```

可通过 `openssl rand -hex 24` 生成 API 令牌。保持环境文件为 root 所有、权限 0600。自建 Bark 若 `/push` 返回 403 而带 key 路径可用，在目标配置中设置 `path_mode: key`；URL 填基础地址，key 仍仅放在环境变量中。

## 启动前验证

先以只读方式确认连接；以下命令不读取、删除或发送短信：

```sh
sudo -u simd /usr/local/bin/simd -probe-port /dev/ttyS5 -probe-sms
```

把示例端口替换为实际值。若报告串口被占用或 SIM 未就绪，先处理对应硬件/系统问题。

**正常启动会将 SIM 中现有短信归档到 SQLite，并在事务提交和复核成功后删除模组对应记录。** 历史短信不推送，后续新短信按确认的简洁样式转发。

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now simd
systemctl status simd
journalctl -u simd -f
curl -f http://127.0.0.1:8080/health/live
curl -f http://127.0.0.1:8080/health/ready
```

`live` 检查核心进程；`ready` 还要求数据库可写、至少一个模组就绪。数据库保存在 `/var/lib/simd/data/`，使用私有目录及文件权限。远程 API 访问需显式修改监听地址，并配置 HTTPS 反向代理和 Bearer Token。

## 更新与备份

更新前停止服务并备份完整数据目录及配置；不要在运行时仅复制主数据库文件。schema 迁移可能使旧版本无法打开数据库，回滚时需一并恢复更新前的完整备份。

```sh
sudo systemctl stop simd
# 备份 /var/lib/simd/ 和 /etc/simd/ 到安全位置
# 校验新版本后替换 /usr/local/bin/simd
sudo systemctl start simd
```

保留 root 环境文件与数据库的原有所有权和访问权限。
