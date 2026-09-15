# DSH image + BI interface deployment

`dsh-bi-interface.management.code27.co` 跑在 `config_bi` 集群的 `management`
命名空间里，镜像是这个目录自己构建的 —— 不是官方镜像。

## 先回答两个前提问题

**1. DSH 有官方 Docker 镜像吗？没有。**

- 上游仓库 `deepseek-ai/deepseek-harness` 根目录没有 `Dockerfile` / compose /
  k8s 目录（只有 `.gitlab-ci.yml`），npm 包 `@deepseek-ai/dsh` 也没有关联的
  容器包，ghcr / Docker Hub 都没有官方 tag。
- 上游只有一个 *Idea: official Docker image / containerized deployment* 的讨论，
  也就是说「容器化」目前是社区自己做的事。

**2. DSH web 支持账号密码吗？不支持；它只有每次启动随机生成的 token。**

实测 `@deepseek-ai/dsh@0.1.5-rc.1`：

- `dsh web` 的参数只有 `--host` / `--port` / `--trusted-host` / `--no-open`，
  没有任何 `--auth` / `--user` / `--password`。
- 它**默认要求一个登录态**：启动时打印
  `dsh web: http://127.0.0.1:3080/?token=<随机 43 字符>`，此后不带 token 的请求
  一律 `401 dsh web authentication required; reopen the URL printed by dsh web.`
  （`/`、`/index.html`、`/api/*` 都是 401），带 token 访问返回 **303 + Set-Cookie**，
  之后靠 cookie 放行。
- 这个 token 每次重启都变，没法当人的账号用；而且 `--host 0.0.0.0` 会被直接拒绝：
  `error: --host 0.0.0.0 is intentionally not supported yet for safety: it would
  expose remote code execution to the network`。

所以本镜像叠两层，**两层都是 token，没有 HTTP Basic 弹窗**：

```
浏览器 ─► auth-proxy :8080 ─► dsh web 127.0.0.1:3080
          │ ① 自己的登录 token → 签名 HttpOnly Cookie（30 天，滑动续期）
          └ ② 捕获 dsh 的 boot token；文档请求被 dsh 401 时透明 303 到 /?token=…
```

① 是给人用的：**只登录一次**。② 是 dsh 自己每次重启都会换的 session token，
由 `entrypoint.sh` 从启动日志里抓出来交给反代，人不用管。

> 为什么不用 Basic Auth：浏览器不会像人期望的那样"记住"Basic —— 任何 XHR /
> WebSocket 的 401 都可能再弹一次框，SPA 场景下就是"每个页面都问一次"。
> 所以默认 `DSH_AUTH_MODE=token`：不发 `WWW-Authenticate`，未登录的文档请求
> 直接 303 到登录页；Basic 只作为可选的脚本后门保留（`both` / `basic`）。

## 登录方式（token + Cookie 会话）

| 项 | 值 |
| --- | --- |
| 登录 token | Secret `dsh-bi-interface-auth` 的 `token` 键（`apply.sh` 首次随机生成，之后复用不轮换） |
| 会话 | `dsh_auth=v1.<exp>.<HMAC>` 签名 Cookie，`HttpOnly` + `SameSite=Lax`（https 时加 `Secure`） |
| 有效期 | `DSH_SESSION_TTL`，默认 30 天；**滑动续期**（过半 TTL 后自动续，常用就不用再登） |
| 退出 | `https://<host>/__logout` |
| 登录页 | `https://<host>/__login`（只输 token 一个框） |
| 一次性登录链接 | `https://<host>/?dsh_token=<token>`（登录后立刻把参数从 URL 抹掉并 303） |

dsh 那一层的引导是**反代在服务端做的**：反代启动后按需用 `Host`（authority）去
`dsh web` 换一次它自己的 `dsh-auth-<random>` 会话 cookie，缓存起来注入后续请求。
好处是浏览器地址栏永远不出现 dsh 的 `?token=`，`-H "Authorization: Bearer <token>"`
这类脚本也能**首跳就拿到 200**（不需要 cookie jar 跟跳转）。

