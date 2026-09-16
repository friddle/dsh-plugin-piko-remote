# 一键启动：`dsh-piko-remote`

`dsh-piko-remote` 是这个仓库的第二个 Go 命令，对应 opencode 那边的
`opencode-piko-remote`：**一条命令**把「运行时 → 插件 → 启动 → 公网地址」全做完。

```bash
dsh-piko-remote up --plugin friddle/dsh-plugin-piko-remote
```

它做这几件事，每一步都是幂等的：

1. **准备 Node**：PATH 上的 `node` 满足 DSH 的 `engines`（`^22.19.0 || >=24.0.0`）就直接用；
   否则下载一份到自己的数据目录（默认先走 npmmirror 镜像，失败再回退 nodejs.org），
   不碰系统 Node。
2. **准备 dsh + pnpm**：优先复用 `--dsh` 指定的、或数据目录里已装的、或 PATH 上的 `dsh`；
   都没有就 `npm i -g --prefix <managed node>` 装 `@deepseek-ai/dsh@0.1.5-rc.1` 和 `pnpm`
   （装 dsh 的同时把它的 Node 依赖一并带上）。
3. **建 profile**：`dsh --profile <name> --from-default-profile web --dump-config`
   —— 从自带的 web 模板初始化，但不启动。默认 profile 名是 `dsh-piko`。
4. **装插件**：`dsh plugin add`，支持多种写法（见下）。
5. **补齐 peerDependency**：读插件 `package.json`，把 profile 里解析不到的
   `@deepseek-ai/*` peer 补上（profile 默认 `autoInstallPeers: false`，
   不补的话插件会以 `ERR_MODULE_NOT_FOUND` 加载失败）。补的方式是
   **软链到 dsh 自带的那一份**（依赖写回 `link:<dsh>/node_modules/@deepseek-ai/...`），
   不是从 npm 另装一份——同一个 harness 包出现两份物理拷贝会让它的 `Symbol()`
   跨模块身份失效，工具调用会死在
   `Cannot read properties of undefined (reading 'prepare')`。装完还会再扫一遍
   profile，把历史遗留的副本一并换成链接（幂等，可反复 `up`）。
6. **补 helper 二进制**：从 GitHub/npm 装的插件不带 `bin/`（二进制是构建产物），
   启动器把自己内嵌的 `piko-expose` 写进插件的 `bin/` 并 `chmod +x`。
7. **写隧道配置**：不修改你自己的 `cordis.patch.yml`，而是生成一份 overlay，
   启动时用 `--patch` 叠加（DSH 的 patch 层顺序保证它最后生效）。
8. **后台启动**：`setsid` 脱离当前会话（SSH 断开也不会被带走），日志写进数据目录。
9. **等就绪**：轮询日志里的 `dsh web: http://...?token=...` 和隧道凭据文件，
   拿到后打印本地地址、公网地址、随机账号密码。

## 插件地址的写法

`--plugin` 可以重复，用来一次装多个插件：

| 写法 | 解释 |
|---|---|
| `friddle/dsh-plugin-piko-remote` | GitHub 简写，自动展开成 `github:friddle/dsh-plugin-piko-remote` |
| `owner/repo#v1.2.3` | 指定分支/标签/commit |
| `github:owner/repo` | 同上，显式写法 |
| `@deepseek-ai/dsh-tools@0.1.5-rc.1` | npm 包（`@scope/name[@version]`） |
| `./my-plugin`、`/abs/my-plugin` | 本地目录（注意：pnpm 会装成 `link:`，裸 import 解析不到宿主包，
  见 [remote-deploy.md](./remote-deploy.md) 的坑） |
| `https://host/plugin.tgz` | 远程 tarball |
| `*.tgz` | 本地 tarball（推荐用于本地开发：它会在 profile 里放一份真实拷贝） |

## 常用命令

```bash
dsh-piko-remote up                       # 默认命令，等价于 dsh-piko-remote
dsh-piko-remote status                   # 当前实例、地址、账号密码
dsh-piko-remote logs --lines 80          # 看 DSH 日志
dsh-piko-remote down                     # 停掉 DSH（插件 dispose 会顺手杀掉 piko-expose）
dsh-piko-remote up --json                # 给脚本用：stdout 只有一行 JSON 结果
```

