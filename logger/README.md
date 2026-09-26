# logger

基于标准库 `log/slog` 的日志包，**零包装直出 `*slog.Logger`**。

设计原则：应用只在**初始化时** import 本包，拿到 `*slog.Logger` 后，打日志完全使用 `log/slog` 原生 API（`slog.Info(...)` / `l.InfoContext(ctx, ...)`），不引入任何自有日志类型。

- **官方标准**：无自定义 Logger 接口，`New` 直接返回 `*slog.Logger` 并接入 `slog.SetDefault`
- **开箱即用**：统一初始化入口，`service`/`env` 全局字段自动注入
- **链路追踪**：`*Context` 方法自动从 `context.Context` 提取 `trace_id`/`req_id` 注入日志属性
- **日志切割**：lumberjack 按大小轮换 / 自研按日期轮换（+gzip+清理）
- **可选装饰器**：异步写入、敏感信息打码、日志采样、错误堆栈、动态调级（全部基于 `slog.Handler`，按需启用）
- **双端输出**：控制台彩色文本 / 文件 JSON 或 text（`WithFormat`，即 console 渲染器 NoColor 形态）

依赖：仅 `gopkg.in/natefinch/lumberjack.v2`。

## 快速开始

```go
import (
    "log/slog"

    "github.com/charlienet/gadget/logger"
)

func main() {
    // 包加载时 logger.DefaultLogger 已就绪并完成 slog.SetDefault，
    // 零配置即可用 slog 包级函数（stdout 彩色输出、Info 级别）
    slog.Info("service started", "port", 8080)
    slog.Error("db miss", slog.String("user_id", "u1001"))
}
```

## 初始化

### Option 模式

```go
l := logger.New(
    logger.WithService("opencode-api"),   // 全局 service 字段（非空才注入）
    logger.WithEnv("prod"),               // 全局 env 字段
    logger.WithLevel(slog.LevelDebug),    // 静态级别
    logger.WithConsole(                   // 控制台 sink：writer 缺省 os.Stdout、颜色自动、版式 text
        logger.WithConsoleColor(true),    //   可选强制彩色；亦可 WithConsoleWriter(w) 指定 writer
        // logger.WithConsoleFormat(logger.FormatJSON), // 可选：JSON 记录写 console（颜色失效，见「输出格式支持矩阵」）
    ),
    logger.WithFile("./logs/app.log",     // 文件 sink（默认 JSON）
        logger.WithMaxSize(100),          // MB，默认 100
        logger.WithMaxAge(30),            // 天，默认 30
        logger.WithMaxBackups(10),        // 默认 10
        logger.WithCompress(true),        // 默认 true
        logger.WithFormat(logger.FormatText), // 可选：text（console 渲染器 NoColor 形态，默认 FormatJSON）
        logger.WithDateRotate("2006-01-02"),  // 可选：按日期轮换
    ),
    logger.WithAsync(),                   // 可选：异步写入（默认队列 10240）
)
// l 是 *slog.Logger，注入业务代码即可

defer logger.Close(0) // 进程退出前 flush 异步队列、关闭文件句柄与远端 sink（syslog / http）
```

> **sink 按「存在性」装配**：`WithConsole(...)` 启用控制台、`WithFile(...)` 启用文件、`WithSyslog(...)` 启用
> syslog、`WithHTTP(...)` 启用 http，各为独立开关（可任意组合，多路经同一 MultiHandler 扇出）。皆不声明时兜底一个 stdout 控制台
> （`logger.New()` 零配置即用、包级日志不静默）；仅声明非 console sink（file / syslog / http）则不写 stdout；
> `WithConsole` + 其它即双/多端输出。

### Config + Init（结构体配置驱动）

后端按 `Outputs` 分组声明：**某节点非 nil = 启用该后端，nil = 不启用**；不再有 `output: console/file/both`
字符串开关。各 sink 节点皆 nil 时由 `New` 兜底 stdout 控制台（显式声明了 `file` / `syslog` / `http` 而未声明 `console`
时不兜底 stdout）。**配置格式解析（yaml/json 等）由应用端负责**——
`Config` 及其子结构的每个导出字段携带 `yaml` / `json` / `mapstructure` tag（键名统一 snake_case，层级嵌套由子结构
自然承载），支持应用端直接嵌入结构体（如 `Logger: logger.Config`）后用对应解码器填充；
logger 库本身不含任何解析代码，`Init` 只接收装配好的结构体。

```go
cfg := logger.Config{
    Level:     "info",          // trace | debug | info | warn | error | fatal
    Service:   "opencode-api",
    Env:       "prod",
    Source:    false,           // 输出 file:line
    Async:     true,
    QueueSize: 10240,
    Sensitive: logger.SensitiveConfig{
        Keys: []string{"mytoken"}, // 可选：追加敏感字段词(子串匹配、与内置词集合并)，nil/空=不注入
        Mask: "[REDACTED]",        // 可选：自定义掩码(默认 ******)，空=不注入
    },
    Outputs: logger.OutputsConfig{
        Console: &logger.ConsoleConfig{}, // 指针非 nil=启用控制台；置 nil=不启用
        // NoColor: true 强制关闭颜色；默认 false=自动（终端支持 ANSI 即应用，跟随 NO_COLOR）
        // Format: "json" 可切 JSON 记录版式；默认空=text（注意：与 file 的空=json 相反，见「输出格式支持矩阵」）
        File: &logger.FileConfig{ // File 节点非 nil=启用文件；置 nil=不启用（启用时 Filename 必填，见下方黑洞校验）
            Filename:   "logs/app.log",
            Format:     "json", // json(默认)/text（大小写不敏感，非法非空值报错）
            MaxSize:    100,    // MB
            MaxAge:     30,     // 天
            MaxBackups: 10,
            Compress:   true,
            // Layout: "2006-01-02", // 可选：非空=按日期轮换(WithDateRotate)，空=lumberjack 按大小轮换
        },
    },
}

if err := logger.Init(cfg); err != nil { // 内部自动 slog.SetDefault
    log.Fatalf("init logger: %v", err)
}

// 变参精调：Init(cfg, opts...) 把 Config 打底为分组 Option，再叠加用户 opts（同名后者胜）
if err := logger.Init(cfg, logger.WithSensitiveKeys("password")); err != nil {
    log.Fatalf("init logger: %v", err)
}
```