⚠️ 踩过的坑：**dsh 的会话是按 authority 签名的**（cookie payload 里带
`"authority":"<host>"`），所以引导时必须用客户端真实的 Host——用 `Host: 127.0.0.1`
去引导再注入给 `Host: <域名>` 的请求会全部 401。反代因此按 authority 建 Map 缓存
（单测的假上游也照这个语义实现，否则测不出这类问题）。

```bash
# 取 token
kubectl -n management get secret dsh-bi-interface-auth -o jsonpath='{.data.token}' | base64 -d; echo

# 脚本用法（不再需要 -u）
TOKEN=$(kubectl -n management get secret dsh-bi-interface-auth -o jsonpath='{.data.token}' | base64 -d)
curl -H "Authorization: Bearer $TOKEN" https://dsh-bi-interface.management.code27.co/       # 200
wget --header="Authorization: Bearer $TOKEN" -O /tmp/ui.html https://dsh-bi-interface.management.code27.co/
# 或者换一次 cookie 然后复用 jar
curl -sL -c /tmp/jar -b /tmp/jar "https://dsh-bi-interface.management.code27.co/?dsh_token=$TOKEN"
```

轮换 token（会让所有人重新登录）：

```bash
AUTH_PASS='<密码>' AUTH_TOKEN="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')" ./deploy/apply.sh
```

模式（`DSH_AUTH_MODE`，在 `k8s/deployment.yaml` 里）：

| 模式 | 行为 |
| --- | --- |
| `token`（默认） | 只认 Cookie / `Authorization: Bearer`；没有任何 HTTP auth，**永远不会弹窗** |
| `both` | 再加上 Basic（给 `curl -u`；curl 是抢先发送凭据的），仍不发 challenge |
| `basic` | 旧行为：所有请求都 Basic challenge（只有 `wget --user/--password` 这类非要 challenge 的客户端才需要） |
| `off` | 完全不鉴权（前面已有别的网关时） |


## 镜像内容

基础镜像 `node:24-bookworm`（满足 dsh 的 `engines: ^22.19.0 || >=24`），另外装：

| 用途 | 内容 |
| --- | --- |
| 运行时 | Node.js 24、pnpm、`@deepseek-ai/dsh@0.1.5-rc.1`（全局） |
| 编译 | Go 1.27.1、build-essential、pkg-config、automake/autoconf/libtool、python3 + venv/pip |
| 集群 | kubectl v1.33.9 |
| 数据库 | mariadb-client（同时提供 `mariadb`/`mysql` 和 **`mysqldump`**） |
| 日常 | git、openssh-client、vim、less、jq、unzip/zip/xz/tar、rsync、tmux、htop、tree、file、socat、curl/wget、dig/ping/net-tools、tini、locales(UTF-8) |

Go 既在 `ENV PATH` 里，也写进 `/etc/profile.d/go-path.sh` —— `bash -lc`
这种登录 shell 会重置 PATH，只靠 ENV 会找不到 `go`。

web profile 在**构建期**就 `dsh --profile web --dump-config` 预热好了，
`$DSH_HOME/profiles/web` 已经烤进镜像，Pod 冷启动不需要联网装依赖。

版本都是 build arg（`NODE_IMAGE` / `GO_VERSION` / `KUBECTL_VERSION` / `DSH_VERSION` /
`NPM_REGISTRY`），要升级直接 `TAG=0.2.0 DSH_VERSION=... ./deploy/build.sh`。

### 构建环境上的几个坑（都已处理）

本机 Docker 是 OrbStack，构建 VM 里**连不上 Docker Hub**，所以：

- Dockerfile 里**故意不写** `# syntax=docker/dockerfile:1` —— 那行会强制从
  Docker Hub 拉 frontend 镜像，直接 `Bad Gateway`；本文件不需要非默认 frontend。
