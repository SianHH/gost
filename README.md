# Changelog

- 添加hy2协议(客户端和服务端无协商速率方案，固定发包速率或使用BBR)

服务端
```shell
./gost -L relay+hy2://:11111?tx=100mbps
# 100mbps为服务端向客户端发送的最大速率
 ```

客户端
```shell
./gost -L socks5://:1080 -F relay+hy2://www.example.com:11111?tx=30mbps
# 30mbps为客户端向服务端发送的最大速率
```

可以使用obfs混淆流量特征
```shell
# 服务端
./gost -L "relay+hy2://:11111?tx=100mbps&obfs=123123123"
# 客户端
./gost -L socks5://:1080 -F "relay+hy2://www.example.com:11111?tx=30mbps&obfs=123123123"

# obfs参数长度最短为4,服务端和客户端必须使用相同的值才能正常连接
```

# GO Simple Tunnel

### GO语言实现的安全隧道

[![zh](https://img.shields.io/badge/Chinese%20README-green)](README.md) [![en](https://img.shields.io/badge/English%20README-gray)](README_en.md)

## 功能特性

- [x] [多端口监听](https://gost.run/getting-started/quick-start/)
- [x] [多级转发链](https://gost.run/concepts/chain/)
- [x] [多协议支持](https://gost.run/tutorials/protocols/overview/)
- [x] [TCP/UDP端口转发](https://gost.run/tutorials/port-forwarding/)
- [x] [反向代理](https://gost.run/tutorials/reverse-proxy/)和[隧道](https://gost.run/tutorials/reverse-proxy-tunnel/)
- [x] [TCP/UDP透明代理](https://gost.run/tutorials/redirect/)
- [x] DNS[解析](https://gost.run/concepts/resolver/)和[代理](https://gost.run/tutorials/dns/)
- [x] [TUN/TAP设备](https://gost.run/tutorials/tuntap/)
- [x] [负载均衡](https://gost.run/concepts/selector/)
- [x] [路由控制](https://gost.run/concepts/bypass/)
- [x] [准入控制](https://gost.run/concepts/admission/)
- [x] [限速限流](https://gost.run/concepts/limiter/)
- [x] [插件系统](https://gost.run/concepts/plugin/)
- [x] [Prometheus监控指标](https://gost.run/tutorials/metrics/)
- [x] [动态配置](https://gost.run/tutorials/api/config/)
- [x] [Web API](https://gost.run/tutorials/api/overview/)
- [x] [GUI](https://github.com/go-gost/gostctl)/[WebUI](https://github.com/go-gost/gost-ui)

## 概览

![Overview](https://gost.run/images/overview.png)

GOST作为隧道有三种主要使用方式。

### 正向代理

作为代理服务访问网络，可以组合使用多种协议组成转发链进行转发。

![Proxy](https://gost.run/images/proxy.png)

### 端口转发

将一个服务的端口映射到另外一个服务的端口，同样可以组合使用多种协议组成转发链进行转发。

![Forward](https://gost.run/images/forward.png)

### 反向代理

利用隧道和内网穿透将内网服务暴露到公网访问。

![Reverse Proxy](https://gost.run/images/reverse-proxy.png)

## 下载安装

### 二进制文件

[https://github.com/go-gost/gost/releases](https://github.com/go-gost/gost/releases)

### 安装脚本

```bash
# 安装最新版本 [https://github.com/go-gost/gost/releases](https://github.com/go-gost/gost/releases)
bash <(curl -fsSL https://github.com/go-gost/gost/raw/master/install.sh) --install
```
```bash
# 选择要安装的版本
bash <(curl -fsSL https://github.com/go-gost/gost/raw/master/install.sh)
```

### 源码编译

```
git clone https://github.com/go-gost/gost.git
cd gost/cmd/gost
go build
```

### Docker

```
docker run --rm gogost/gost -V
```

## 工具

### GUI

[go-gost/gostctl](https://github.com/go-gost/gostctl)

### WebUI

[go-gost/gost-ui](https://github.com/go-gost/gost-ui)

### Shadowsocks Android插件

[xausky/ShadowsocksGostPlugin](https://github.com/xausky/ShadowsocksGostPlugin)

## 帮助与支持

Wiki站点：[https://gost.run](https://gost.run)

YouTube: [https://www.youtube.com/@gost-tunnel](https://www.youtube.com/@gost-tunnel)

Telegram：[https://t.me/gogost](https://t.me/gogost)

Google讨论组：[https://groups.google.com/d/forum/go-gost](https://groups.google.com/d/forum/go-gost)

旧版入口：[v2.gost.run](https://v2.gost.run)
