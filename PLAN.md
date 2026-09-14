# PLAN.md — DSH Piko Remote 插件

> 把本机端口通过 piko 隧道暴露到远程服务器，并把远程访问地址交还给用户 / 交还给模型。
> 调用方式完全对齐 `opencode-piko-remote`，交付形态是 **DSH（DeepSeek Harness）插件包**。

---

## 实现现状（2026-09-14）

**已落地并端到端验证**（commit `ffd47c6`，插件 `0.2.0`）：

| 部分 | 状态 | 证据 |
|---|---|---|
| Go helper `piko-expose` | ✅ | `go/`，13 个单测；`scripts/smoke-test.sh` 真实公网 3/3 |
| 插件半侧（supervisor / endpoint / tools / config） | ✅ | `lib/`，48 个 node 单测 |
| 三个模型工具 | ✅ | `remote_expose` / `remote_status` / `remote_close` |
| 子域名模式端到端 | ✅ | `dsh-zi38kw.clauded.friddle.me`：SPA 200、`/assets/*.js` 200、`/api/remote.mux` **101** |
| 远程部署链路 | ✅ | `docs/remote-deploy.md`（Ubuntu + `dsh@0.1.5-rc.1` + tarball 安装） |

关键结论（覆盖下面调研里的推测）：

1. **公共服务器不需要 upstream key**：直接连 `https://clauded.friddle.me` 即可。
2. **`*.clauded.friddle.me` 泛域名 DNS + 泛域名证书已就绪**（证书 `CN=*.clauded.friddle.me`），
   Phase 4/5 的最后一块基础设施已补齐。
3. **DSH Web 必须走子域名模式**：已在源码中确认（`dsh-client-connection/lib/client.js`
   用 `location.origin` 拼 API base），路径模式下 `/api/...` 会被当成另一个 endpoint。
4. **Host 本地化可以替代 `--trusted-host`**：helper `--preserve-host=false` 会同时改写
   Host/Origin/Referer，DSH 的 `Origin==Host` 围栏直接通过，因此 endpoint 不必固定。
5. **helper 二进制不进 git**：`bin/` 被忽略；远程安装必须用 `npm pack` 出来的 tarball
   （目录安装会变成 `link:`，裸 import 解析不到宿主包），见 `docs/remote-deploy.md`。

**仍未做**（Phase 6）：客户端设置卡片、settings 命名空间注册、release 时交叉编译
helper 并随包发布（目前靠 `scripts/build-helper.sh` + 手工分发）。

---

## 0. 一句话目标

在 DSH 里装一个插件，模型或用户说一句「把本地端口暴露出去」，
插件就拉起 piko 上游连接，返回 `https://<endpoint>.clauded.friddle.me/`
（**子域名模式**：整个 origin 归该 endpoint，路径原样透传），
并且这个隧道随会话生命周期可控（查看 / 关闭 / 超时自动关）。

---

## 1. 调研结论（已验证事实，非推测）

### 1.1 opencode-piko-remote 到底怎么调用的

参考实现：`friddle/opencode-piko-remote`（本机已 clone 到 `tmp/opencode-piko-remote/` 作为只读参考）。

调用链（`client/src/service.go:147-214`）：

```
piko client.Upstream{URL, Logger}
      → upstream.Listen(ctx, endpointID)        // 返回 net.Listener，远端流量从这条流进来
      → reverseproxy.NewServer(listenerConfig…)  // piko 自带反向代理
      → Serve(ln)                                // 转发到 listenerConfig.Addr 指向的本地端口
```

关键点：