- 基础镜像默认走 `docker.m.daocloud.io/library/node:24-bookworm`（`--build-arg
  NODE_IMAGE=...` 可换）。局域网镜像 `dockermirror.service.code27.cn/library/node:24-bookworm`
  虽然能解析，但拉那个 211MB 的层会 `short read: unexpected EOF` 截断，不要用。
- apt 源在构建时被改写成 `mirrors.aliyun.com`（`deb.debian.org` 通但慢）；Go 用
  阿里云镜像、失败回落 `go.dev/dl`；npm 默认 `registry.npmjs.org`，可用
  `NPM_REGISTRY=https://registry.npmmirror.com` 覆盖。
- `docker build` 会被 Docker client 配置里 `proxies.default -> localhost:7897`
  拖累（daemon 在 VM 里够不到宿主 localhost），`build.sh` 显式把 `*_proxy`
  build-arg 清空；同一个坑在 `docker run` 时也会把 `http_proxy=localhost:7897`
  注进容器，冒烟测试因此显式清空这些变量（否则容器里 curl 127.0.0.1 会走代理）。

反代那层不依赖 Docker，可以直接在本机跑单元验证：

```bash
node .build/proxy-test.mjs   # 23 项：登录/会话/过期/篡改/模式差异/Host 透传/WS/dsh token bootstrap
```

## 目录

```
deploy/
├── docker/
│   ├── Dockerfile        镜像定义（工具链 + dsh）
│   ├── auth-proxy.mjs    token→Cookie 会话 + dsh token bootstrap + WebSocket 反代（0 依赖）
│   └── entrypoint.sh     播种凭据 → 起 dsh(loopback) → 抓 token → 起反代
├── k8s/
│   ├── deployment.yaml   Deployment（无 Secret 明文，只引用名字）
│   ├── service.yaml      ClusterIP :80 -> :8080
│   └── ingress.yaml      dsh-bi-interface.management.code27.co + TLS
├── build.sh              构建 + 冒烟测试 + 推送
├── apply.sh              用本机真实文件建 Secret 并 apply（DRY_RUN=1 可预演）
└── .secrets/             暂存改写过的 kubeconfig（已 gitignore）
```

## 构建 & 推送

```bash
./deploy/build.sh              # build(linux/amd64) + 冒烟 + push
PUSH=0 ./deploy/build.sh       # 只构建 + 冒烟，不推
```

冒烟测试起一个容器（带生产 Host 头），验证完整链路：

| 请求 | 期望 |
| --- | --- |
| `/` 无会话（浏览器导航） | `303` → `/__login`，且**不发 Basic challenge** |
| `GET /__login` | `200` 登录表单 |
| `POST /__login` 正确 token | `303` + `Set-Cookie: dsh_auth=v1.…` |
| 跟随跳转 + cookie jar | `200`，正文是 SPA HTML |
| 带 session 请求 `/api/state` | 非 401（session 生效） |
| 带 session 的 WebSocket 升级 | `101` |

并打印容器里 `go/git/kubectl/mysqldump/node/vim` 的版本。

推送用两个 tag：本机推 `registrylan.service.code27.cn/app/dsh-bi-interface:<tag>`
（局域网别名），Deployment 里写 `registry.code27.co/app/dsh-bi-interface:<tag>`
（集群内名字），是同一个仓库。

## 凭据：都从本机真实文件导入

`./deploy/apply.sh` 直接读本机文件建 Secret，**Secret 明文不落仓库**：