启用文件后端时可参考 `DefaultConfig` 文档中的默认参数模板（MaxSize 100 / MaxAge 30 /
MaxBackups 10 / Compress true / Format "json"）；`DefaultConfig()` 本身返回纯 console
默认（`Outputs.Console` 非 nil、`Outputs.File` 为 nil），等价旧 `output: "console"`。

配置文件驱动时，应用端自行解码后传入即可。字段 tag 对应的 yaml 键名示意（仅示意，
解析动作在应用端，如 `yaml.Unmarshal` / viper / mapstructure）：

```yaml
# 与 logger.Config 的 yaml tag 对齐（snake_case；outputs 下某节存在 = 启用该后端）
level: "info"
service: "opencode-api"
env: "prod"
source: false
async: true
queue_size: 10240
sensitive:            # 可选：横切打码（console/file 双端生效），整节缺省=不注入
  keys: ["mytoken"]
  mask: "[REDACTED]"
outputs:
  console:            # 节存在=启用控制台；缺省=不启用
    no_color: false   #   true 强制关闭颜色；默认 false=自动（ANSI + TTY + 无 NO_COLOR）
    format: "text"    #   text(默认)/json；空=text（与 file 的空=json 相反！），非法非空值 Init 报错
  file:               # 节存在=启用文件；缺省=不启用（存在时 filename 必填，见下方黑洞校验）
    filename: "logs/app.log"
    format: "json"    #   json(默认)/text（大小写不敏感，非法非空值报错）
    max_size: 100     #   MB
    max_age: 30       #   天
    max_backups: 10
    compress: true
    layout: "2006-01-02" # 可选：非空=按日期轮换(WithDateRotate)，缺省/空=lumberjack 按大小轮换
  http:                 # 节存在=启用 http 后端；缺省=不启用（存在时 url 必填，见下方黑洞校验）
    url: "http://127.0.0.1:8686/v1/logs" # 完整 POST 端点（含路径），scheme 必须 http/https
    headers:            #   附加请求头（认证等由应用端注入）
      Authorization: "Bearer <token>"
    batch_size: 100     #   每次 POST 最大条数，0/缺省=100，负数 Init 报错
    flush_interval: 500ms #  不满批强制投递间隔，空默认 500ms；非法/负值 Init 报错
    timeout: 5s         #   整请求超时（含重试单次尝试），空默认 5s；非法/负值 Init 报错
    gzip: false         #   请求体 gzip 压缩（置 Content-Encoding: gzip）
    format: "text"      #   message 正文版式 text(默认)/json；空=text，非法非空值 Init 报错（信封不变，见「输出格式支持矩阵」）
```

> **控制台颜色 `ConsoleConfig.NoColor` 两态**：`true` 强制关闭颜色；**`false`（默认零值）** 走自动判定（终端支持 ANSI、输出为 TTY 且无 `NO_COLOR` 环境变量三者同时满足才涂色，管道 / 重定向到文件自动无 ANSI）。仅作用于非 nil 的 `Console` 节点；`Console` 为 nil（不启用控制台）不受影响。
> `DefaultConfig()` 对 `NoColor` 保持零值 `false`（自动判定），开发终端有色、生产非 TTY 无色，无需按环境专门配置；若确需强制开色（非 TTY / 管道也输出 ANSI），Config 层不提供，请用 Option 精调：`logger.Init(cfg, logger.WithConsole(logger.WithConsoleColor(true)))`。

> `Init(cfg, opts...)` 先由 Config 生成打底 Option、再拼接用户 opts（后者覆盖同类项），**合并后**才校验：
> `Outputs.File` 节点非 nil 但合并后 `Filename` 仍为空 → 报错（黑洞；可用 `WithFile(path)` 补路径消除）；
> `FileConfig.Format` 为非法非空值 → 报错（不静默回退 JSON）；
> `Outputs.Syslog` 节点非 nil 时：合并后 `Address` 为空、`Network` 非 `""`/`tcp`/`udp`、`Facility` 非法名、
> `Timeout` 非空但无法 `time.ParseDuration` 解析、`Format` 非法 → 一并报错。任一失败均**不**改动 `DefaultLogger`。
> 注意 syslog 连接为**懒建 / 后台重连**，地址不可达**不在** `Init` 报错（与 file 的 lumberjack 惰性 IO 语义一致）。
> `Outputs.HTTP` 节点非 nil 时：合并后 `URL` 为空、`URL` 无法 `url.Parse` 解析或 scheme 非 `http`/`https`、
> `BatchSize` 负数、`Timeout` / `FlushInterval` 非空但无法解析或为负值、`Format` 非法非空值 → 一并报错。http 连接同样**懒建**
> （首个批次发送时才拨号），端点不可达**不在** `Init` 报错。
> `Outputs.Console` 节点非 nil 且 `Format` 为非法非空值 → 报错（**空 = 默认 text**，与 `File`/`Syslog`
> 的「空 = json」缺省语义相反，见「输出格式支持矩阵」）。
> `Init` / `New` 每次替换默认实例前会关闭（flush + 释放句柄）上一个默认实例，见「设计说明」。

> **Config 能力映射**：`FileConfig.Layout` 非空 → `WithDateRotate(layout)`（按日期轮换，仅 `File` 节点消费，空则维持 lumberjack 按大小轮换）；`Sensitive.Keys` 非空 → `WithSensitiveKeys(...)`（追加词、与内置词集合并）、`Sensitive.Mask` 非空 → `WithSensitiveMask(...)`——敏感打码为横切能力，console/file 双端生效。二者**零值均不注入**对应 Option（默认行为不变，`DefaultConfig` 敏感配置 nil/空）；Init 打底后用户 opts 可再追加（`WithSensitiveKeys` 为 append 语义，两组词都生效）。

## 输出格式支持矩阵（Format per-backend）

四类后端各自独立可选 `Format`（`FileFormat` 为**通用输出格式枚举**，`FormatText` / `FormatJSON`，
类型名沿用历史不表示"仅文件"）。**全部默认值即历史行为**，不配置零变化：

| 后端 | Config 字段 | Option 子选项 | 默认 | 可选值 | 作用范围 |
| --- | --- | --- | --- | --- | --- |
| console | `ConsoleConfig.Format` | `WithConsoleFormat` | `text` | text / json | 整行版式；json=标准 JSON 记录写 console writer，**颜色语义失效**（NoColor/自动判定不参与） |
| file | `FileConfig.Format` | `WithFormat` | `json` | json / text | 整行版式（text 即 console 渲染器 NoColor 形态） |
| syslog | `SyslogConfig.Format` | `WithSyslogFormat` | `json` | json / text | 仅 **MSG 体**；RFC5424 报文头（PRI/TIMESTAMP/HOSTNAME/…）恒定 |
| http | `HTTPConfig.Format` | `WithHTTPFormat` | `text` | text / json | 仅帧内 **message 正文**；传输信封 `{"src","message"}` 与单行 NDJSON 帧形**恒定** |