| 事实 | 位置 |
|---|---|
| 上游连接 URL = piko server 地址，走到 `/piko/v1/upstream/<endpointID>` 的 WebSocket | piko `client/upstream.go:167-184` |
| 只出站、不开放本地端口 | 同上注释 |
| endpoint ID 只允许 `[A-Za-z0-9_-]`，非法字符会导致 `404 websocket: bad handshake` | gotty-piko `client/src/config.go` `sanitizeSession()` |
| 服务端 nginx 按**路径第一段**选 endpoint：`X-Piko-Endpoint: $1`，原样透传 URI | `server/build/piko.conf` |
| 因此 localhost 服务收到的是 **带前缀的路径**，SPA/绝对路径资源会 404 → opencode-piko 专门加了一个剥离前缀的反向代理 | `client/src/middleware.go`（`RewriteProxy`，条件剥离，`pr.Out.Host = pr.In.Host`） |

**等价 CLI**：`piko agent http <endpoint> <port>`（`piko agent start --config.file agent.yaml`），
配置字段就是 `ListenerConfig{endpoint_id, addr, protocol, timeout}`。
所以「一个进程连上游 + 转发到本地端口」既可以用 SDK 写，也可以用官方 CLI 跑。

### 1.2 远程服务器现状（clauded.friddle.me）

实测结果：

| 探测 | 结果 | 含义 |
|---|---|---|
| `GET https://clauded.friddle.me/health` | `200 {"status":"ok"}` | 服务活着 |
| `GET https://clauded.friddle.me/` | `301 → github.com/friddle/claude-web-remote` | 根路径被占用（兜底跳转） |
| `GET https://clauded.friddle.me/piko/v1/upstream/probe` | `400` | 该路径**确实**路由到了 piko upstream（400 = 缺 WS upgrade 头，符合预期） |
| `clauded.friddle.me:8022 / :8088` 直连 | 超时 | 只有 443 对外（Cloudflare 前置） |

⇒ **连上游用 `https://clauded.friddle.me`，访问用 `https://clauded.friddle.me/<endpoint>/`。**
⇒ 服务端是否要求 token（`--upstream-key` / `PIKO_TOKEN`）未知，**Phase 0 必须实测**。

### 1.3 DSH 插件体系（不需要 fork DSH）

DSH Desktop 2.0.9 内是 `@deepseek-ai/dsh 0.1.5-rc.1`。插件是**树外 npm 包**，机制已完全打通：

```
~/.dsh/profiles/desktop/package.json
  ├─ dependencies: { "<插件包名>": "<spec>" }
  └─ dsh.profile.bundles: [ "@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", "<插件包名>" ]
```

安装命令（官方实现见 `@deepseek-ai/dsh/lib/plugin-Ddi42qoW.js`）：

```bash
dsh-desktop plugin add <npm名 | /绝对路径 | github:... | file:...>
#  = 在 profile 目录跑 pnpm，然后按「已安装包是否声明 dsh.bundle」自动增删 bundles 行
```

插件包契约：

| 能力 | 落地方式 | 证据 |
|---|---|---|
| 宿主插件 | `main` 导出 `{ name, inject, Config, apply(ctx, config) }`（ESM） | `@deepseek-ai/dsh-tool-todo/lib/index.js` 末尾 `export { Config, apply, inject, name }` |
| 挂进 profile | `package.json` → `dsh.bundle.patch: "./cordis.patch.yml"`，patch 里 `- insert: [{id, name}]` | `dshmarket/cordis.patch.yml` |
| 注册模型工具 | `ctx.tools.register(defineTool({name, description, parameters, output, execute}))` | `@deepseek-ai/dsh-tools` README |
| 起子进程 | `ctx.subprocess.resolveExecutable()` + `ctx.subprocess.spawn({argv, stdio, graceMs})` | `@deepseek-ai/dsh-subprocess` README |
| 拿本机 GUI 端口 | `ctx.get('webServer')?.port` | `@deepseek-ai/dsh-web-app/lib/index.js` `localWebUrl()` |
| 注册本地 HTTP 路由 | `ctx.webServer.register({path, handler})` | `@deepseek-ai/dsh-host-webserver` README |
| 客户端 UI | `package.json` → `dsh.client: {inject, platform:'web'}` + `exports["./client"]`，用 `ctx.slots` 注册 | `dshmarket/package.json` + `dshmarket/client/client.js:9820-9860` |
| 运行时配置 | `ctx.settings` 命名空间（secret 字段走 credentials） | `@deepseek-ai/dsh-settings` README |