`--json` 输出形如：

```json
{"event":"ready","profile":"dsh-piko","pid":1234,
 "localUrl":"http://127.0.0.1:3080/?token=...",
 "remoteUrl":"https://dsh-xxxxxx.clauded.friddle.me/?token=...",
 "endpoint":"dsh-xxxxxx","authUser":"...","authPass":"...","expiresAt":"...",
 "logFile":"/home/u/.local/share/dsh-piko-remote/logs/dsh-dsh-piko.log"}
```

## 主要参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--plugin SPEC` | `github:friddle/dsh-plugin-piko-remote` | 要装的插件，可重复 |
| `--profile NAME` | `dsh-piko` | DSH profile 名 |
| `--remote URL` | `https://clauded.friddle.me` | piko 服务器 |
| `--endpoint NAME` | 随机 | 固定 endpoint 名（例如要配合 `--trusted-host` 时） |
| `--ttl MINUTES` | `0` | 隧道存活时间。**默认 0 = 不过期**：只有显式给值，helper 才会拿到 `--auto-exit N`。设了 TTL 的隧道到点会自己退出，公网地址随即变成 `404 no available upstreams`（DSH 本体不受影响） |
| `--basic-auth` | `false` | 隧道 HTTP Basic Auth。**默认关**：DSH 自带的 `?token=` 围栏才是真正的凭证（启动 token → 30 天签名 cookie，`HttpOnly; SameSite=Strict`，密钥持久化）。打开就是加第二层，账号可用 `--auth-user/--auth-pass` 固定 |
| `--expose-dsh-ui` | `true` | 允许暴露 DSH 自己的界面；关掉则只装插件不暴露 |
| `--credentials-file F` | `<data-dir>/access.json` | 隧道地址与账号密码落盘位置（0600） |
| `--dsh PATH` / `--dsh-version V` | — / `0.1.5-rc.1` | 用现成的 dsh / 指定安装版本 |
| `--node PATH` / `--node-version V` | — / `v24.19.0` | 用现成的 node / 指定下载版本 |
| `--data-dir DIR` | XDG 数据目录 | 启动器自己的状态、日志、托管的 Node |
| `--dsh-home DIR` | `$DSH_HOME` 或 `~/.dsh` | DSH home |
| `--registry URL` | 用户 npm 配置 | 安装时用的 registry |
| `--timeout SECONDS` | `180` | 等待启动的超时 |
| `--sandbox-runner MODE` | `native` | `native` = 用 DSH 自己的 bwrap/landlock 链；`bwrap-noproc` = 用启动器自己做适配器（保留 `--ro-bind` 等文件策略，只去掉 `--unshare-pid --proc /proc`）；`auto` = 先探测，只有「完整 profile 不行、去掉这个组合行」时才装适配器 |
| `--no-sandbox` | `false` | 新会话默认用 `danger-full-access`：命令不加沙箱包装、审批也关掉。等价于 `--env DSH_PERMISSION_MODE=danger-full-access`；调用方自己指定了 `DSH_PERMISSION_MODE` 时不覆盖（只 warn） |
| `--force` | `false` | 强制重装 node 与 dsh |

## 重启与登录态

- **固定 endpoint 时保留真实 Host**：`--endpoint dsh-browser` 会被展开成
  `preserveHost: true` + `dsh web --trusted-host dsh-browser.<piko域名>`。这样 cookie 的 authority 是稳定的
  公网域名（实测 payload `"authority":"dsh-browser.clauded.tools.yicoson.cn"`），重启/换端口都不会掉登录态；
  代价是域名必须固定（`--trusted-host` 不支持通配符）。
- **没有固定 endpoint 时复用端口**：authority 退化为 `127.0.0.1:<port>`，于是 `up` 会读上次记录的
  `localUrl` 并继续用同一个端口（`--port` 显式给值优先；被占则退回随机并 warning）。
- **launch token 每次都换**：URL 里那个 `?token=` 是进程级的，旧链接 401 是正常的；
  `dsh-piko-remote status`（或 `access.json`）里有当前带 token 的地址。访问一次换到 30 天 cookie 之后，
  就不再需要 token 了。

