# 林霖动环数据模拟器

林霖动环数据模拟器是一个基于 TCP 的轻量设备数据模拟服务。程序从本地 SQLite 快照 `simulator.db` 一次性加载端口、命令和响应数据，在本机或指定网卡地址上监听 TCP 端口，用于联调、演示和离线测试动环采集链路。

## 环境要求

- Go 1.21+
- 纯 Go 编译，零 CGO 依赖
- Windows PowerShell 可用于发布构建脚本

## 快速开始

```bash
go build -o lin_simulator .
./lin_simulator
```

仓库自带 `simulator.db` 演示快照，默认从当前目录读取。默认监听 `0.0.0.0`，因此本机 `127.0.0.1` 和本机网卡 IP 都可以访问。传入 `-ip` 时，该 IP 必须是本机已启用网卡上的合法 IPv4 地址，程序将只监听这个 IP。

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-db` | `simulator.db` | SQLite 数据库文件路径 |
| `-delay` | `50` | 全局发送延迟（毫秒）；数据库端口级 `ports.delay > 0` 时优先 |
| `-debug` | `false` | 启用调试日志 |
| `-max-conn` | `5` | 每个端口最大并发连接数，`0` 表示不限制 |
| `-ip` | 空 | 手动指定本机 IP 并仅监听该 IP；为空则监听 `0.0.0.0` |
| `-h` | - | 显示帮助信息 |

示例：

```bash
# 默认配置启动
./lin_simulator

# 自定义延迟和调试模式
./lin_simulator -delay 100 -debug

# 每个端口最多 20 个并发连接
./lin_simulator -max-conn 20

# 仅监听指定本机 IP
./lin_simulator -db /data/simulator.db -ip 192.168.1.100
```

## 工作原理

```text
simulator.db -> 内存 PortMap -> TCP 监听端口 -> 客户端
```

1. 启动时读取 SQLite 中的 `ports` 和 `commands`。
2. 加载阶段校验端口延迟、命令编码、返回值编码和命令长度。
3. 运行阶段按 TCP 字节流缓冲匹配命令，支持拆包、粘包和垃圾字节后的自动重同步。
4. 同一命令存在多条返回值时，每个 TCP 连接独立轮转返回。

### 命令匹配流程

```text
客户端发送原始字节
    |
    v
连接缓冲区累积字节
    |
    v
按已知命令做最长前缀匹配
    |
    +-- 匹配 -> 按轮转顺序选取预解码返回值 -> 延迟发送
    |
    +-- 仍是某个命令前缀 -> 等待更多字节
    |
    +-- 无法形成任何命令前缀 -> 丢弃 1 字节并继续同步
```

## 项目结构

```text
lin_simulator/
|-- build.ps1            # Windows 发布构建脚本，输出多平台主程序和 simulator.db
|-- main.go              # 入口：参数解析、日志初始化、数据库加载、启动服务
|-- database.go          # SQLite 数据加载
|-- database_test.go     # 数据加载和数据校验测试
|-- model.go             # 数据结构
|-- server.go            # TCP 服务器
|-- server_test.go       # TCP 流匹配、连接保护和重试测试
|-- utils.go             # 工具函数
|-- utils_test.go        # IP 选择和绑定校验测试
|-- docs/
|   |-- requirements.md  # 需求文档
|   |-- design.md        # 设计文档
|   `-- releases/        # 发布说明
|-- scripts/check.ps1    # 统一测试、构建和扫描脚本
|-- go.mod               # 主程序依赖
`-- simulator.db         # SQLite 演示快照
```

## 数据库表结构

`simulator.db` 包含两张业务表：

**ports** - 端口配置

| 列 | 类型 | 说明 |
|----|------|------|
| `port_id` | INTEGER | 端口 ID（主键） |
| `ip` | TEXT | 演示快照来源字段；运行时不用于筛选，公开快照统一为 `127.0.0.1` |
| `port` | INTEGER | TCP 端口号；运行时只加载 `1..65535` |
| `delay` | INTEGER | 端口级延迟（毫秒） |

**commands** - 预编码命令和返回值映射

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | INTEGER | 自增主键 |
| `port_id` | INTEGER | 关联端口 |
| `command_key` | TEXT | 大写十六进制命令 |
| `return_value` | TEXT | 大写十六进制返回值 |
| `seq` | INTEGER | 同命令返回值序号 |

## 运维

### 发布构建

```powershell
.\build.ps1
```

脚本在 Windows 下执行，默认交叉编译 8 个平台主程序，并把产物和 `simulator.db` 直接输出到 `C:\source\01_project\linlin_pub\dist\`。可通过 `-DistDir` 覆盖输出目录。

### 本地检查

```powershell
.\scripts\check.ps1
```

脚本会执行 `go test`、`go vet`、`go build` 和 `govulncheck`。它不会导入、生成或改写 `simulator.db`。如需跳过漏洞扫描：

```powershell
.\scripts\check.ps1 -SkipVulnCheck
```

### 信号处理

- `SIGINT` / `SIGTERM`：关闭所有 TCP 监听器和活动连接，等待处理协程退出后结束。

### 端口故障恢复

端口监听失败时自动进入指数退避重试：1s -> 2s -> 4s -> 8s -> 16s -> 32s -> 60s（封顶），成功后恢复正常。

### 连接保护

每个端口默认最多 5 个并发连接，可通过 `-max-conn` 调整。连接读超时为 5 分钟，响应写入超时为 10 秒，连接命令缓冲区上限为 64KB。

## 依赖

主程序仅依赖 `modernc.org/sqlite` 及 Go 标准库，SQLite 驱动为纯 Go 实现。