### 1.4 两个必须正视的硬约束（已解决 / 已收窄）

**(A) DSH Web 的 API 是 origin 绝对路径 → 路径前缀代理不可用。已按方案 A（子域名）落地。**

`@deepseek-ai/dsh-client-connection/lib/client.js:6255`：

```js
function resolveBase() {
  const location = globalThis.location;
  return location?.origin !== undefined && location.origin !== "null" ? location.origin : INTERNAL_BASE;
}
// 调用：new URL(`${channel}/${endpoint}`, resolveBase())  →  https://host/api/<endpoint>
// 流：  /api/remote.mux  (WebSocket)
```

浏览器在 `https://clauded.friddle.me/dsh-xxx/` 打开页面后，`location.origin` 是裸域名，
它会去请求 `https://clauded.friddle.me/api/...` → 路径第一段被当成 endpoint `api` → 永远打不到隧道。

**解法：子域名模式——整个 origin 归一个 endpoint 所有，路径原样透传。**

✅ **服务端已实现**（gotty-piko `server/`，commit `9920b36`）：

- `SUBDOMAIN_BASE`（默认 `clauded.friddle.me`）开启后，`<endpoint>.<base>` 的请求整体路由到该 endpoint，
  路径不被剥离也不被加前缀——所以 `/api/...`、`/assets/...` 都原样到达上游。
- `/piko/*`、`/v1/upstream/*`、`/health` 永不被劫持：否则用子域名做 `--remote` 的 Agent
  会把自己的隧道切断，健康检查也会依赖 endpoint 在线。
- `SUBDOMAIN_RESERVED`（默认 `www,api,admin,piko,health,root-service,localhost`）保留给普通路径路由。
- 端点标签严格限制为 `[a-z0-9-]`：DNS 标签要能过泛域名证书，下划线和大小写一律拒绝。

⏳ **剩余是基础设施**（用户自理）：`*.clauded.friddle.me` 的泛域名 DNS + 泛域名证书。
现有 `clauded.friddle.me` 证书是 `SAN: friddle.me, *.friddle.me`，**不覆盖**二级泛域名；
计划用源站 Let's Encrypt DNS-01 签 `*.clauded.friddle.me`。

**(B) 目标从 DSH Desktop 收窄为 `dsh`（CLI）。**

原计划的 Desktop 403 问题不再相关。现在只面对 `dsh --profile web` 的
**`/api` 浏览器信任围栏**（`@deepseek-ai/dsh-client-connection/lib/index.js`）：

```js
if (!isLoopbackHostname(hostUrl.hostname) && !isTrustedAuthority(hostUrl, trustedHosts)) return false;
if (headers['sec-fetch-site'] === 'cross-site') return false;
const origin = headers['origin'];
if (origin === undefined) return true;
return new URL(origin).host === hostUrl.host;   // ← Origin 必须与 Host 同源
```

三条约束，缺一不可：

1. Host 必须是回环地址，或在 `trustedHosts` 里；
2. Origin 若存在，`Origin.host` 必须等于 `Host.host`；
3. `sec-fetch-site` 不能是 `cross-site`。

⇒ 于是有两条可走的路，服务端都支持：

| 方式 | 服务端配置 | DSH 侧配置 | 代价 |
|---|---|---|---|
| **保留 Host**（推荐） | `SUBDOMAIN_PRESERVE_HOST=true`（默认） | `dsh --profile web --trusted-host <endpoint>.<base>` | `--trusted-host` 只接受裸 authority、**不支持通配符**，所以端点名要固定，不能用随机后缀 |
| **本地化 Host** | `SUBDOMAIN_PRESERVE_HOST=false` | 无 | 服务端会把 `Host` 改写成回环地址，**并且必须同时改写 `Origin`/`Referer`**——只改 Host 不改 Origin 依旧 403。上游会以为自己在 `127.0.0.1`，绝对地址重定向和依赖 Host 的 cookie 可能不符合预期 |