## 一台机器只能有一个实例

DSH 把会话放在 `$DSH_HOME/sessions`，一个会话同时只归一个进程所有。**两个实例共用一个
DSH home 会互相抢会话**：Web 客户端要列出斜杠指令时会先 resume 当前会话，抢输的那个拿到
`SessionAlreadyOwnedError`，于是指令目录加载失败——`/` 菜单空白、`/compact`「执行失败」，
而服务端一条日志都没有（因为根本没有 turn 开始）。

所以 `up` 把 profile 当成自己的：启动前会**先停掉同一 profile 的上一个进程**（并说明原因），
如果机器上还有别的 profile 的 dsh 在跑，会警告一句（它们同样共享这个 home 时，会话会抢）。
`status` 也会列出这些进程。想同时跑两个实例，就给它们不同的 `--dsh-home`。

```bash
dsh-piko-remote up --dsh-home ~/.dsh-b      # 第二实例：独立 home，互不干扰
```

## 沙箱开关

启动器不替换执行器插件，只改**默认策略模式**（`DSH_PERMISSION_MODE`）：宿主组合里
唯一的 bash 执行器 `@deepseek-ai/dsh-bash-sandbox` 在 `danger-full-access` 下会直接
交给本地执行器，等于「不走沙箱」。为什么不能干脆换成
`@deepseek-ai/dsh-bash-local`：`permission` 预设插件会拒绝没有 `sandboxMode` 的执行器，
而 `fs-sandbox` / `api-workspace-files` / deliverables 界面都注入 `sandboxPolicy`。
各层控制点、以及宿主需要什么能力（`bwrap` 的 PID namespace + `/proc`，或启用了
`landlock` 的 LSM）见 [README 的「沙箱与权限」](../README.md#沙箱与权限命令到底怎么跑)。

容器里两条链都跑不起来时，还有第三条路——**沙箱适配器**：DSH 的 `sandbox` 行支持
`runnerCommand`，而它会拿到和 bwrap 一模一样的 profile 参数，所以启动器可以自己当这个
runner，把 `--unshare-pid --proc /proc` 这两项过滤掉再 exec bwrap：

```bash
dsh-piko-remote up --sandbox-runner bwrap-noproc   # 或 auto
```

文件策略（`--ro-bind / /`、`--dev /dev`、`--tmpfs /tmp`、`--bind <workdir>`）原样保留，
所以「工作区外不可写」依然成立（实测写 `/etc` 得到 `Read-only file system`，DSH 也能正确
识别成沙箱拒绝）；丢掉的是 PID namespace 隔离——被沙箱的命令本来就和 DSH 同用户，
杀掉 DSH 进程这件事不需要靠 PID 隔离来防。适配器走 `syscall.Exec`，不额外留一层进程。

## 安全

- `--expose-dsh-ui` 默认开，因为这就是这个工具的用途。**凭证默认只有 DSH 的 `?token=` 一层**：
  每次启动一个 launch token，用带 token 的 URL 访问一次换一个 30 天签名 cookie
  （绑定 Host、HMAC-SHA256、密钥存在 `~/.dsh/.credentials.yaml`），此后请求必须带它。
  代价是 **URL 即密码**（历史/Referer/聊天里都可能泄露，cookie 无 `Secure`、30 天、无账号维度），
  想加第二层就 `--basic-auth`。
- 隧道账号密码由插件随机生成，落在 `--credentials-file`（0600），日志里不出现。
- API token（`?token=...`）每次启动都会变，`status` 会重新读出来。

## 发行方式

启动器不随 npm 包发布（`files` 只包含 `piko-expose-*`），因为它内嵌了同平台的
`piko-expose`，体积翻倍。取用方式：

```bash
# 源码构建（同时产出 piko-expose 和 dsh-piko-remote）
scripts/build-helper.sh
# 或从 release 下载 dsh-piko-remote-<os>-<arch>
```

内嵌的意义：拿到单独一个启动器二进制，也能把 helper 补给从 GitHub 装的插件。
如果编译时没有内嵌（例如直接 `go build ./launcher`），它会退回到「自身旁边」
找 `piko-expose-<os>-<arch>`，再不行就明确报错并告诉你用 `scripts/build-helper.sh`。
