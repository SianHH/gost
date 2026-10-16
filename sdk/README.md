# gost SDK

把 [gost](https://github.com/go-gost/gost) 封装成 C ABI 动态库，供 Flutter 通过 `dart:ffi`
在**应用进程内**启动/停止一个 gost 实例。不依赖外部可执行文件——Android 10+ 本来也不允许 App
执行自带二进制。

产物：

| 平台 | 文件 |
| --- | --- |
| Android | `jniLibs/{arm64-v8a,x86_64}/libgost.so` |
| Windows / Linux / macOS | `build/gost.dll` / `gost.so` / `gost.dylib` |
| 通用 | `libgost.h`（cgo 生成，两个 ABI 相同） |

## 目录结构

```
sdk/
├── libgost.go            c-shared 入口：cgo preamble + //export 函数 + func main() {}
├── engine/               生命周期逻辑，无 cgo，可纯 Go 单测
│   ├── engine.go         Engine：Start / Stop / Reload
│   ├── run.go            起 service goroutine，管理 API / metrics / profiling server
│   ├── apiservice.go     内置 Web API 与 metrics 服务的构造
│   ├── sink.go           日志汇聚：有界队列 + 单 drain goroutine
│   └── logging.go        stderr 管道重定向
├── register/
│   ├── register.go       移动端组件集（默认）
│   └── register_all.go   //go:build gost_all —— 与 CLI 完全一致的全量集
└── scripts/
    ├── build-android.sh  产出 jniLibs/
    ├── build-native.sh   产出 build/（Linux / macOS）
    └── build-native.ps1  产出 build/（Windows）
```

`//export` 与 cgo preamble 只能位于被构建为 c-shared 的 `package main`，而 main 包无法被测试
代码 import，所以真正的逻辑都放在 `engine` 里。

## 构建

### Android

```sh
bash scripts/build-android.sh                 # arm64-v8a + x86_64（默认）
bash scripts/build-android.sh arm64-v8a       # 单个 ABI
ANDROID_NDK_HOME=/path/to/ndk bash scripts/build-android.sh
```

脚本自动从 `$ANDROID_NDK_HOME`、`$LOCALAPPDATA/Android/Sdk/ndk` 等位置找 NDK，取版本最高的一个。
最低 API level 为 21（可用 `API_LEVEL` 覆盖）。

NDK 28 默认启用 16 KB 页对齐，产物天然满足 Android 15+ 的要求；可用
`readelf -l jniLibs/arm64-v8a/libgost.so | grep LOAD` 确认 `Align` 为 `0x4000`。

### 桌面（调试用，无需设备）

```sh
bash scripts/build-native.sh                  # Linux / macOS / Git Bash
TAGS=gost_all bash scripts/build-native.sh    # 全量组件集
```

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build-native.ps1
```

`-buildmode=c-shared` 是 cgo 特性，因此构建时必须 `CGO_ENABLED=1` 且有可用的 C 编译器。
**Android 上尤其不能关掉 cgo**：纯 Go 的 DNS resolver 会去读 `/etc/resolv.conf`，而 Android
没有这个文件，所有域名解析都会失败。

### 版本号

版本从 `gost/cmd/gost/version.go` 读取，通过
`-ldflags "-X github.com/go-gost/sdk/engine.version=..."` 写入 `engine` 包，由 `gost_version()`
返回。不传 `-ldflags` 时是 `engine` 包里的默认值。

### 组件集

默认是**移动端精简集**（`register/register.go`）：与 CLI 相同的列表，但去掉了在 Android 应用
沙箱里必然失败的部分——`tun` / `tungo` / `tap` / `stdio` / `serial` / `icmp` / `redirect`。
它们需要 `/dev/net/tun`、`CAP_NET_ADMIN`、`CAP_NET_RAW` 或真实 tty，普通 App 都拿不到；去掉它们
同时也去掉了 gvisor + tun2socks + gopacket 这一整棵依赖树。

需要全量集时加 `-tags gost_all`（例如排查"某个协议怎么没注册上"）：

```sh
TAGS=gost_all bash scripts/build-native.sh
TAGS=gost_all bash scripts/build-android.sh
```

## C API

完整声明见生成的 `libgost.h`：

```c
/* level: 0=trace 1=debug 2=info 3=warn 4=error 5=fatal, msg: NUL-terminated UTF-8 */
typedef void (*gost_log_cb)(int level, const char *msg);

extern char* gost_version(void);
extern void  gost_set_log_callback(gost_log_cb fn);
extern void  gost_set_log_level(int level);
extern int   gost_log_level(void);
extern int   gost_start(char* config, char** errOut);
extern int   gost_stop(char** errOut);
extern int   gost_reload(char** errOut);
extern int   gost_is_running(void);
extern void  gost_free(char* p);
```

约定：

- **返回值**：`gost_start` / `gost_stop` / `gost_reload` 返回 0 表示成功，非 0 表示失败。
- **错误串**：失败时 `*errOut` 收到一段 UTF-8 字符串，**必须**用 `gost_free` 释放；成功时
  写入 NULL。`errOut` 传 NULL 表示不关心错误信息。
- **`gost_version` 的返回值是库持有的只读常量，不要 free。**
- **panic 安全**：每个导出函数入口都捕获 panic 并转成错误串返回。在 Android 上 panic 跨 cgo
  边界会直接杀进程，宿主将一无所知。
- **线程安全**：任意 Dart isolate 都可以调用。锁只加在 start/stop/reload 这类低频路径上，
  数据面不受影响。
- **日志回调**在一条专用 goroutine 上被调用，**不在** gost 的业务 goroutine 上，因此回调里
  可以安全地反过来调用本库的任意导出函数（比如 `gost_stop`）。但回调不能长时间阻塞——队列
  满时日志会被丢弃而不是阻塞数据面。

### 配置格式

`config` 是 UTF-8 的 JSON 或 YAML，NUL 结尾。传 NULL 时 gost 回退到 `GOST_*` 环境变量和默认
配置搜索路径。

配置里可以使用字符串形式的时长（`"10s"`、`"5m"`）——引擎会把配置落到临时文件后以*路径*形式
交给解析器，从而走 viper 的 decode hook。

### 内置 Web API

`gost_start` 没有暴露 API 地址参数（C API 目前只做 start/stop），要开 Web API 就在配置里写：

```json
{ "api": { "addr": "127.0.0.1:18080" }, "services": [ ... ] }
```

> **Web API 默认无鉴权**：`auth` / `auther` 都为空时中间件直接放行。移动端**不要**绑到
> `0.0.0.0`，并在 `api.auth` 里带上用户名密码。默认建议不开。

配置里的 `metrics` 和 `profiling` 会被引擎强制关闭：它们在移动端没什么用，还会白占端口
（profiling 默认 `:6060`）。

## Flutter 集成

把 `jniLibs/` 拷到 Flutter 工程的 `android/app/src/main/jniLibs/`，Gradle 会自动打包进 APK：

```sh
cp -r sdk/jniLibs android/app/src/main/jniLibs
```

Dart 侧：

```dart
import 'dart:ffi';
import 'dart:convert';
import 'package:ffi/ffi.dart';

typedef _VersionC = Pointer<Utf8> Function();
typedef _VersionD = Pointer<Utf8> Function();
typedef _SetLogCbC = Void Function(Pointer<NativeFunction<Void Function(Int32, Pointer<Utf8>)>>);
typedef _StartC = Int32 Function(Pointer<Utf8>, Pointer<Pointer<Utf8>>);
typedef _StartD = int Function(Pointer<Utf8>, Pointer<Pointer<Utf8>>);
typedef _StopC = Int32 Function(Pointer<Pointer<Utf8>>);
typedef _StopD = int Function(Pointer<Pointer<Utf8>>);
typedef _FreeC = Void Function(Pointer<Utf8>);
typedef _FreeD = void Function(Pointer<Utf8>);

class Gost {
  final DynamicLibrary _lib;
  Gost._(this._lib);

  factory Gost.open() => Gost._(DynamicLibrary.open('libgost.so'));

  late final _version = _lib.lookupFunction<_VersionC, _VersionD>('gost_version');
  late final _start = _lib.lookupFunction<_StartC, _StartD>('gost_start');
  late final _stop = _lib.lookupFunction<_StopC, _StopD>('gost_stop');
  late final _free = _lib.lookupFunction<_FreeC, _FreeD>('gost_free');

  String get version => _version().toDartString();

  /// 抛出的异常里带上库返回的详细错误。
  String _takeError(Pointer<Pointer<Utf8>> out) {
    final p = out.value;
    if (p == nullptr) return '';
    final msg = p.toDartString();
    _free(p);
    return msg;
  }

  void start(Map<String, dynamic> config) {
    final err = malloc<Pointer<Utf8>>();
    final cfg = jsonEncode(config).toNativeUtf8();
    try {
      final rc = _start(cfg, err);
      if (rc != 0) throw StateError('gost_start failed: ${_takeError(err)}');
    } finally {
      malloc.free(cfg);
      malloc.free(err);
    }
  }
  // stop / reload 同形，只是没有 cfg 参数
}
```

日志回调要用 `NativeCallable.isolateLocal`（Dart 2.17+）或把 level/msg 投递到 Dart isolate 的
Port；直接用 `Pointer.fromFunction` 在 isolate 之外调用会崩：

```dart
final cb = NativeCallable<Void Function(Int32, Pointer<Utf8>)>.isolateLocal(
  (int level, Pointer<Utf8> msg) {
    // msg 只在本次调用内有效，不要保存指针
    print('[gost ${_levels[level]}] ${msg.toDartString()}');
  },
);
_lib.lookupFunction<...>('gost_set_log_callback')(cb.nativeFunction);
```

`NativeCallable` 需要保持强引用，否则会被 GC 掉。

### 桌面调试

在 PC 上先跑通同一套 FFI，省去装模拟器的来回：

```sh
bash scripts/build-native.sh
```

然后把 `build/gost.dll`（Windows）/ `gost.so`（Linux）/ `gost.dylib`（macOS）放到 Flutter
可执行文件旁边，`DynamicLibrary.open` 用对应文件名。

### C 冒烟测试

`scripts/smoke.c` 用**运行时加载**的方式（和 dart:ffi 完全一样的流程）验证整条 ABI：解析全部
符号、start 一个 socks5 监听、reload、stop 后确认端口释放、重复 start 必须失败、非法配置必须
被拒绝。它不链接 libgost，因此通过即说明导出的 ABI 可用。

```sh
# Windows
gcc -o build/smoke.exe scripts/smoke.c -lws2_32
./build/smoke.exe build/gost.dll

# Linux / macOS
cc -o build/smoke scripts/smoke.c -ldl
./build/smoke build/gost.so
```

## 注意事项

- **一个进程只能跑一个实例**。gost 的 registry、默认 logger、metrics 开关都是包级全局状态，
  `Engine` 本身也是单例。Dart 侧开多个 isolate 也共享同一个引擎。
- **仅支持 >= 1024 的端口 + 用户态代理协议**。绑 `:80` / `:443` 之类的特权端口、以及任何
  TUN/TAP、raw socket、`SO_BINDTODEVICE` / `SO_MARK` 场景在非 root 设备上会失败或静默降级。
- **不要混用两种配置下发方式**。`gost_reload`（以及 Web API 的 `POST /config/reload`）会
  重新解析 `gost_start` 传入的那份配置，因此**会丢弃通过 Web API 动态增删的服务**。要么整体
  start/stop 下发，要么只用 Web API 动态改。
- **`os.Stderr` 重定向是进程级副作用**。为了让绕过结构化 logger 的日志（gin 的 panic 兜底、
  标准库 `log`、第三方 `fmt.Fprintf(os.Stderr, ...)`）也能到达宿主，引擎把 Go 的 `os.Stderr`
  变量指向了一个管道。它**不做 `dup2`**，所以宿主原生层打到 fd 2 的内容不受影响，但同一进程里
  其他 Go 代码会受影响（纯 Flutter 场景下没有别人，除非这个库被多个插件共用）。调用
  `gost_set_log_callback(NULL)` 可以关掉转发。
- **`gost_start` 成功后再调一次会失败**（返回 `gost: already running`），不会隐式重启——宿主
  需要确切知道上一轮的端口是什么时候释放的。改配置请走 `gost_stop` + `gost_start`（会断开所有
  连接）或 `gost_reload`（原地重载）。
- **体积**：精简集 `-ldflags "-s -w"` 后 arm64-v8a 约 46 MB、x86_64 约 50 MB。要更小可以把
  listener/dialer 的注册集也参数化，需要时再加 build tag。

## 测试

```sh
cd sdk
go test ./engine/     # 纯 Go 单测：起停、端口释放、重载、日志转发、时长字符串
go vet ./...
gofmt -l .
```