- **默认值不对称是有意为之**：console / http 的空串 = `text`（接入契约推荐形态），file / syslog 的
  空串 = `json`（历史默认）；`ParseFileFormat` 的空串 JSON 回退不适用于 console / http 的 Config
  映射——两节点空串**不注入** Format Option，非法非空值一律 `Init` 报错（不静默回退）；
- **console json 形态**：与 file / syslog / http 的 json 渲染同源（`slog.NewJSONHandler` + time 定制
  `2006-01-02 15:04:05.000`），字段为 `msg/level/time/attrs` 标准记录；零 sink 兜底的 stdout
  控制台保持 text 版式不受影响；
- **http json 形态**：`message` 变为整条 JSON 记录的渲染串（外层信封 Marshal 自动二次转义，
  帧仍恒单行）。对端落盘内容随之是一行 JSON 文本而非裸值行——这是**发送方的内容选择**，
  `src` 归属、两键信封、NDJSON 分帧等**传输契约均不变**，对端仍可把 message 文本再
  `json.Unmarshal` 还原结构化记录；默认 text 仍是接入指导推荐形态。

## syslog 后端（RFC5424 → 远端 Vector）

把日志以 **RFC5424 报文**逐条发送到远端 syslog 服务，报文以 `\n` 分帧。适配对端
**Vector 0.58 `sources.syslog`**（`mode: tcp`/`udp`，自动识别 RFC5424）。连接为**懒建**（首条
消息时拨号）、**断线后台重连**（下次写入重试 dial），失败期间该条丢弃并向 stderr 输出**限流**
告警（同一原因每 5s 至多 1 条，防洪泛），**绝不阻塞日志调用方超过 `Timeout`**。

### Go 构造（Option 模式）

```go
l := logger.New(
    logger.WithSyslog("127.0.0.1:6514", // 远端地址（必填）
        logger.WithSyslogNetwork("tcp"),           // "tcp"(默认)/"udp"
        logger.WithSyslogTag("opencode-api"),      // RFC5424 APP-NAME；空回退 Service，再空回退 "gadget"
        logger.WithSyslogHostname("opencode-api"),     // HOSTNAME 位=落盘归属（建议服务名）；空回退 Service，再空回退 os.Hostname()
        logger.WithSyslogFacility("local0"),       // 默认 "user"；见下方设施名表
        logger.WithSyslogFormat(logger.FormatJSON),// MSG 体："json"(默认)/"text"
        logger.WithSyslogTimeout(5*time.Second),   // 单次 dial/write 超时（默认 5s）
    ),
)
defer logger.Close(0) // 关闭 syslog 连接（此后不再重连写出），与 flush/文件释放同链
```

### Config + Init（yaml 示意）

```go
cfg := logger.Config{
    Level:   "info",
    Service: "opencode-api", // 未设 Tag 时作为 APP-NAME 回退值
    Outputs: logger.OutputsConfig{
        Syslog: &logger.SyslogConfig{ // 节点非 nil=启用；置 nil=不启用
            Network:  "tcp",
            Address:  "127.0.0.1:6514", // 必填
            Tag:      "opencode-api",
            Facility: "local0",
            Format:   "json",
            Timeout:  "5s", // time.ParseDuration
        },
    },
}
_ = logger.Init(cfg)
```

```yaml
outputs:
  syslog:                 # 节存在=启用；缺省=不启用（address 必填，见黑洞校验）
    network: "tcp"        #   "tcp"(默认)/"udp"，其它值 Init 报错
    address: "127.0.0.1:6514" # 非空
    tag: "opencode-api"   #   空回退 service，再空回退 "gadget"
    hostname: ""          #   HOSTNAME 位=落盘归属（建议服务名）；空回退 service，再空回退 os.Hostname()
    facility: "local0"    #   空默认 "user"；非法名 Init 报错
    format: "json"        #   json(默认)/text
    timeout: "5s"         #   time.ParseDuration，空默认 5s；非法值 Init 报错
```

> 未声明 `Console` 时，仅启用 `syslog`（或 `file`）不兜底 stdout 控制台；如需 console+syslog
> 双端，额外加 `Console: &ConsoleConfig{}` 节点（或 `WithConsole()`）。

### 报文格式与级别映射

单条报文：`<PRI>1 <RFC3339毫秒> <HOSTNAME> <APP-NAME> <PROCID> <MSGID> <SD-ID> <MSG>\n`

- **PRI = facility × 8 + severity**；
- **HOSTNAME 位 = 对端落盘归属（src 语义）**：建议显式配置服务名；缺省链
  `Hostname` > `Config.Service` > `os.Hostname()`（见上 Option / yaml 注释）；
- **TIMESTAMP** 采用 Go 布局 `2006-01-02T15:04:05.000Z07:00`：输出恒为 UTC `Z` 或
  `+HH:MM` **带冒号**形态——规避对端实测的 `+0800` 无冒号形态被 Vector syslog source
  **静默丢帧**的坑（本机时区非 UTC 时 Go 也只会产出带冒号偏移，无需特殊配置）；
- **PROCID** 为真实进程号（`os.Getpid()`）、**MSGID/SD-ID** 固定 `-`：三者不会同时为 `-`
  （Vector 实测对「procid/msgid/sd 全 `-`」会拒收，填真实 pid 即规避）；
- **MSG** 体为单行：`json` 走标准 `slog` JSON、`text` 走 console 渲染器 NoColor 形态，与
  文件 sink **同源复用**（保证无裸换行、可反序列化）。

| slog 级别 | syslog severity | 编号 |
| --- | --- | --- |
| trace / debug | debug | 7 |
| info | informational | 6 |
| warn | warning | 4 |
| error / fatal | err | 3 |

设施名（`facility`）→ 编号：`kern`0、`user`1、`mail`2、`daemon`3、`auth`4、`syslog`5、`lpr`6、
`news`7、`uucp`8、`cron`9、`authpriv`10、`ftp`11、`ntp`12、`security`13、`console`14、
`solaris-cron`15、`local0`16 … `local7`23（大小写不敏感、自动 trim）。

### 与 Vector syslog source 对接

Vector 侧示例（newline 分帧 + 自动识别 RFC5424）：

