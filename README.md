# dsh-plugin-piko-remote

把本机端口通过 [piko](https://github.com/andydunstall/piko) 隧道暴露到远程服务器，
并把公网访问地址交还给用户和模型。调用方式与
[opencode-piko-remote](https://github.com/friddle/opencode-piko-remote) 完全一致：
每个 endpoint 一条**只出站**的上游连接，前面挂一个反向代理指向本地目标。

它是 [gotty-piko](https://github.com/friddle/gotty-piko) 服务端的插件侧对偶——
服务端负责按 endpoint 路由，插件负责把本地端口接上去。

```text
本机                                    公网
┌──────────────────────────────┐
│ dsh (--profile web)          │
│  └─ dsh-plugin-piko-remote   │        https://dsh-k3f9qz.clauded.friddle.me/
│       └─ piko-expose (Go) ───┼──WS──►  gotty-piko server ──► 浏览器
│            └─ 反代 ──► 127.0.0.1:<port>
└──────────────────────────────┘
```

## 状态

可用。模型工具、子进程管理、隧道与鉴权均已实现，并通过真实公网链路验证：

- `piko-expose`（`go/`）单跑即可连公共服务器，端到端冒烟脚本 3/3 通过。
- 插件半侧（`lib/`）48 个单测 + Go 13 个单测通过。
- 已在远程 Linux 主机上以 `dsh` + 本插件跑通 DSH Web 的公网访问：
  SPA `200`、`/assets/*.js` `200`、`/api/remote.mux`（WebSocket）`101`。

尚未做（见 [PLAN.md](./PLAN.md) Phase 6）：客户端设置卡片、settings 命名空间、
release 时交叉编译并随包发布 helper。

## 安装

```bash
dsh plugin add github:friddle/dsh-plugin-piko-remote
# 或本地开发
dsh plugin add /absolute/path/to/dsh-plugin-piko-remote
```

`dsh plugin add` 会在 profile 目录跑 pnpm，然后按「已安装包是否声明 `dsh.bundle`」
把包写进 `dsh.profile.bundles`。**改完插件记得重启 DSH。**

### helper 二进制

隧道引擎是 Go 写的，**不随 git 仓库分发**（`bin/` 被 git 忽略）。安装后需要：

```bash
cd <插件目录>
scripts/build-helper.sh          # 需要 Go 工具链
```

脚本按 `bin/piko-expose-<os>-<arch>` 命名交叉编译。若二进制缺失，插件仍会加载，
但会在日志里报出它查过的路径；此时 `remote_expose` 会给出同样的修复指引。
也可以把 `helperPath` 配置指向任意已有的 `piko-expose`。

## 使用

模型侧三个工具：

| 工具 | 参数 | 作用 |
|---|---|---|
| `remote_expose` | `port?` `name?` `ttlMinutes?` `auth?` | 暴露端口，返回 `remoteUrl` 与随机账号密码 |
| `remote_status` | — | 列出当前隧道、地址、状态、过期时间 |
| `remote_close` | `endpoint?` | 关闭一条或全部隧道 |

不给 `port` 时默认暴露**当前 DSH Web 的端口**；这需要显式打开
`allowDshUiExpose`（默认关，原因见下）。

### 配置

配置写在 profile 的 `cordis.patch.yml` 里，按 row id 定向：

```yaml
# ~/.dsh/profiles/web/cordis.patch.yml
- id: piko-remote
  config:
    remote: https://clauded.friddle.me
    endpointPrefix: dsh
    basicAuth: true
    urlMode: subdomain
    preserveHost: false        # 让服务端把 Host/Origin/Referer 改写成回环地址
    allowDshUiExpose: true     # 允许暴露 DSH 自己的界面
    autoExpose: true           # 插件加载时自动暴露 DSH Web 端口
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `remote` | `https://clauded.friddle.me` | piko 服务器地址 |
| `upstreamKey` | 空 | piko 上游鉴权 key（公共服务器不需要） |
| `endpointPrefix` | `dsh` | 随机 endpoint 前缀，如 `dsh-k3f9qz` |
| `defaultTtlMinutes` | `120` | 默认存活分钟数，`0` = 不过期 |
| `basicAuth` | `true` | 默认给隧道加 Basic Auth（账号密码随机生成） |
| `urlMode` | `subdomain` | `subdomain` = `https://<endpoint>.<base>/`；`path` = 路径前缀模式 |
| `preserveHost` | `true` | 是否把浏览器 Host 原样转给本地服务；`false` 会连同 Origin/Referer 一起改写成回环地址 |
| `allowDshUiExpose` | `false` | 是否允许暴露 DSH 自己的 Web 界面 |
| `autoExpose` | `false` | 加载时自动暴露 DSH Web 端口（仍需 `allowDshUiExpose`） |
| `helperPath` | 空 | 指定 `piko-expose` 路径 |
| `connectTimeoutMs` | `20000` | 等待 helper `ready` 的超时 |

### 为什么 DSH Web 必须用子域名模式

DSH 客户端把 API base 解析成 `location.origin`
（`@deepseek-ai/dsh-client-connection/lib/client.js`），在
`https://host/dsh-xxx/` 下打开页面后，浏览器会去请求 `https://host/api/...`——
路径第一段变成另一个 endpoint，永远打不到隧道。子域名模式下整个 origin 归该
endpoint，路径原样透传，`/api/...`、`/assets/...` 都正常。

两种过 DSH `/api` 信任围栏的配置：

| 方式 | 配置 | 代价 |
|---|---|---|
| 保留 Host | `preserveHost: true` + `dsh --profile web --trusted-host <endpoint>.<base>` | `--trusted-host` 不支持通配符，endpoint 必须固定 |
| 本地化 Host | `preserveHost: false` | 上游以为自己在 `127.0.0.1`，依赖 Host 的绝对跳转/cookie 可能不符合预期 |

## 部署到远程主机

在无图形界面的 Linux 主机上装 DSH、装本插件、把 Web 界面暴露到公网，完整命令见
[docs/remote-deploy.md](./docs/remote-deploy.md)。三个要点：

- helper 用 `npm pack` 出来的 **tarball** 安装：用目录安装会变成 `link:`，Node 会从插件的
  真实路径解析裸 import，`schemastery` / `dsh-tools` 全都找不到；
- `@deepseek-ai/dsh-tools` 是 peerDependency，而 profile 默认 `autoInstallPeers: false`，
  需要单独 `dsh plugin add @deepseek-ai/dsh-tools@<版本>`；
- DSH Web 自己还有一道 `?token=` 门（每次启动都变），所以完整地址是
  `https://<endpoint>.<base>/?token=…`，外面再套一层本插件的 Basic Auth。
  自动暴露（`autoExpose`）时用 `credentialsFile` 把地址与随机账号密码以 0600 权限落盘。

## 验证

```bash
npm test                  # 44 个插件单测
(cd go && go test ./...)  # 13 个 helper 单测
scripts/smoke-test.sh     # 真实公网链路：起目标服务 → 连 piko → 校验 401/200/路径透传
```

`smoke-test.sh` 会打印一条真实可访问的 URL 和随机账号密码，结束后自动清理。

## 安全

暴露 DSH 自己的 Web 界面等于**把本机 Agent 的控制权交给任何拿到该 URL 的人**。
因此：

- Basic Auth 默认开，账号密码随机生成（20 位，去掉易混字符）；
- TTL 默认非空（120 分钟）；
- `allowDshUiExpose` 默认关，`remote_expose` 在未打开时会拒绝暴露 DSH 端口；
- 插件日志只记录 endpoint，不记录完整 URL 和账号密码。

## License

MIT