| Secret | 来源 | 挂载点 | 容器内用途 |
| --- | --- | --- | --- |
| `dsh-bi-interface-kubeconfig` | `~/bin/ssh_bi`（`root@47.90.211.213:2222`）的 `/root/.kube/config`，server 改写成 `https://kubernetes.default.svc:443` | `/root/.kube/config` | 容器内 `kubectl` 直接用（admin 客户端证书 + CA 都在里面，集群内 apiserver 证书含 `kubernetes.default.svc` SAN，校验通过） |
| `dsh-bi-interface-ssh` | `~/.ssh/{id_ed25519,id_ed25519.pub,config,known_hosts}` | `/root/.ssh`（`defaultMode: 0600`） | git over ssh / ssh 跳板 |
| `dsh-bi-interface-dsh-credentials` | `~/.dsh/{.credentials.yaml,settings.yaml}` | `/etc/dsh-credentials`，entrypoint 拷进 `$DSH_HOME` | 容器里的 dsh 真能调模型（`DEEPSEEK_API_KEY` 等） |
| `dsh-bi-interface-auth` | `AUTH_USER` / `AUTH_PASS` / `AUTH_TOKEN`（apply.sh 生成，复用不轮换） | 环境变量（`token` 键 = 登录 token） | Cookie 会话的登录 token；user/pass 仅 `both`/`basic` 模式用 |

注意 kubeconfig 挂的是 **ssh_bi 那台机器上的** admin 配置（`172.18.0.1:6443`，
就是这台 bi 控制面），不是本机 `~/.kube/config` —— 后者指向本地 orbstack，
容器里根本用不了（`apply.sh` 自己也显式 `export KUBECONFIG=config_bi`，
免得手滑部署到 orbstack）。因为 Deployment 就在这个集群里，地址换成 apiserver 的
Service 域名最稳。要加别的集群，改 `apply.sh` 里的 `SSH_SRC`/`DSH_SRC` 附近那段
`--from-file` 列表，或在 `deploy/.secrets/` 放好再 `--from-file`。

## 部署

```bash
DRY_RUN=1 AUTH_PASS='<密码>' ./deploy/apply.sh   # 预演，什么都不改
AUTH_PASS='<密码>' ./deploy/apply.sh             # 真正应用
```

脚本会：建 4 个 Secret → apply Service/Deployment/Ingress → 等 rollout →
打印 Pod/Service/Ingress。

当前部署：

- 集群/命名空间：`config_bi` / `management`
- Ingress：`dsh-bi-interface.management.code27.co`（`nginx` class，
  `cert-manager.io/cluster-issuer: letsencrypt-cloudflare-issuer`，
  TLS secret `dsh-bi-interface-management-tls`，WS 超时 3600s）
- 登录：`?dsh_token=<token>` 或 `/__login` 输 token（`both`/`basic` 模式下 `friddle` + 密码仍可用）
- `*.management.code27.co` 的 A 记录已存在（指向 `10.10.32.12`），不用另外配 DNS

## 验证

```bash
kubectl --kubeconfig ~/.kube/config_bi -n management get pod,svc,ingress
kubectl --kubeconfig ~/.kube/config_bi -n management logs deploy/dsh-bi-interface | tail
cert=$(kubectl --kubeconfig ~/.kube/config_bi -n management get certificate dsh-bi-interface-management-tls -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
echo "cert ready=$cert"

# 无会话 -> 303 到登录页；用 token 换 cookie 后 -> 200
TOKEN=$(kubectl --kubeconfig ~/.kube/config_bi -n management get secret dsh-bi-interface-auth -o jsonpath='{.data.token}' | base64 -d)
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' https://dsh-bi-interface.management.code27.co/          # 303 /__login
curl -sL -c /tmp/jar -b /tmp/jar -o /dev/null -w '%{http_code}\n' "https://dsh-bi-interface.management.code27.co/?dsh_token=$TOKEN"   # 200
curl -s -H "Authorization: Bearer $TOKEN" -o /dev/null -w '%{http_code}\n' https://dsh-bi-interface.management.code27.co/            # 200

# 容器里的工具链 + kubeconfig 是否真的通
kubectl --kubeconfig ~/.kube/config_bi -n management exec deploy/dsh-bi-interface -- \
  bash -lc 'go version; kubectl get nodes | head -3; mysqldump --version; dsh --version'
```