## 2. 总体架构

```
┌──────────────── 本机 (macOS) ─────────────────────────┐
│                                                        │
│  dsh (--profile web) 宿主进程                           │
│   └─ dsh-plugin-piko-remote (本插件, 宿主半侧)          │
│        ├─ ctx.subprocess.spawn ──► piko-expose (Go)     │
│        ├─ ctx.tools.register  remote_expose/status/close│
│        ├─ ctx.webServer.register  GET /piko-remote      │
│        └─ ctx.settings  namespace: piko-remote          │
│                                                        │
│  piko-expose  ──WS(443)──►  clauded.friddle.me          │
│      └─ 本地反代 ──► 127.0.0.1:<port> (dsh web / 任意端口)│
└────────────────────────────────────────────────────────┘
                     ▲
                     │  wss://clauded.friddle.me/piko/v1/upstream/<endpoint>
                     │
   浏览器  https://<endpoint>.clauded.friddle.me/   （子域名模式，路径原样）
                     │
   gotty-piko server │  按 Host 第一段选 endpoint，路径不剥离
                     ▼
   服务端 subdomainMiddleware → ProxySubdomainRequest → piko proxy:8023 → Agent
```

---

## 3. 交付物与仓库布局

仓库：**`friddle/dsh-plugin-piko-remote`**（repo 根即 npm 包根，可被
`dsh plugin add github:friddle/dsh-plugin-piko-remote` 直接安装）

```
dsh-plugin-piko-remote/
├── package.json                # dsh.bundle.patch + exports + peerDependencies
├── cordis.patch.yml            # - insert: [{ id: piko-remote, name: dsh-plugin-piko-remote }]
├── lib/
│   ├── index.js                # 宿主半侧：name / apply(ctx)          ✅ 已有骨架
│   ├── supervisor.js           # piko-expose 子进程管理（启停/重启/状态机）
│   ├── endpoint.js             # endpoint 命名与 sanitize（对齐 gotty-piko sanitizeSession）
│   └── settings.js             # settings 命名空间注册
├── client/
│   └── client.js               # 客户端半侧：settings 卡片 + 复制地址 + 二维码（Phase 6）
├── go/                         # Go helper 源码
│   ├── go.mod                  # module piko-expose; require github.com/andydunstall/piko v0.7.0
│   ├── main.go                 # flag 解析 + JSON ready 行 + 生命周期
│   ├── tunnel.go               # 复用 opencode-piko service.go startPiko() 的写法
│   └── rewrite.go              # 复用 middleware.go 的 RewriteProxy
├── bin/                        # 预编译产物（构建脚本生成，git 忽略）
├── scripts/
│   ├── build-helper.sh         # 交叉编译 4~5 平台
│   └── dev-install.sh          # dsh plugin add ./ 的开发装法
├── docs/
│   └── troubleshooting.md
├── README.md
└── PLAN.md
```

### 服务端（不在本仓库）