```yaml
sources:
  in:
    type: syslog
    mode: tcp        # 或 udp
    address: 0.0.0.0:6514
```

- **分帧**：本后端每条报文以 `\n` 结尾，Vector syslog source 按 newline 拆帧、自动解析 RFC5424 头部；
- **单帧上限 102400（超限客户端截断）**：对端 syslog source 实配 `max_length: 102400`，
  超限帧会被对端**静默丢弃**，截断责任在客户端——本后端对整帧（含尾 `\n`）超过 102400 字节的
  报文自动截短 MSG 体并保留 `…[truncated]` 收尾标记，截断点回退到 UTF-8 字符边界（绝不产出
  残缺多字节序列）；截断是确定性策略而非故障，不告警、不计 dropped（包内可观测计数）；
- **UDP ≤4KB**：UDP 每条报文为**单个数据报**（不拆包），且 IPv4 UDP 载荷受协议上限约束
  （截断到 102400 的极端帧在 UDP 下也会因超过单数据报上限而写失败），建议 `format: json` +
  控制属性体量使整包不超过约 4KB，避免超过 MTU 触发 IP 分片被丢弃（`Timeout` 仅约束读写，
  不重试 UDP 丢包）；超长日志场景请走 `network: tcp`；
- **失败语义**：连接建立或写入失败 → 该条丢弃 + 下次写入重试 dial + stderr 限流告警（每原因每 5s ≤1 条）；
  进程退出调用 `logger.Close` 关闭连接后不再重连写出。日志可靠性为「尽力而为」，需要不丢日志请配
  `WithAsync` 由异步队列削峰（队列满丢弃策略见「运行时管理」）；
- **端点与网络可达性**（监听地址、容器网络与服务名解析等部署事实）以**对端接入指导为准**，
  本文不固定具体地址。

## http 后端（NDJSON 批量 → 远端 Vector）

把日志以 **NDJSON 批量 POST** 到远端 HTTP 收集端（一个请求携带多行 JSON）。适配对端
**Vector 0.58 `sources.http_server`**（`codec: json` 逐行解码）。发送在**独立 worker goroutine**
里完成（日志调用方 `Handle` 只做「加锁 + 渲染 + 追加缓冲」，**绝不做网络 IO**），因此本后端
无需开启 `WithAsync` 也不会把网络耗时压到业务线程上。

### Go 构造（Option 模式）

```go
l := logger.New(
    logger.WithHTTP("http://127.0.0.1:8686/v1/logs", // 完整 POST 端点（含路径，必填）
        logger.WithHTTPSrc("order-svc"),             // 落盘帧 src（服务归属）；空回退 Service，再空回退 os.Hostname()；越界字符 sanitize 为 '-'
        logger.WithHTTPHeaders(map[string]string{     // 附加请求头（认证等由应用端注入）
            "Authorization": "Bearer <token>",        //   logger 库不含鉴权语义，token 从哪来由应用决定
        }),
        logger.WithHTTPBatchSize(100),                // 每次 POST 最大条数（默认 100）
        logger.WithHTTPFlushInterval(500*time.Millisecond), // 不满批强制投递间隔（默认 500ms）
        logger.WithHTTPTimeout(5*time.Second),        // 整请求超时，含重试的单次尝试（默认 5s）
        logger.WithHTTPGzip(false),                   // 请求体 gzip 压缩（置 Content-Encoding: gzip）
    ),
)
defer logger.Close(0) // 停发送 worker + 把残余缓冲批最后发一次，与 flush/文件释放同链
```

### Config + Init（yaml 示意）

```go
cfg := logger.Config{
    Level:   "info",
    Service: "opencode-api",
    Outputs: logger.OutputsConfig{
        HTTP: &logger.HTTPConfig{ // 节点非 nil=启用；置 nil=不启用
            URL:           "http://127.0.0.1:8686/v1/logs", // 必填
            Src:           "opencode-api", // 落盘帧 src；空回退 Service，再空回退 os.Hostname()
            Headers:       map[string]string{"Authorization": "Bearer <token>"},
            BatchSize:     100,   // 0=默认 100；负数 Init 报错
            FlushInterval: "500ms",
            Timeout:       "5s",
            Gzip:          false,
        },
    },
}
_ = logger.Init(cfg)
```

```yaml
outputs:
  http:                 # 节存在=启用；缺省=不启用（url 必填，见下方黑洞校验）
    url: "http://127.0.0.1:8686/v1/logs"
    src: "opencode-api" # 落盘帧 src（服务归属）；空回退 service，再空回退 os.Hostname()；越界字符 sanitize 为 '-'
    headers:
      Authorization: "Bearer <token>"
    batch_size: 100     # 0/缺省=默认 100；负数 Init 报错
    flush_interval: 500ms # 空默认 500ms；非法/负值 Init 报错
    timeout: 5s         # 空默认 5s；非法/负值 Init 报错
    gzip: false
```

> 未声明 `Console` 时，仅启用 `http`（或 `file` / `syslog`）不兜底 stdout 控制台；如需 console+http
> 双端，额外加 `Console: &ConsoleConfig{}` 节点（或 `WithConsole()`）。

### 请求与帧格式

- **方法 / 路径**：固定 `POST`，请求发到 `URL` 指定的**完整端点**（含路径）。Vector 侧
  `sources.http_server.path` 默认是 `/` 且为**精确匹配**，所以两端路径必须写一致：
  Vector 配 `path: /v1/logs` ↔ 客户端 `url: http://host:8686/v1/logs`。若希望按前缀接收
  （例如同时接 `/v1/logs` 与 `/v1/logs/*`），需按所用 Vector 版本开启 `strict_path: false`
  （该选项并非所有版本都有，以官方文档为准）；本库不做客户端健康检查。
- **请求体**：NDJSON——每行一个 JSON 对象（帧形见下「落盘字段契约」）、行间单个 `\n`、
  **末尾亦以单个 `\n` 结束**（发送前统一补齐；gzip 压缩的即加尾后的完整体），不依赖对端
  「EOF 时刷出无尾换行残余」的行为差异。
  **不发** v2 批格式
  `{"logs":[{"event","metadata"}]}`（会被 json codec 当作**单个**不透明事件）。