## 外网入口（已通，全链路实测）

`https://dsh-bi-interface.management.code27.co` 现在从办公网直连可用，实测结果：

| 请求 | 结果 |
| --- | --- |
| `/` 无会话（浏览器） | `303` → `/__login`，响应里**没有** `WWW-Authenticate`（不会再弹框） |
| `/?dsh_token=<token>` | `303`（抹掉参数、下发 Cookie）→ 跟随后 `200` + SPA |
| `Authorization: Bearer <token>` | `200` |
| 无会话的 `/api/*` | `401` JSON（不是 challenge） |
| 带 session 的 `/api/remote.mux` WebSocket | `101 Switching Protocols`（穿过外层网关 + ingress） |
| 不带 session 的 WebSocket | `401` |

```bash
TOKEN=$(kubectl --kubeconfig ~/.kube/config_bi -n management get secret dsh-bi-interface-auth -o jsonpath='{.data.token}' | base64 -d)
# 浏览器：打开下面这个链接登录一次（30 天滑动续期）；或打开 / 走登录表单
echo "https://dsh-bi-interface.management.code27.co/?dsh_token=$TOKEN"
# 脚本
curl -H "Authorization: Bearer $TOKEN" -o /tmp/ui.html https://dsh-bi-interface.management.code27.co/
wget --header="Authorization: Bearer $TOKEN" -O /tmp/ui.html https://dsh-bi-interface.management.code27.co/
```

注意区分两个 token：**`dsh_token` 是给人用的登录 token**（稳定，存在 Secret 里）；
dsh 启动日志里的 `?token=` 是它自己的 per-boot session token（每次重启都变），
由 entrypoint 抓给反代，人不需要也不应该用它。

### 如果哪天又变成不可达（历史现象，留作排障参考）

部署当天从这台 Mac 访问时，外层网关（`10.10.32.12:443`，`Server: openresty`）
在 TLS 握手阶段就拒绝：

```
LibreSSL: error:1404B458:SSL routines:ST_CONNECT:tlsv1 unrecognized name
（openssl: ssl alert number 112 = unrecognized_name）
```

当时**并非我们独有**：`codiehub-bi.management.code27.co`、`bijob-bi.management.code27.co`
同样是 000，而 `codiehub-bi-prod` / `community-poll-prod` / `ai-ability-prod` /
`*-mp2` 都是 200 —— 说明该网关按 host 静态配置。后来这个名字就通了（网关侧更新/
生效），所以现在不需要加别名。若再次出现同样的 `unrecognized_name`，
先按这个对照表确认是不是网关那一层，而不是集群出了问题：

```bash
# 集群内（绕开外层网关，直接问 ingress controller 的 SNI）
kubectl --kubeconfig ~/.kube/config_bi -n management exec deploy/dsh-bi-interface -- \
  curl -sk -o /dev/null -w '%{http_code}\n' \
  --resolve dsh-bi-interface.management.code27.co:443:10.106.177.237 \
  https://dsh-bi-interface.management.code27.co/     # 期望 303（跳登录页）
```

返回 303/200 就说明 ingress/证书/后端都正常，问题在集群外那一跳。


## 工作区 / 持久化（节点本地目录）

这个集群没有 StorageClass，所以持久化用的是**节点本地目录**：

| 项 | 值 |
| --- | --- |
| 宿主机目录 | `/root/data/project`（在独立大盘 `/dev/nvme1n1p1`，492G，剩 ~169G） |
| Pod 内路径 | `/root/data/project`（`hostPath`，`type: Directory`） |
| DSH 工作区 | `WORKSPACE_DIR=/root/data/project` —— dsh 的 cwd 就在这里，重启/重建 Pod 数据都在 |
| 节点绑定 | `nodeSelector: kubernetes.io/hostname=iz0xi7yxr2llsjyhd7mvksz`（硬固定；hostPath 数据只在这台）。`apply.sh` 会先校验该节点存在且未被 cordon，避免 Pod 一直 Pending |
| 目录创建 | `apply.sh` 会 ssh 到 `$SSH_HOST` 执行 `mkdir -p`，跑之前先保证它存在 |

