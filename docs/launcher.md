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
   `@deepseek-ai/*` peer 装上（profile 默认 `autoInstallPeers: false`，
   不补的话插件会以 `ERR_MODULE_NOT_FOUND` 加载失败）。
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
| `--ttl MINUTES` | `480` | 隧道存活时间，`0` = 不过期 |
| `--basic-auth` | `true` | 隧道 Basic Auth（`--expose-dsh-ui` 时不允许关） |
| `--expose-dsh-ui` | `true` | 允许暴露 DSH 自己的界面；关掉则只装插件不暴露 |
| `--credentials-file F` | `<data-dir>/access.json` | 隧道地址与账号密码落盘位置（0600） |
| `--dsh PATH` / `--dsh-version V` | — / `0.1.5-rc.1` | 用现成的 dsh / 指定安装版本 |
| `--node PATH` / `--node-version V` | — / `v24.19.0` | 用现成的 node / 指定下载版本 |
| `--data-dir DIR` | XDG 数据目录 | 启动器自己的状态、日志、托管的 Node |
| `--dsh-home DIR` | `$DSH_HOME` 或 `~/.dsh` | DSH home |
| `--registry URL` | 用户 npm 配置 | 安装时用的 registry |
| `--timeout SECONDS` | `180` | 等待启动的超时 |
| `--force` | `false` | 强制重装 node 与 dsh |

## 安全

- `--expose-dsh-ui` 默认开，因为这就是这个工具的用途；但它**要求 Basic Auth 开着**，
  两者同时关闭会被直接拒绝——那等于把本机的 Agent 无凭证挂到公网。
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