- **落盘字段契约**（对端接入指导核定，逐帧两键、不多不少）：

  ```json
  {"src":"order-svc","message":"2026-09-26T08:59:01 INFO order created id=42"}
  ```

  - **`src`**：服务归属标识，决定对端落盘**文件名**。缺省链 `HTTPConfig.Src` /
    `WithHTTPSrc` > `Config.Service`（Option 层 `Options.Service`）> `os.Hostname()`，
    handler 构建时一次性解析。对端落盘文件名字符集限 `[a-zA-Z0-9._-]`——**由库在构建期
    sanitize 兜底**：越界字节一律替换为 `-`、空值回退 `-`（对端 `:602` 实测：src 含 `/..`
    时应答 200 但落盘文件名被污染，故发送前强制收敛）；Init 层不校验该字段，**建议显式
    配置合规名**，不依赖 sanitize 的替换产物做服务名；
  - 对端接入指导的「发送前行校验 / 剔坏行 / 死信通道」条款对本库**天然满足**：帧每行必为
    `json.Marshal` 产物（两键、紧凑、单行），不存在自造坏帧路径；对端因任一行解码失败整批
    回 400 属确定性失败，已由本后端「4xx 不重试、立即丢弃 + 限流告警」短路覆盖，故本库
    不另行实现死信通道；
  - **`message`**：日志正文。对端落盘**只保留 message 文本**、其余结构化字段一律丢弃，
    因此时间/级别/msg/attrs 必须全部拼进 message 自身：本后端用 **text 版式渲染器**
    （与 console / 文件 `FormatText` 同一实现的裸值版式，含 time/level/msg/attrs 全量、
    单行、裸 `\n`/`\r` 已转义为字面量）渲染整条 record 得单行文本后，作为 message 字段
    做 JSON 字符串编码（src 同样经 JSON 转义防注入）。落盘后 message 含 `\n` 字面量两字符
    属预期（对端契约要求正文单行）。message 正文版式可经 `format: "json"` /
    `WithHTTPFormat(FormatJSON)` 切换为整条 JSON 记录串（落盘为一行 JSON 文本、可被对端
    再解析；信封与帧形不变，见「输出格式支持矩阵」）；
- **请求头**：`Content-Type: application/json`（服务端不强制，统一发送）；`Gzip` 开启时追加
  `Content-Encoding: gzip`。用户 `Headers` 在默认头**之后**逐条 `Set`，因此可覆写
  `Content-Type`（例如对端要求 `application/x-ndjson`）。
- **级别门槛**与 console / file / syslog 同一 `Leveler`。
- **响应**：`2xx` 视为成功（Vector 默认 `response_code: 200`），`4xx`/`5xx` 与网络错误同样判失败。

### 批量、重试与丢弃语义

| 触发 | 行为 |
| --- | --- |
| 当前批攒满 `BatchSize` 条 | 换出交给发送 worker，立即 `POST` |
| `FlushInterval` 到点且当前批非空 | 换出发送（不满批也发） |
| 换出时上一批仍在重试（inflight 槽占用） | **丢弃新换出的整批**（按条计 dropped）+ 限流告警 |
| 网络错误 / 5xx / **408 / 429** | 整批指数退避重试：100ms 起倍增至上限 2s，**含首次共 3 次尝试**（不可对外配置） |
| **其余 4xx**（400 / 404 / 413 …） | **确定性失败 → 不重试**：立即丢弃该批（按条计 dropped）+ 限流告警（文案含状态码，如 `http post 400 …`） |
| 重试次数用尽 | **丢弃该批**（按条计 dropped）+ 限流告警（同一原因每 5s 至多 1 条，与 syslog 同策略） |
| `Close`（含包级 `logger.Close` / 替换默认实例 / **`Fatal` 退出前**） | 停 ticker 与 worker；残余缓冲批最后发**一次**（快速尝试、不重试、限时 `Timeout`）；此后 `Handle` 静默丢弃 |

- **为什么 4xx 不重试**：对端 Vector 的 `http_server` + `codec: json` 对**任一坏帧即整批回 400**
  （解码失败发生在服务端解析阶段，与是哪一条无关），重试必然同败；把它计入 3 次重试只会
  拖慢丢弃、并占用 inflight 槽加剧后续批溢出。`408 Request Timeout` 与 `429 Too Many Requests`
  属暂时性状况（对端可能只是忙），仍走重试；
- **可靠性 = at-least-once**：重试发的是整批，若某次尝试已被对端接收但响应丢失/后续失败，
  **同一批会在对端重复落库**。本库不投递去重、不加事件 ID——需要 exactly-once 请在对端
  （Vector `dedupe` transform 或存储层主键）处理。
- **绝不 panic、绝不无限重试**：任何失败只影响该批，日志调用方永不因网络受阻（`Handle` 无 IO）。
- **`Close` 的等待上界**：常规单批总时限 = `Timeout` × 尝试次数 + 退避之和（默认上界约
  3×5s + 0.3s）；`Close` 只等 `2 × Timeout`，超时段内未落定的残留批按「丢弃 + 限流告警」处理，
  语义与 `AsyncHandler.Close` 的超时报告一致（此时不应再复用该实例）。
- **`Fatal` 保证限时 flush**：`logger.Fatal` / `Fatalf` 在记录后、`ExitFunc(1)` 之前，对当前默认实例
  执行**完整 sink 释放链**（异步队列 flush → 文件 → syslog → http 收尾投递，限时 2s），因此
  **未满批、未到 `FlushInterval` 的 Fatal 级日志也会尽力送达**——不会因为「投递在后台 worker」
  而随进程退出丢失。仍是尽力语义：2s 预算内未落定则按丢弃 + 限流告警处理；对端长期不可达时
  需要绝对送达请自行在退出前调用 `logger.Close(更大的 timeout)` 并检查返回错误。
- **批量默认值与 100MiB 上限**：Vector 对**解压后**的请求体有 100MiB 上限。默认 `BatchSize=100`
  条按常规日志体量（每条数百字节～数 KB）算，单请求约在 KB～百 KB 级，与上限相差 3~4 个数量级；
  调大 `BatchSize` 前请估算自身单条日志体积，避免触发对端 413。
- **连接**：`http.Client` 单例复用，`IdleConnTimeout` 取 100s（短于 Vector 默认 300s 连接回收），
  空闲连接由本端先淘汰；对端已关闭的连接 Go 传输层会自动重拨，无需应用层探活。
- **观测**：http 丢弃数不在 `logger.Stats()` 内（该方法只汇总异步队列），以 stderr 限流告警暴露。

### 与 Vector http_server source 对接

```yaml
sources:
  in:
    type: http_server
    address: 0.0.0.0:8686
    path: /v1/logs        # 必须与客户端 url 的路径部分一致（默认是 "/"）
    codec: json           # 逐行解码 NDJSON（与本项目后端输出的帧格式对应）
```