`/root/project` 已经软链到 `/root/data/project`（老路径继续可用）：

```
lrwxrwxrwx /root/project -> /root/data/project
```

搬运过程（1.4G / 99518 个条目）用的是 rsync + `--itemize-changes` 空输出校验，
校验通过才删源目录，条目数搬前搬后一致；`.claude`/`bi`/`codie-bi-system`/`deployment`/`redash`
原属主与 mtime 保留。

**换节点时**：`hostPath` 不会跟着走。要么在新节点上也建好目录并改
`deployment.yaml` 的 `nodeSelector`，要么把这个目录做成 `local` PV + PVC
（更 k8s 的做法，Pod 会被 PV 的 nodeAffinity 自动约束）。

**权限注意**：宿主机上属主是 `sybran`(uid 1000)，Pod 里 `1000` 显示成 `node`；
容器以 root 运行，读写没问题。别把 `/root/data` 整个挂进去 —— 里面是节点的
containerd/k8s/kafka 数据。

## Pod 里的 git 凭据（clone 用）

Pod 的 `/root/.ssh/id_ed25519` 来自 Secret（本机 `~/.ssh`），公钥：

```
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINdwuAvvMG1qPXe0B1SHRBobEYY9A1rIBeJBW3eduMn7 friddle@friddle-copperdeMacBook-Air.local
```

指纹 `SHA256:4c0bUkjJQkPIzPORQoGi6WThwiQmqSUEYmqPoQwlMdk`。实测已经能过 GitHub：

```bash
kubectl -n management exec deploy/dsh-bi-interface -- cat /root/.ssh/id_ed25519.pub
kubectl -n management exec deploy/dsh-bi-interface -- ssh -o BatchMode=yes -T git@github.com
# Hi friddle! You've successfully authenticated, ...
```

要换 key：改 `~/.ssh/id_ed25519` 后重跑 `apply.sh`（会重建 `-ssh` Secret），
再 `kubectl rollout restart deploy/dsh-bi-interface`。

## 插件：chrome-driverless（浏览器自动化）

镜像里**预装了** `dsh-plugin-chrome-driverless`（构建期 `dsh plugin --profile web add
github:friddle/dsh-plugin-chrome-driverless#v0.1.0`），Pod 起来就能用，不需要联网装。

BI 集群里已经有一个 chrome 服务（`deployment/chrome-driverless-mp2`，19 天），所以插件
**只做 HTTP 客户端**，自己不建容器（Pod 里没有 docker socket）：

| 项 | 值 |
| --- | --- |
| 服务 | `chrome-driverless-mp2.management.svc.cluster.local`（`/health` → `{"status":"ok"}`，Pod 内 21ms） |
| 插件配置 | `baseUrl=<上面的服务>`、`manageContainer: false`、`autoStart: false` |
| 配置怎么传 | `deploy/docker/patches/chrome-driverless.yml.tmpl` → entrypoint 启动时渲染到 `/tmp/dsh-patches/chrome-driverless.yml`，以 `dsh --profile web --patch …` 传入 |
| 换服务地址 | 改 Deployment 的 `DSH_CHROME_BASE_URL` 即可，**不用重建镜像**（entrypoint 每次启动重新渲染） |
| 升级插件版本 | `TAG=0.7.0 CHROME_PLUGIN_SPEC='github:friddle/dsh-plugin-chrome-driverless#v0.2.0' ./deploy/build.sh` |

三个必须注意的点（都是踩过的坑）：