子域名模式实现在 [gotty-piko](https://github.com/friddle/gotty-piko) 的 `server/`：

| 文件 | 职责 |
|---|---|
| `server/subdomain/subdomain.go` | Host → endpoint ID 的严格解析 |
| `server/handlers/handler.go` | `subdomainMiddleware`：劫持与让路规则 |
| `server/proxy/manager.go` | `ProxySubdomainRequest` + Host/Origin 本地化 |
| `server/config/config.go` | `SUBDOMAIN_BASE` / `SUBDOMAIN_RESERVED` / `SUBDOMAIN_PRESERVE_HOST` |

镜像：`ghcr.io/friddle/gottyp-piko-server:latest`（CI 在 `server/**` 变更时自动发布）

---

## 4. 接口设计

### 4.1 Go helper `piko-expose`

```
piko-expose \
  --remote      https://clauded.friddle.me \   # 默认，与 opencode-piko 的 DefaultRemote 一致
  --endpoint    dsh-a1b2c3                   \   # 必填；[a-z0-9-]，≤63（要当 DNS 标签），非法直接拒绝
  --target      127.0.0.1:43120              \   # 必填；只写端口号时自动补 127.0.0.1:
  --url-mode    subdomain                    \   # subdomain | path，只影响打印出来的 remoteUrl
  --strip-prefix auto                        \   # auto：subdomain 不剥，path 剥 /<endpoint>；命中才剥离
  --upstream-key <token>                     \   # 可选，piko API key（公共服务器不需要）
  --auth / --auth=false                      \   # 默认开
  --auth-user / --auth-pass                  \   # 默认都随机生成（20 位密码，去易混字符）
  --preserve-host / --preserve-host=false    \   # 默认 true；false 表示同时改写 Host/Origin/Referer
  --auto-exit 0                              \   # 分钟；0 = 不自动退出
  --local-addr 127.0.0.1:18081               \   # 调试：额外在本地起同样的 handler
  --insecure                                 \   # 调试：跳过 piko 服务器证书校验
  --json                                         # stdout 输出机读事件
```

stdout 协议（一行一个 JSON，供插件解析；`auth` 在 `ready` **之前**，这样等到 ready
的 supervisor 已经拿到账号密码）：
```json
{"event":"auth","user":"yg8uncvn","pass":"YpSdimgdAgY9A9Xv7CZy"}
{"event":"ready","endpoint":"dsh-zi38kw","target":"127.0.0.1:3080","remoteUrl":"https://dsh-zi38kw.clauded.friddle.me/","urlMode":"subdomain"}
{"event":"closed","reason":"signal"}
{"event":"error","message":"..."}
```

实现要点（直接复用已验证代码，不重新发明）：
- `client.Upstream{URL: parsed, Token: key, Logger}` → `Listen(ctx, endpoint)`
- 本地起 `httputil.ReverseProxy`，`Rewrite` 里条件剥离 `--strip-prefix`（`middleware.go` 同款逻辑）
- `oklog/run` 管理 opencode/代理/信号/定时器（对齐 `service.go`）
- 必须保留 WebSocket upgrade 透传（DSH 的 `/api/remote.mux`、opencode web 都依赖）

### 4.2 模型工具

| 工具 | 参数 | 返回 |
|---|---|---|
| `remote_expose` | `port?`(默认 DSH GUI 端口) `name?` `ttlMinutes?` `auth?` | `{ endpoint, remoteUrl, localPort, authUser?, expiresAt? }` |
| `remote_status` | — | `[{ endpoint, remoteUrl, localPort, state, startedAt, expiresAt }]` |
| `remote_close` | `endpoint?`（缺省=全部） | `{ closed: [endpoint…] }` |

`remote_expose` 的工具描述里必须写明：**暴露 DSH GUI = 把本机 Agent 控制权交给任何拿到该 URL 的人**，
默认要求开启 Basic Auth，且默认 TTL 非空。

### 4.3 settings 命名空间 `piko-remote`（⏳ Phase 6）

当前所有配置走 cordis row 的 `config`（见 README），**尚未**注册 `ctx.settings`
命名空间，所以还没有「设置页可改」的入口。计划中的 namespace：

```yaml
piko-remote:
  remote: https://clauded.friddle.me   # piko 服务器
  upstreamKey: <secret>                # 走 credentials，不落 settings 明文
  endpointPrefix: dsh
  defaultTtlMinutes: 120
  basicAuth: true
  allowDshUiExpose: false              # 单独开关，默认关
```

### 4.4 本地状态页

`ctx.webServer.register({ path: '/piko-remote', handler })` —— 只读 JSON/HTML：
当前隧道列表、地址、二维码、一键复制、关闭按钮。仅回环可见（webServer 本身只绑 127.0.0.1）。

---

## 5. 分阶段任务

### Phase 0 — 可行性验证 ✅
- [x] 用 Go helper 连 `https://clauded.friddle.me`，暴露一个静态目录并验证公网可访问
- [x] 确认服务端**不要求** token（`--upstream-key` 留空即可连通）
- [x] 验证 WebSocket 透传：DSH `/api/remote.mux` 返回 `101 Switching Protocols`
- [x] 记录 endpoint 行为：子域名模式下标签为 `[a-z0-9-]`；重名会被 piko 负载均衡（所以默认随机后缀）

**出口标准**：✅ 拿到可用 `remoteUrl`，鉴权方式 = 可选 Basic Auth + 子域名路由。

### Phase 1 — Go helper ✅
- [x] `go/piko-expose`：`main.go` + `tunnel.go` + `rewrite.go`
- [x] 条件剥离前缀 + Host/Origin/Referer 改写 + Basic Auth + TTL + JSON 事件
- [x] `scripts/build-helper.sh`：darwin/arm64、darwin/amd64、linux/amd64、linux/arm64、windows/amd64
- [x] 单元测试：prefix 剥离边界（`/ep`、`/ep/`、`/ep/x`、`/api/x` 不剥离、`/epx` 不剥离）

**出口标准**：✅ `piko-expose --target 127.0.0.1:8080 …` 单跑成功，SIGTERM 干净退出。

### Phase 2 — 插件骨架 + 安装链路 ✅
- [x] `package.json`（`type: module`、`dsh.bundle.patch`、`exports`、`peerDependencies`）
- [x] `cordis.patch.yml`（row id `piko-remote`）
- [x] `lib/index.js`：`name` + `apply(ctx)`；可选服务走 `ctx.inject`（`subprocess` / `webServer`），
      这样 headless profile 里插件仍能加载并说明它做不了什么
- [~] `scripts/dev-install.sh`：未单独写；开发装法写在 `docs/remote-deploy.md`（含两个坑）
- [x] 安装链路实测：远程 `dsh plugin --profile piko add <tarball>` 后插件激活

**出口标准**：✅ 插件能被 DSH 加载且不报错，日志里能看到本地 web 端口。


### Phase 3 — 隧道服务与工具 ✅
- [x] `lib/supervisor.js`：用 `ctx.subprocess` 启动 helper，解析 ready/auth 行，维护状态机（starting/running/failed/stopped），dispose 时 `terminate()` + `waitForExit()`
- [x] `lib/endpoint.js`：`<prefix>-<base36 随机>` + sanitize（DNS 标签规则）
- [x] `remote_expose` / `remote_status` / `remote_close` 三个工具
- [x] 二进制定位策略：优先包内 `bin/`，缺失则报可执行的修复指引（不静默下载）

**出口标准**：✅ 模型调用 `remote_expose(port=8000)`，拿到可用 URL，`remote_close` 能停掉且进程不残留。

### Phase 4 — DSH Web 暴露（旗舰场景）✅
- [x] 服务端侧：子域名模式（见 Phase 5）
- [x] Go helper 支持 `--strip-prefix` 条件剥离（子域名模式下为空，保留给路径模式）
- [x] `remote_expose()` 不带 `port` 时默认取 DSH web 端口（`ctx.inject(['webServer'])` 懒取）
- [x] 用静态资源（`/assets/*.js` 200）与 `/api/remote.mux`（**101**）验证
- [x] 走到 `preserveHost: false` 路线：helper 同时改写 Host/Origin/Referer，
      `--trusted-host` 与固定 endpoint 都不再必要（endpoint 默认随机）
- [x] 默认开 Basic Auth，`allowDshUiExpose` 默认 false，需用户显式打开

**出口标准**：✅ 远端浏览器打开 `https://dsh-zi38kw.clauded.friddle.me/?token=…` 能加载完整界面并建立流。


### Phase 5 — 服务端子域名模式 ✅ 已完成
- [x] `server/subdomain` 包：Host → endpoint ID 的严格解析（大小写、端口、尾点、嵌套标签、保留名）
- [x] `handlers.subdomainMiddleware`：gin 前置中间件，`/piko`、`/v1/upstream`、`/health` 让路
- [x] `proxy.ProxySubdomainRequest`：整个 origin 转发，路径原样透传
- [x] `SUBDOMAIN_PRESERVE_HOST=false` 时同步本地化 `Origin`/`Referer`
- [x] 端到端测试（真实反向代理 + 假 piko 端口）+ 解析单测 + config 单测
- [x] 修复镜像流水线：补齐 `build-push` 目标、`packages: write`、QEMU、`server/**` 变更自动发镜像
- [x] **基础设施**：`*.clauded.friddle.me` 泛域名 DNS + 泛域名证书（2026-09-14 实测生效，
      证书 `CN=*.clauded.friddle.me`，直连 192.227.178.111）

**代码已在 gotty-piko `main`（commit `9920b36`），镜像由 CI 自动发布为 `ghcr.io/friddle/gottyp-piko-server:latest`。**


### Phase 6 — 客户端 UI 与发布
- [ ] `client/client.js`：`settings.plugin.item`（namespace `piko-remote`）卡片：地址、复制、二维码、启停
- [ ] 可选：`sidebar.footer.action` 小入口
- [ ] README（中英）、`docs/troubleshooting.md`、截图
- [ ] GitHub Actions：release 时交叉编译 helper 并随 npm 包发布
- [ ] 发布 npm + 提交 dsh-market 收录（可选）

---

## 6. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| 服务端要求 token，而我们没有 | 隧道连不上 | Phase 0 先测；helper 支持 `--upstream-key`；文档给出自建服务端步骤 |
| DSH 的 `/api` origin 绝对路径 | 远端页面打不开（404 on `/api`） | ✅ 子域名模式（服务端已实现） |
| DSH `/api` 信任围栏要求 Host==Origin | 远端 403 | 保留 Host 时用 `--trusted-host`（需固定端点名）；或 `SUBDOMAIN_PRESERVE_HOST=false` 由服务端同时改写 Host 与 Origin |
| `--trusted-host` 不支持通配符 | 随机端点名无法预先声明 | DSH 场景固定用 `dsh` 这个端点名 |
| 泛域名证书缺失 | 子域名无法 HTTPS | 用户自理（Let's Encrypt DNS-01 或 Cloudflare Total TLS） |
| **暴露 GUI = 交出 Agent 控制权** | 严重安全后果 | 默认 Basic Auth + TTL + `allowDshUiExpose=false`；工具描述显式告警；远端 URL 不落日志 |
| endpoint 重名被负载均衡 | 流量串号 | 随机后缀（`dsh-a1b2c3`）；暴露前查 `remote_status` |
| 子进程残留 | 端口/连接泄漏 | `ctx.subprocess` dispose → `terminate()`；helper 自己处理 SIGTERM；注册 `waitForExit` |
| DSH 版本升级导致 API 漂移 | 插件加载失败 | 锁定已验证版本（desktop 2.0.9 / dsh 0.1.5-rc.1），peerDependencies 声明范围 |
| asar 路径 / Electron 环境差异 | 本地开发与打包行为不同 | 开发期用 `dsh-desktop plugin add` 走真实 profile，不用 mock |

---

## 7. 验收标准

1. `dsh plugin add /path/to/dsh-plugin-piko-remote` → 重启后在「设置 → 插件 → 插件列表」可见且 active。
2. 模型调用 `remote_expose(port=8000)`，返回的 URL 在**另一台设备/4G**上可访问到本机服务。
3. `remote_expose()` 默认能把 `dsh --profile web` 暴露出去，远端浏览器能完整对话 + 收到流式输出。
4. `remote_close` 后：远端立即 404/无路由，本机 helper 进程消失（`ps` 验证无残留）。
5. TTL 到期自动关闭，`remote_status` 状态正确。
6. 未开 Basic Auth 时工具返回明确告警；`allowDshUiExpose=false` 时拒绝暴露 GUI。
7. 不修改 DSH 自身任何文件（除 profile 的 package.json / bundles 与 pnpm 产物外），可一条命令卸载。

---

## 8. 已确认决策（2026-09-14）

| # | 决策点 | 结论 |
|---|---|---|
| D1 | 仓库 | ✅ 独立仓库 `friddle/dsh-plugin-piko-remote`，repo 根即 npm 包根，Go helper 与插件同仓 |
| D2 | 隧道引擎 | ✅ 自带 Go helper `piko-expose`（复用 opencode-piko 的 `client.Upstream` 写法 + 前缀剥离反代 + JSON 事件协议） |
| D3 | 远端访问方案 | ✅ **子域名模式（原方案 A）**，服务端已实现；不做 Cookie 亲和路由 |
| D4 | 交付范围 | ✅ 一路做到 `dsh --profile web` 远端可对话 |
| D5 | 目标面 | ✅ 只管 `dsh` CLI（`dsh --profile web`），**不管 DSH Desktop**（Desktop 的 403 / renderer token 问题不再相关） |
| D6 | DNS / 证书 | ✅ **用户自理**，不在本仓库范围内；代码侧只需 `SUBDOMAIN_BASE` 默认值与文档 |
| D7 | 客户端 UI / npm 发布 | 顺延到 Phase 6，不阻塞「远端可对话」 |
| D8 | 提交方式 | ✅ 直接推 `main` |

### 关键路径上的剩余不确定项

1. `*.clauded.friddle.me` 的泛域名证书（用户负责）——没有它 Phase 4 无法端到端验收。
2. 服务端是否要求 `--upstream-key`（Phase 0 实测）。
3. WebSocket upgrade（`/api/remote.mux`）穿过 piko 链路——`/health` 通了不代表 WS 通了。

### 执行顺序

```
Phase 0 可行性验证 ──► Phase 1 Go helper ──► Phase 2 插件骨架+安装链路   [← 当前]
                                                      │
                       Phase 4 DSH Web 暴露 ◄── Phase 3 隧道服务与工具
                              │
                              ▼
                       Phase 5 服务端子域名模式 ✅ 代码完成（等 DNS/证书）
                              │
                              ▼
                       验收：4G 远端对话
```


## 9. 附：参考文件索引

| 内容 | 路径 |
|---|---|
| piko 上游调用参考 | `tmp/opencode-piko-remote/client/src/service.go` |
| 前缀剥离反代参考 | `tmp/opencode-piko-remote/client/src/middleware.go` |
| endpoint sanitize 参考 | `client/src/config.go`（gotty-piko） |
| 服务端 nginx 路由 | `tmp/opencode-piko-remote/server/build/piko.conf` |
| piko SDK（本地已缓存） | `~/go/pkg/mod/github.com/andydunstall/piko@v0.7.0/` |
| DSH 插件导出契约范例 | asar `@deepseek-ai/dsh-tool-todo/lib/index.js` |
| DSH 插件安装实现 | asar `@deepseek-ai/dsh/lib/plugin-Ddi42qoW.js` |
| DSH 浏览器访问门槛 | asar `lib/desktop-browser-access-5-Ph3Uv7.js` |
| DSH 客户端 API base（关键约束） | asar `@deepseek-ai/dsh-client-connection/lib/client.js:6255` |
