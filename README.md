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
- 插件半侧（`lib/`）48 个单测 + Go 单测（helper 13 + 启动器 26）通过。
- 一键启动器 `dsh-piko-remote` 已在**空环境**远程主机实测：自动装 Node + dsh +
  插件并给出可用公网地址。
- 已在远程 Linux 主机上以 `dsh` + 本插件跑通 DSH Web 的公网访问：
  SPA `200`、`/assets/*.js` `200`、`/api/remote.mux`（WebSocket）`101`。

尚未做（见 [PLAN.md](./PLAN.md) Phase 6）：客户端设置卡片、settings 命名空间、
release 时交叉编译并随包发布 helper。

## 一键启动（推荐）

仓库里第二个 Go 命令 `dsh-piko-remote` 负责把整套东西装好并跑起来，
用法与 opencode 那边的 `opencode-piko-remote` 对齐：

```bash
dsh-piko-remote up                                  # 装 Node/dsh/插件 → 启动 → 打印公网地址与随机账号密码
dsh-piko-remote up --plugin someone/their-plugin     # 插件地址可指定，owner/repo 自动指向 GitHub
dsh-piko-remote status | logs | down
```

它会自动：准备满足 DSH `engines` 的 Node（不够就自己下一份，不碰系统 Node）→
装 `@deepseek-ai/dsh` 与 `pnpm` → 从 web 模板建一个专用 profile → 装插件 →
补齐插件缺的 peerDependency → 把内嵌的 `piko-expose` 补进插件的 `bin/` →
生成 `--patch` overlay（不改你自己的 `cordis.patch.yml`）→ `setsid` 后台启动 →
等就绪后打印本地地址、公网地址、随机账号密码。

`--plugin` 支持 `owner/repo`（→ `github:owner/repo`）、`@scope/pkg@ver`、本地目录、
`.tgz`、URL，可重复。完整参数与设计说明见 [docs/launcher.md](./docs/launcher.md)。

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

## 沙箱与权限：命令到底怎么跑

DSH 把「谁能改文件」拆成三层，插件和启动器都不改写其中任何一层，只提供开关：

| 层 | 位置 | 作用 |
|---|---|---|
| 执行器 | 宿主组合里的 `bash-sandbox` 行（`@deepseek-ai/dsh-bash-sandbox`，注册为 `ctx.shell`） | 每条命令都过一遍沙箱包装；只有这一个执行器，没有「无沙箱执行器」可选 |
| 策略模式 | `sandbox-policy` 行的 `mode`，取自环境变量 `DSH_PERMISSION_MODE`（默认 `workspace-write`） | 新会话的默认模式，决定包装成只读 / 只写工作区 / 不限制 |
| 会话预设 | `permission` 行的 presets，界面上是权限选择器 | 每个会话可单独选 `read-only` / `workspace-write` / `danger-full-access` |

关键点：**`danger-full-access` 就是「不走沙箱」**。
`dsh-bash-sandbox` 在该模式下直接 `super.start(spec)` 交给本地执行器，不加任何包装
（`lib/index.js` 里 `if (mode === "danger-full-access") return super.start(spec)`），
同时 `approval` 行的 policy 也从 `ask` 变成 `never`，不再弹审批。

因此「很多时候不想让它走沙箱」有三种写法，粒度和持久性递增：

```bash
# 1. 单个会话：界面右下角的权限选择器切到「完全权限」
# 2. 这台机器上的新会话默认不走沙箱：
dsh-piko-remote up --no-sandbox          # = --env DSH_PERMISSION_MODE=danger-full-access
# 3. 自己显式指定：
dsh-piko-remote up --env DSH_PERMISSION_MODE=danger-full-access
```

`--no-sandbox` 只改默认模式（沿用宿主组合的沙箱执行器），**不是**把执行器换成
`@deepseek-ai/dsh-bash-local`：那样组合直接起不来——`permission` 预设插件会拒绝一个
没有 `sandboxMode` 的执行器（`the mounted bash executor does not confine (no
sandboxMode)`），而 `fs-sandbox`、`api-workspace-files`、deliverables 界面都注入
`sandboxPolicy`，删掉策略行会留下一堆 pending 条目。

### 宿主需要什么才能真的沙箱

Linux 上 DSH 依次探测两条链（`@deepseek-ai/dsh-sandbox-local`）：

| 链 | 探测命令 | 失败时看到 |
|---|---|---|
| `bwrap` | `bwrap --ro-bind / / --dev /dev --unshare-pid --proc /proc --die-with-parent -- true` | `bwrap: Can't mount proc on /newroot/proc: Operation not permitted` |
| `landlock` | 预编译的 `landlock-run` | `/sys/kernel/security/lsm` 里没有 `landlock` |

两条都不可用时，`workspace-write` 会话的每条命令都会被拒绝
（`No sandbox backend is usable on this host`）并触发一次升权审批——这不是插件的问题，
而是宿主内核/容器不给能力：

- `bwrap` 需要能建 PID namespace 并挂载新的 `/proc`；LXC 里通常被
  `lxc.mount.auto` / apparmor / userns 配置挡住（去掉 `--unshare-pid --proc` 能跑，
  但那不是 DSH 用的 profile）；
- `landlock` 需要在物理机的内核命令行里启用（Proxmox 默认只开 apparmor），
  并且该 LSM 必须在容器的 `/sys/kernel/security/lsm` 里出现。

容器里两样都拿不到时，有两条路：

**1）用沙箱适配器，保住文件沙箱**（推荐）：