1. **必须用 `dsh --profile web --patch … <app flags>`，不能用 `dsh web --patch …`**：
   后者会报 `error: web takes none of parent --profile, --from-default-profile, --patch, …`。
2. **不能让 profile 装第二份 `@deepseek-ai/dsh-tools`**：两份拷贝 = 两个 `Symbol`，
   `ctx.tools[TOOL_RUNTIME_SCHEDULER]` 变 `undefined`，每次工具调用都挂在
   `Cannot read properties of undefined (reading 'prepare')`。web profile 的
   `autoInstallPeers: false` 让 peer 走 dsh 自带那份，Dockerfile 里还有一条
   `test ! -e …/@deepseek-ai/dsh-tools` 断言把它钉死。
3. **`profiles/` 是镜像拥有的**：持久卷不能把它永久挡住，否则镜像升级（比如这次加插件）
   永远进不去。entrypoint 每次启动用 `rsync -a --delete` 把镜像里的 profile 同步进卷，
   **sessions/storages/凭据不动**（那是卷拥有的）。所以：**插件升级/新增插件 = 重建镜像 +
   滚动更新**；会话数据不受影响。

验证（都在 Pod 内真跑过）：

```bash
K="kubectl --kubeconfig ~/.kube/config_bi -n management"
# 组合里这一行确实带上了我们的配置
$K exec deploy/dsh-bi-interface -- bash -lc 'dsh --profile web --patch /tmp/dsh-patches/chrome-driverless.yml --dump-config | grep -A5 dsh-plugin-chrome-driverless'
# 真驱动 BI 的 chrome（返回里有真 PNG 截图）
$K exec deploy/dsh-bi-interface -- bash -lc 'B=http://chrome-driverless-mp2.management.svc.cluster.local; \
  curl -s -X POST "$B/mcp" -H "content-type: application/json" -d "{\"method\":\"pw/navigate\",\"params\":{\"url\":\"https://example.com\"}}" | head -c 160'
```

⚠️ **工具要新建会话才可见**：会话的工具目录在会话创建时就定下来了，老会话里没有
`browser_*`（这是 DSH 的行为，不是插件问题）。**新建一个会话**，让它「打开 example.com
并截图」即可看到 `browser_open` / `browser_screenshot` 等 13 个工具。

## 卡顿 / "思考卡住" 排查（实测结论）

有人反馈"远程一直卡住、好几个思考都是 20 分钟前的"，本地桌面版正常。逐项量过之后：

| 假设 | 实测 | 结论 |
| --- | --- | --- |
| Pod CPU 被限流/挨饿 | `cpu.pressure some avg10=0.00`；全生命周期只 throttle 42s；用量约 0.26 核 | 排除 |
| 节点太忙（load 12.2/8 核） | 节点整机 `cpu.pressure some=73%`，但 Pod 自身 PSI=0（请求 500m 够用） | 不是我们被饿 |
| 存储慢 | fsync 4-7ms、64MB 读 11ms；容器 overlay 与 `/root/data` **同一块 NVMe** | 排除 |
| 模型 API 不通/慢 | Pod 内真 key 调用：非流式 ttfb 32ms 总 0.97s，流式 ttfb 96ms 总 1.15s，200 | 排除 |
| 本地/远程配置不同 | 两边 `cordis.patch.yml` 都是空、`.credentials.yaml`/`settings.yaml` 一致 | 排除 |
| 外层网关掐长轮询 | 3 个并发 `/plugins/events` 都跑到 90-92s 正常 200 | 排除 |
| 每次请求的网关开销 | 连接复用后 p50≈0ms（之前看到的 480ms 是 curl 每次新建 TLS 的假象） | 排除 |

剩下最像的机制：**SPA 的实时通道（`/api/remote.mux` WebSocket）被静默掐断，前端就永远停在旧状态**。
证据：出问题时 Pod 完全空闲（无 CPU、无子进程）、模型 1 秒就回，但 ingress 日志里浏览器**有约 4 分钟一个请求都没有** —— 服务端没事，是浏览器侧停了。

