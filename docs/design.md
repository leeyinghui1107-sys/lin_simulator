# 林霖动环数据模拟器设计文档

## 1. 当前架构

```text
simulator.db
  -> database.go 加载端口和命令
     -> server.go 监听 TCP 并处理连接
```

主程序只依赖本地 SQLite。启动加载完成后会关闭数据库连接，设备模拟运行时不访问数据库。

## 2. 模块职责

| 文件 | 职责 |
|------|------|
| `main.go` | 参数解析、日志初始化、绑定 IP 校验、数据库打开、启动服务 |
| `database.go` | 从 SQLite 加载有效端口和命令，校验并预解码命令/返回值，构建内存 `PortMap` |
| `model.go` | `PortInfo` 状态锁、重试计数、连接计数、命令响应数据和字节 Trie 匹配索引 |
| `server.go` | TCP 监听、指数退避重试、连接限制、连接处理和优雅关闭 |
| `utils.go` | 本机 IP 自动识别、`-ip` 校验、字节转十六进制 |
| `build.ps1` | PowerShell 多平台发布构建脚本 |

测试覆盖分布：`database_test.go` 覆盖 SQLite 加载和数据校验，`server_test.go` 覆盖 TCP 流匹配、连接保护和重试状态，`utils_test.go` 覆盖本机 IP 选择和绑定校验。

## 3. 启动流程

1. 解析命令行参数。
2. 初始化 `slog` 日志。
3. 校验 `-delay >= 0`、`-max-conn >= 0`。
4. 计算监听地址：
   - 未指定 `-ip`：监听 `0.0.0.0`，日志中尽量展示自动识别的主网卡 IP。
   - 指定 `-ip`：解析为 IPv4，并确认该 IP 属于本机已启用网卡；通过后只监听该 IP。
5. 校验 SQLite 文件存在并打开。
6. `LoadPortMap(db, bindHost)` 加载端口和命令，并过滤没有命令的端口。
7. 关闭数据库连接，释放文件句柄；后续运行只使用内存数据。
8. `signal.NotifyContext` 接管 `SIGINT` / `SIGTERM`。
9. `Start(ctx, pm, delay, maxConn)` 启动所有端口。

## 4. 数据库加载

SQLite 表结构：

```sql
CREATE TABLE ports (
    port_id INTEGER PRIMARY KEY,
    ip      TEXT    NOT NULL,
    port    INTEGER NOT NULL,
    delay   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE commands (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    port_id      INTEGER NOT NULL,
    command_key  TEXT    NOT NULL,
    return_value TEXT    NOT NULL,
    seq          INTEGER NOT NULL DEFAULT 0
);
```

运行时端口查询：

```sql
SELECT port_id, port, delay
FROM ports
WHERE port BETWEEN 1 AND 65535
ORDER BY port, port_id;
```

命令查询：

```sql
SELECT p.port, c.command_key, c.return_value
FROM commands c
JOIN ports p ON c.port_id = p.port_id
WHERE p.port BETWEEN 1 AND 65535
ORDER BY p.port, c.command_key, c.port_id, c.seq;
```

设计要点：

- `ports.ip` 不参与筛选或监听绑定；公开演示快照中统一为 `127.0.0.1`。
- 同一端口号出现多条记录时，只创建一个监听器，并合并这些记录下的命令；启动时对重复端口输出警告摘要。
- 同一端口重复记录的 `delay` 使用排序后的第一条端口记录。
- `command_key` 和 `return_value` 会先去除首尾空白，再按十六进制解码；大小写均可，且都不能为空。
- `delay < 0`、空命令/返回值、非法十六进制命令/返回值、超过 64KB 的命令都会导致启动加载失败。
- `port <= 0`、`port > 65535` 或没有任何命令的端口不会被监听。

## 5. TCP 服务

`Start(ctx, pm, globalDelay, maxConn)` 负责启动和重试端口监听：

- 首次并发启动所有端口。
- 监听失败或监听异常退出时，将端口标记为错误。
- 重试退避为 `1s -> 2s -> 4s -> 8s -> 16s -> 32s -> 60s`，之后保持 60s。
- 每秒检查一次需要重试的端口。
- 每分钟输出一次各端口聚合拒绝连接数。
- 收到关闭信号后关闭监听器和活动连接，并等待处理协程退出。
- 每个端口用自身状态锁保护 `Status`、`failCount` 和 `nextRetry`，避免全局端口锁造成跨端口阻塞。

`handleConn` 行为：

- 多返回值命令在每个连接内按需维护独立轮转索引；单返回值命令不分配轮转 map。
- 每次读取后重置 5 分钟读超时。
- 读取的是 TCP 字节流，连接内维护缓冲区；沿字节 Trie 做最长前缀匹配，支持拆包/粘包。
- 当前缓冲仍是某个命令前缀时继续等待；无法形成任何命令前缀时丢弃 1 字节并继续同步，Debug 日志记录。
- 连接命令缓冲区小容量起步，按需增长，消费后复用底层空间，上限为 64KB；超过上限时关闭该连接。
- 返回值在启动阶段预解码为字节；响应写入前设置 10 秒写超时。
- 端口级延迟大于 0 时优先，否则使用全局延迟。

## 6. IP 选择和校验

默认启动不需要指定 IP，服务监听 `0.0.0.0`。这允许本机 `127.0.0.1` 和本机网卡 IP 同时访问。

自动识别本机 IP 仅用于日志展示，会跳过回环地址、链路本地地址、未启用接口和常见虚拟网卡。显式 `-ip` 校验只要求它是合法 IPv4，并且存在于本机已启用网卡地址中。

## 7. 发布构建

`build.ps1` 默认交叉编译 8 个平台裸二进制到项目根目录 `dist/`，并复制 `simulator.db`：

- `windows/amd64`
- `windows/arm64`
- `linux/amd64`
- `linux/arm64`
- `linux/arm GOARM=6`
- `linux/arm GOARM=7`
- `darwin/amd64`
- `darwin/arm64`

可通过 `-DistDir` 指定输出目录，通过 `-Clean` 清理旧发布产物。

## 8. 当前依赖

- `modernc.org/sqlite`
- Go 标准库