```bash
dsh-piko-remote up --sandbox-runner bwrap-noproc    # 或 auto：先探测再决定
```

DSH 的 `sandbox` 行接受操作者提供的 `runnerCommand`，并且会把**和 bwrap 完全相同的
profile 参数**交给它（`@deepseek-ai/dsh-sandbox-local` 的 `confine()`）。启动器因此可以自己
当这个 runner：把 `--unshare-pid` 和 `--proc /proc` 过滤掉，其余原样 `exec bwrap`。
实测（本机 LXC）：

```
$ bwrap --ro-bind / / --dev /dev --tmpfs /tmp --bind <ws> <ws> --die-with-parent -- sh -c 'echo ok > <ws>/x'
ok
$ bwrap ... -- sh -c 'echo nope > /etc/probe'
sh: cannot create /etc/probe: Read-only file system
```

`--ro-bind / /` 这条文件策略没变，所以「工作区外不可写」依然成立，DSH 也能把
`Read-only file system` 正确识别成沙箱拒绝（它给自定义 runner 的拒绝签名正是这一句）；
代价只是丢掉 PID namespace 隔离，而被沙箱的命令本来就和 DSH 跑在同一用户下。

**2）干脆不走沙箱**：`--no-sandbox` / `danger-full-access`，命令不加包装地跑，
也就没有「没法建沙箱所以拒绝执行」这种死路。

## 部署到远程主机

在无图形界面的 Linux 主机上装 DSH、装本插件、把 Web 界面暴露到公网，完整命令见
[docs/remote-deploy.md](./docs/remote-deploy.md)。三个要点：

- helper 用 `npm pack` 出来的 **tarball** 安装：用目录安装会变成 `link:`，Node 会从插件的
  真实路径解析裸 import，`schemastery` / `dsh-tools` 全都找不到；
- `@deepseek-ai/dsh-tools` 是 peerDependency，而 profile 默认 `autoInstallPeers: false`，
  需要让 profile 能解析到它——**要 link，不要另装一份**（见下条）；
- **同一个 harness 包只能有一份物理拷贝**。`@deepseek-ai/dsh-tools` 用 `Symbol()` 把
  「工具执行调度器」交给 agent loop，Node 按解析后的真实路径判定模块身份，profile 里多一份
  拷贝就是多一个 Symbol，agent loop 读到 `undefined`，于是每次工具调用都失败在
  `Cannot read properties of undefined (reading 'prepare')`。启动器因此把 profile 里
  `@deepseek-ai/*` 的副本一律换成指向 dsh 自带那几份的软链（并写回
  `link:` 依赖），手工等价操作：
  `ln -sfn <dsh>/node_modules/@deepseek-ai/dsh-tools <profile>/node_modules/@deepseek-ai/dsh-tools`
  并把 package.json 里的版本号改成 `link:<dsh>/node_modules/@deepseek-ai/dsh-tools`；
- DSH Web 自己还有一道 `?token=` 门（每次启动都变），所以完整地址是
  `https://<endpoint>.<base>/?token=…`，外面再套一层本插件的 Basic Auth。
  自动暴露（`autoExpose`）时用 `credentialsFile` 把地址与随机账号密码以 0600 权限落盘。

## 一台机器只能有一个实例

两个实例共用 `$DSH_HOME` 会互相抢会话所有权：客户端列斜杠指令时要 resume 当前会话，
抢输的那个是 `SessionAlreadyOwnedError` → 指令目录加载失败 → `/` 菜单空白、`/compact`
报失败，且服务端不留日志。启动器因此会**先停掉同一 profile 的旧进程**（`up` 自带这步），
并在 `status` 里列出机器上其它 dsh 进程；真要并行就各给一个 `--dsh-home`。

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

- **凭证默认只有一层：DSH 自己的 `?token=`。** 启动器不再默认给隧道加 HTTP Basic Auth
  （要加就 `--basic-auth`）。这一层的机制是：每次启动 DSH 生成一个进程级 launch token，
  写在它打印的 URL 里；用这个 URL 访问一次，服务端就下发一个 **30 天**（`cookieMaxAgeDays`，
  默认 30）的签名 cookie（`HttpOnly; SameSite=Strict; Path=/`，内容绑定 Host 与该次签发/过期
  时间，HMAC-SHA256 签名，密钥持久化在 `~/.dsh/.credentials.yaml` 的
  `client-connection/browser-session` 记录里）。之后每个请求都要带这个 cookie，否则 401
  `dsh web authentication required`。因为签名密钥是持久的，**cookie 能活过 DSH 重启**，
  变的只是 URL 里那个 launch token。
- 代价必须说清楚：**URL 就是密码**。它出现在浏览器历史、Referer、聊天记录里都可能被拿走；
  cookie 也没有 `Secure` 标记、有效期 30 天、没有账号维度、没有速率限制。要第二层就
  `--basic-auth`（可配 `--auth-user/--auth-pass` 固定账号）。
- **TTL 默认不过期**（`--ttl 0`）。设了 `--ttl N` 时，helper 到点自动退出，公网 URL 会变成
  `404 {"error":"no available upstreams"}` —— 注意这时 DSH 本体还在跑（回环地址照常），
  只是隧道没了；`dsh-piko-remote status` 能看出 `expires` 字段；
- `allowDshUiExpose` 默认关，`remote_expose` 在未打开时会拒绝暴露 DSH 端口；
- 插件日志只记录 endpoint，不记录完整 URL 和账号密码。

## License

MIT