因此 0.5.0 起加了四件事：

1. **WS keepalive**：反代在 101 之后每 25s 往浏览器发一个空 WS ping（`0x89 0x00`），
   浏览器按 RFC 自动回 pong，字节流持续双向流动 → 中间两跳的空闲超时再也掐不断它。
   （`DSH_WS_PING=0` 关闭，`DSH_WS_PING_MS` 调间隔）
2. **完整可观测性**：`DSH_PROXY_LOG=0` 关闭，默认每个请求一行
   `[access] GET /plugins/events -> 200 90123ms host=... via=cookie`，
   WS 生命周期 `[ws] open/closed ... pings=N client->dsh=NB dsh->client=NB`，
   以及 `[dsh] session for host=...`（dsh 的 per-boot 会话引导）。
   **下次再卡，先看这三类日志**：
   - `[access]` 断了 → 浏览器/标签页/本地网络停了（服务端无责）；
   - 有 `[ws] closed` 且 `pings>0` 之后再没 `[ws] open` → 前端没重连，属前端行为；
   - 一直没有任何 `[access]` 但你在操作 → 请求根本没到集群（网关/网络）。
3. **持久化 `$DSH_HOME`**（`/root/data/dsh-home`）：以前 sessions/storages/投影缓存都在
   容器可写层，**每次滚动更新都清空**，浏览器手里的 session 立刻变孤儿 —— 现象和"卡住"一模一样。
   现在跨 Pod 重建保留（已用"写标记 → 删 Pod → 标记仍在、6 个 session 文件仍在"验证过）。
4. **bootstrap 看门狗**：dsh 若不回 `/?token=` 交换，10s 后放弃等待（否则所有请求会排在
   那个 promise 后面一起挂死）。

另外两点运营注意：

- 滚动更新/重启会立刻断开现有 WS，浏览器要刷新一次才会重连（这是前端行为，不是部署故障）。
- 节点 `iz0xi7yxr2llsjyhd7mvksz` 是控制面 + kafka/postgres/ES/chrome 混布（load 10-12）；
  单副本交互式 harness 想更稳，可考虑迁到空得多的 `iz0xi8kw308idqdw69tavxz`（把
  `/root/data/{project,dsh-home}` 一起搬），但当前 PSI 显示我们并不缺 CPU。

## 已知限制

- **单副本 + `Recreate`**：harness 持有长连接 session，不做多副本；更新会短暂中断
  （Pod 重建后 dsh 的 token 也会换新，浏览器刷新即可）。
- **工作区绑定在单个节点**：见上一节，`hostPath` 跟着 `nodeSelector` 走，
  换节点要手动把目录准备好或改用 local PV。
- **dsh 的 token 每次重启都变**：这是 dsh 的设计，人不用管 —— 反代在服务端
  换它的会话并缓存；万一 dsh 中途重启，第一个 401 会自动触发重新引导并重试一次
  （Bearer/Cookie 都不用动）。
- **登录态是 Cookie，不是 Basic**：浏览器对同源 WebSocket 握手会带上 Cookie
  （冒烟测试覆盖了带 Cookie 的 101）。`token` 模式下 HTTP auth 完全关闭；
  要接 SSO 就把反代那层换掉，或者在 ingress 上做 `auth-type: basic`。
- **token 泄露等于登录态泄露**：登录链接 `?dsh_token=…` 会短暂出现在地址栏/history
  （反代立刻 303 抹掉），只在可信环境分享；轮换用 `AUTH_TOKEN=... ./deploy/apply.sh`。
- `--host 0.0.0.0` 的限制没被绕过：dsh 仍然只监听 loopback，只有反代对外。
- 容器内是 root，且挂着集群 admin 证书、节点上的项目目录 —— 这是「让 harness
  能干活」的代价，请注意这个 URL 谁能访问。