- **`codec: json` 的解码粒度**是「一行一个 JSON 对象」，正是本后端的分帧方式；解出帧后
  对端按上「落盘字段契约」取 `src` 定文件名、只落 `message` 文本；若对端配成
  `codec: text`，每行会连帧 JSON 一起变成正文，违反落盘行格式；
- **gzip**：对端需支持 `Content-Encoding: gzip`（Vector `http_server` 支持透明解压），
  不确定时把 `gzip` 设为 `false`；
- **失败反馈**：对端返回非 2xx（如写入下游失败、请求体超限 413）都会触发本后端的整批重试，
  因此请确保 Vector 的健康应答码路径畅通，避免长期不可用时以 3 次尝试/批的频率反复投递；
- **端点与网络可达性**（监听地址、容器网络与服务名解析等部署事实）以**对端接入指导为准**，
  本文不固定具体地址。

## 身份与链路字段前置（service / env / trace_id / req_id）

### trace_id / req_id 自动注入

handler 链内置 `TraceHandler`：所有 `*Context` 方法（`InfoContext` 等）执行时自动从
ctx 提取非空 `trace_id` / `req_id` 附加为日志属性，**业务代码无需手写**。

```go
// 中间件：把 trace_id / req_id 放进 context（包内类型化 key，无冲突）
func TraceMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        traceID := r.Header.Get("X-Trace-ID")
        if traceID == "" {
            traceID = generateTraceID()
        }
        ctx := logger.WithTraceID(r.Context(), traceID)
        ctx = logger.WithReqID(ctx, generateReqID())
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

// 业务代码：未注入 ctx 时不含这两个属性，注入后自动带出
func (s *UserService) GetUser(ctx context.Context, userID int64) {
    slog.InfoContext(ctx, "fetching user", slog.Int64("user_id", userID))
}

// 需要回读时
id := logger.GetTraceID(ctx)
```

### 自研 handler 的前置字段顺序（service → env → trace_id → req_id）

`service` / `env`（`WithService`/`WithEnv` 或 `With` 预设的 handler 级属性）与
`trace_id` / `req_id`（ctx 注入的 record 级属性）四个 key，在 console 与
file `FormatText` 两个 text 类通道中执行统一的「前置 + 去重」，固定相对次序为：

```
time → level → service(如有) → env(如有) → trace_id(如有) → req_id(如有) → msg → 其余 attrs → source(如有，行尾)
```

> **file `FormatText` 即 console 渲染器的 NoColor 形态（同一实现）**：前置段（`time` / `level` 及命中的前置字段）与
> `msg` 均为裸值——无 `key=` 前缀，`msg` 原样输出不加引号（前置字段值含空格时才加引号）；
> `msg` 中 `\n`/`\r` 以字面量转义（`\n`、`\r` 两字符形态）保持单行（防按 `req_id`/`trace_id` 过滤断行），不加引号；`\t` 等其余字符原样；
> `source=` 与其余 attrs 保持 k=v 风格，`source`（若启用）恒在行尾（其余 attrs 之后）。
> 唯一差异是控制台通道可叠加 ANSI 颜色（时间亮白、级别词按级染色、attr 的 `key=` 亮蓝），
> 关闭颜色后两通道输出字节等同（同一实现构造性成立）。例：
> `2026-09-24 10:12:33.456 INFO opencode-api prod fetching user user_id=42`。
> attr 值渲染先解析 `slog.LogValuer`（对齐标准库 handler）：`LogValue()` 返回 Group 时按 `key.sub=…`
> 递归展开、string 等形态按对应 Kind 处理（字符串走上述 quoting 规则）；未实现 LogValuer 的
> 普通 Any 值仍 `encoding.TextMarshaler` 优先、`%+v` 兜底；链式自引用由标准库深度保护退化为
> error 字符串，渲染层不 panic。Group 递归展开有 100 层深度上限，超限输出 `!DEPTH` 退化标记
> （保护同机内存，仅病理/程序化超深输入可达）。

缺失的 key 跳过，存在的保持上述相对次序。规则（四 key 各自独立判定）：

- 无论来自 record 级（如 `TraceHandler` 从 ctx 注入的 `trace_id`/`req_id`，或业务显式写入 record 的同名 attr）
  还是 `With(slog.String("service", ...))` 预设的 handler 级属性，都前置到 msg 之前，且全行只输出一次；
- 两源并存时以 **record 为准**，`With` 预设的同名值被去重剔除；但**值为空串视为未命中**
  （与 ctx 空值过滤一致）——record 存在某 key 但值为空串时，`With` 预设值仍会顶位前置；
- `With` 预设同名 key 多次出现时取**最后一次**出现的值；最后一次为空串时视为未命中，同名项均按普通属性输出（自洽退化）；
- `WithGroup` 派生后 `With` 的同名 key 带分组前缀，视为普通属性、不参与前置；
- 仅 `string` kind 命中（如 `slog.Int("env", ...)` 不前置，仍作普通属性输出）。

> **与 JSON 路径的分叉**：以上前置/去重仅作用于 console 渲染器（含文件 `FormatText` 的
> NoColor 形态，同一实现），
> 覆盖 `service`/`env`/`trace_id`/`req_id` 四 key；文件格式 `FormatJSON` 走标准
> `slog.NewJSONHandler`，保持标准字段语义，**不**套用该规则。

## 依赖注入模式（推荐）

```go
type UserService struct {
    log *slog.Logger
}

func NewUserService(base *slog.Logger) *UserService {
    // With 是不可变派生：返回新 logger，不影响 base 与其他协程
    return &UserService{log: base.With(slog.String("module", "user_service"))}
}

func (s *UserService) GetUser(ctx context.Context, id int64) {
    s.log.InfoContext(ctx, "fetching user", slog.Int64("user_id", id))
}
```

注意区分两类上下文数据的挂载方式：

- **静态字段**（模块名、组件名，以及全局 `service`/`env`）：初始化时用 `With` 绑定
- **请求级字段**（trace_id、req_id）：走 `logger.WithTraceID(ctx, ...)` + `InfoContext(ctx, ...)`，由 handler 自动注入，避免每请求构造临时 logger

> `With` 预设的 `service`/`env`/`trace_id`/`req_id`（原生 string）在 console 渲染器（文件 text 通道同实现）中
> 同样会被前置到 msg 之前（次序 `service → env → trace_id → req_id`，见上「身份与链路字段前置」）；
> 但请求级动态值仍推荐走 ctx——两源并存时以 record 值为准，`With` 预设值被去重剔除。
> `service`/`env` 用 `logger.WithService`/`WithEnv` 或 `Config.Service`/`Env` 配置，`New()` 内部即以常量 key 注入。

## 可选能力

```go
l := logger.New(
    // 错误堆栈：对 logger.Wrap 过的 error 自动附加 "<key>_stack" 属性
    logger.WithStackTrace(true),

    // 敏感信息打码：命中 key 的属性值替换为掩码（Group 递归）
    logger.WithSensitiveKeys("password", "token"), // 子串匹配，与内置词集合并
    logger.WithSensitiveMask("******"),            // 自定义掩码
    // Config 面可直接声明 Sensitive.Keys / Sensitive.Mask（映射同上「Config + Init」）
    // logger.WithSensitiveMatch(func(key string) bool { ... }), // 自定义匹配

    // 日志采样：窗口内前 10 条全留，之后每 100 条留 1 条
    logger.WithSampling(10, 100),

    // 异步写入：非阻塞入队，后台消费
    logger.WithAsync(2048),      // 队列满默认丢弃并计数
    logger.WithAsyncBlocking(),  // 可选：改为阻塞背压，绝不丢日志
)
```

错误堆栈与敏感打码配合使用：

```go
slog.Error("op failed", logger.Err(err)) // logger.Err: error 转 Attr（key 为 "error"）
err = logger.Wrap(err)                   // 记录创建位置调用栈
text := logger.SensitiveString(msg)      // 对消息文本按内置词集打码
```

## 运行时管理与退出

```go
logger.SetLevel(logger.Debug)        // 包级动态调级（对最近一次 New/Init 的实例即时生效）

lvl := logger.NewDynamicLevel(slog.LevelInfo)
l := logger.New(logger.WithLeveler(lvl))
lvl.Set(slog.LevelWarn)              // 自定义 Leveler：级别控制权完全在调用方，
                                     // 包级 SetLevel 对该实例无效

total, dropped := logger.Stats()     // 异步累计统计（total / 因队列满丢弃数）
logger.Fatal("unrecoverable", "component", "broker") // 记录 → 完整 sink 释放链（限时 2s，含 http 收尾投递）→ ExitFunc(1)
logger.Close(2 * time.Second)        // 进程退出前统一 flush + 关文件 + 关 syslog 连接 + 停 http worker
```

`Close` 返回非 nil 错误表示某实例的异步队列**未在 timeout 内排空**（错误信息含残余条数）。
此时该实例的后台消费 goroutine 仍在写入，且文件 writer「写时重开」可能产生永不关闭的残余句柄——
调用方应视该实例存在残余、不再复用其日志能力，只能假定进程即将退出；
需要确定性落盘的场景请加大 timeout 或改用同步 writer（不启用 `WithAsync`）。
`Fatal` / `Fatalf` 退出前走的是**同一个** `close` 释放链（预算 2s、幂等），故异步队列、文件、
syslog 与 http 缓冲批都会在退出前尽力送出；其超时残余同样只以告警暴露（进程即将退出，错误不上抛）。
syslog sink 的 `Close` 关闭底层连接并置为已关闭，此后**不再重连写出**（幂等，与文件句柄释放同链）；
http sink 的 `Close` 停止发送 worker、把残余缓冲批最后发一次（快速尝试、不重试）后关闭空闲连接，
此后 `Handle` 静默丢弃（幂等；其内部等待上界为 `2 × Timeout`，排在异步 flush 与文件/syslog 释放之后）；
`Stats()` 仅统计异步队列丢弃，**不含** syslog / http 的发送失败丢弃（后者以 stderr 限流告警暴露，语义不同）。
另注意：按日期轮换（`WithDateRotate`）的跨日压缩/清理在后台执行，其 `Close` 等待任务收敛
**没有自有超时**——病态文件系统（如 NFS 挂起）下可能超出 `logger.Close(timeout)` 的预算
（见 `rotate.RotateDateWriter.Close` 文档）。

> **引用失效契约**：每次 `New` / `Init` 关闭上一默认实例，先前捕获的一切 `*slog.Logger`
> 引用（含 `logger.DefaultLogger` 旧值）自此陈旧且已关闭——异步链静默丢日志、文件链借
> 「写时重开」复活产生残余句柄。长生命周期组件（内部持有 logger 引用的 cache、监控器等）
> 不应跨重建持有引用：应用运行期重建 logger 后，须重新读取 `logger.DefaultLogger`
> 或重新构造组件。

级别常量：`logger.Trace / Debug / Info / Warn / Error / FatalLevel`（`Level = slog.Level` 别名）。
`logger.ExitFunc` 在测试中可替换，防止 `Fatal` 真实退出进程。

## 从旧版迁移

| 旧 API（已删除） | 新写法 |
| :--- | :--- |
| `l.Info("a", "b")`（拼接为消息） | `slog.Info("a", "b", "c", v)`（msg + 键值对属性） |
| `l.Infof("x=%v", x)` | `slog.Info(fmt.Sprintf("x=%v", x))` |
| `l.WithField(k, v)` / `WithFields(map)` | `l.With(k, v)`（成对参数） |
| `l.SetLevel(lvl)`（实例方法） | `logger.SetLevel(lvl)`（包级）或 `WithLeveler` |
| `l.SetOutput(w)` | 重新 `logger.New(logger.WithConsole(logger.WithConsoleWriter(w)))` |
| `logger.WithLogger(ctx, l)` / `FromContext` | `logger.WithTraceID(ctx, id)` + `InfoContext(ctx, ...)` |
| 常量 `logger.Fatal` | 常量 `logger.FatalLevel`；函数 `logger.Fatal(msg, args...)` |
| `logger.LogRecorder` / `WithRecorder`（logrus 插件） | 已整体移除，统一走 `slog.Handler` 生态 |

### v0.3.0 → v0.4.0（sink 分组，破坏性）

独立发布 module `logger/v0.4.0` 的破坏性重构：删除顶层 sink Option、按输出目标分组，**不保留** deprecated 兼容层。

| 旧 API（已删除） | 新写法 |
| :--- | :--- |
| `logger.WithOutput(w)` | `logger.WithConsole(logger.WithConsoleWriter(w))` |
| `logger.WithColor(b)` | `logger.WithConsole(logger.WithConsoleColor(b))` |
| `logger.WithFileFormat("text")` | `logger.WithFile(path, logger.WithFormat(logger.FormatText))` |

- **sink 按存在性开关**：`WithConsole` 启用控制台、`WithFile` 启用文件；二者皆不声明时兜底一个 stdout 控制台（`logger.New()` 零配置即用、避免包级日志静默），仅 `WithFile` 则纯文件、不写 stdout。
- **文件格式改枚举** `FileFormat`（`FormatJSON` 默认 / `FormatText`），`WithFormat` 收 typed 值而非裸 string；`Config.FileFormat` 的 yaml 字符串（`"json"`/`"text"`，大小写不敏感）仍向后兼容，非法非空值经 `Init` 报错而非静默回退。
- `Config` 字段名与 `yaml`/`mapstructure` tag（含 `output`/`file_format`）**保持不变**，仅内部映射为分组 Option；`Init` 新增变参 `Init(cfg, opts...)`（`Init(cfg)` 源码兼容）。（v0.5.0 起 Config 层级化，平铺 tag 键随结构调整为层级 snake_case，旧键不再被识别，见下节。）

### v0.4.0 → v0.5.0（Config 层级化多后端，破坏性）

`Config` 由平铺结构改为层级化多后端结构（Option 层 API 不变），**不保留**旧字段兼容层；
字段继续携带 `yaml` / `json` / `mapstructure` tag（键名统一 snake_case，层级嵌套由子结构自然承载），
支持应用端直接嵌入（如 `Logger: logger.Config`）后解码；logger 库本身仍不含任何解析代码，
`Init` 只接收装配好的结构体：

| 旧 Config 字段（已删除） | 新 Go 写法 | 新 tag 键（yaml/mapstructure） |
| :--- | :--- | :--- |
| `Output: "console"` | `Outputs.Console: &ConsoleConfig{}`（指针非 nil 即启用） | `outputs.console` 节存在 |
| `Output: "file"` | `Outputs.File: &FileConfig{…}`（仅 File 节点） | `outputs.file` 节存在 |
| `Output: "both"` | `Outputs.Console` 与 `Outputs.File` 两节点同时非 nil | 两节同时存在 |
| `NoColor` | `Outputs.Console.NoColor` | `outputs.console.no_color` |
| `File` | `Outputs.File.Filename` | `outputs.file.filename` |
| `FileFormat` | `Outputs.File.Format` | `outputs.file.format` |
| `MaxSize` / `MaxAge` / `MaxBackups` / `Compress` / `Layout` | 迁入 `Outputs.File.*` | `outputs.file.max_size` / `max_age` / `max_backups` / `compress` / `layout` |
| `Sensitive_Keys` / `Sensitive_Mask` | `Sensitive.Keys` / `Sensitive.Mask` | `sensitive.keys` / `sensitive.mask` |

- **后端按节点存在性启用**：`Outputs.Console` / `Outputs.File` 某节点非 nil = 启用该后端，nil = 不启用；两节点皆 nil 由 `New` 兜底 stdout 控制台。未来新增 syslog/http 后端在 `OutputsConfig` 加指针字段即可。
- `DefaultConfig()` 返回纯 console（`Outputs.Console = &ConsoleConfig{}`、`Outputs.File = nil`），等价旧 `Output: "console"`；文件默认参数（MaxSize 100 / MaxAge 30 / MaxBackups 10 / Compress true / Format "json"）见 `DefaultConfig` 文档中的 `FileConfig` 模板。
- 黑洞校验措辞随语义更新：`Outputs.File` 节点非 nil 但合并后 `Filename` 为空 → 报错。

## 设计说明

- **sink 装配**：控制台与文件为两路独立 sink，按存在性装配——`WithConsole`（彩色文本）与/或 `WithFile`（`FormatJSON` 默认 / `FormatText` 即 console 渲染器的 NoColor 形态、同一实现：`time → level → service → env → trace_id → req_id → msg → 其余 attrs → source（若启用，行尾）`，行版式为前置段裸值、msg 原样不加引号（`\n`/`\r` 以字面量转义保持单行，`\t` 等其余字符原样）、source 与其余 attrs 保持 k=v；其中四个前置字段 `service`/`env`/`trace_id`/`req_id` 由 record 与 `With` 累积属性双源前置并按 record 优先去重，见「身份与链路字段前置」；`FormatJSON` 走标准 `slog.NewJSONHandler`，不套用该排序与双源规则）；二者并存时以 `MultiHandler` 汇聚，皆未声明则兜底 stdout 控制台（`New()` 零配置不静默），仅 `WithFile` 则不写 stdout
- **远端 sink**：`WithSyslog`（逐条 RFC5424、newline 分帧、懒 dial + 后台重连）与 `WithHTTP`（批量 NDJSON POST、单 worker 发送 + 指数退避重试）在 `New` 时各构建一次并注册到 Close 链，与本地 sink 并存时同样进 `MultiHandler` 扇出；两者的失败都是「丢弃 + stderr 限流告警」，不阻塞、不报错给日志调用方
- handler 链（内 → 外）：`(console 与/或 file)` → `StackHandler` → `SensitiveHandler` → `SamplingHandler` → `AsyncHandler` → `TraceHandler`（内置，始终位于最外层）；可选项未启用时不参与链
- `TraceHandler` 置于最外层：`trace_id`/`req_id` 在**调用方 goroutine 内同步提取进 record**后才进入采样 / 异步队列，因此异步队列 entry 无需（也不应）持有请求级 `context.Context`；异步模式下 trace 提取同样生效，且避免了长命队列持有可取消 ctx 的反模式
- 默认（零 sink）输出为 **stdout 控制台**（见上「sink 装配」）；`WithAsync` 队列容量默认 10240（与引擎一致）
- `With` 派生共享底层设施（writer/异步队列/文件句柄），均为并发安全共享；派生实例与父实例的属性互不影响
- **连续 `New` / `Init` 语义**：每次 `New` 在把新实例登记为「默认实例」前，会 `close` 上一个默认实例（flush 其异步队列、关闭文件句柄、从注册表注销），避免队列 / 句柄累积；首次创建（无旧实例）跳过。`SetLevel` / `Fatal` 读取的默认实例引用与 `New` / `Init` 的写入由 `defaultMu` 互斥；`Fatal` 退出前对默认实例执行的是同一条 `close` 释放链（限时 2s、`closeOnce` 幂等），与 `New` 替换旧实例时的 `prev.close(2s)` 同一实现，`close` 内部不取 `defaultMu`（只取实例 `l.mu` 与注册表 `loggerMu`），故在 `defaultMu` 之外调用无自锁风险
