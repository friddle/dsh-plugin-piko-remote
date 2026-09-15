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

**2. DSH web 支持账号密码吗？不支持，但有一层每次启动随机的 token 鉴权。**

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

所以本镜像叠两层：

```
ingress ─► auth-proxy :8080 ─► dsh web 127.0.0.1:3080
           │ Basic Auth（固定账号密码，给人用）
           └ 捕获 dsh 的 boot token；文档请求被 dsh 401 时透明 303 到 /?token=…
```

`docker/auth-proxy.mjs`（零依赖）负责：Basic Auth 校验 → 原样转发（保留原始
Host，给 dsh 的 browser-trust fence 看）→ 遇到 dsh 的 401 且是文档导航
（`/` 或 `Accept: text/html`）就跳到 `/?token=<token>`，浏览器拿到 cookie 后就正常了；
`/api/*` 与 WebSocket 升级不做重定向，保持真实状态码。
`entrypoint.sh` 从 dsh 的启动输出里抓这个 token，并把它作为 `DSH_WEB_TOKEN`
交给反代。

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
node .build/proxy-test.mjs   # 12 项：401/200、Host 透传、healthz、WS 升级、token bootstrap
```

## 目录

```
deploy/
├── docker/
│   ├── Dockerfile        镜像定义（工具链 + dsh）
│   ├── auth-proxy.mjs    Basic Auth + dsh token bootstrap + WebSocket 反代（0 依赖）
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
| `/` 不带账号 | `401` |
| `/` 带账号、无 dsh session | `303` + `Location: /?token=…` |
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
| `dsh-bi-interface-auth` | `AUTH_USER` / `AUTH_PASS` | 环境变量 | Basic Auth 账号密码 |

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
- 账号：`friddle`
- `*.management.code27.co` 的 A 记录已存在（指向 `10.10.32.12`），不用另外配 DNS

## 验证

```bash
kubectl --kubeconfig ~/.kube/config_bi -n management get pod,svc,ingress
kubectl --kubeconfig ~/.kube/config_bi -n management logs deploy/dsh-bi-interface | tail
cert=$(kubectl --kubeconfig ~/.kube/config_bi -n management get certificate dsh-bi-interface-management-tls -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
echo "cert ready=$cert"

# 不带账号 -> 401；带账号 -> 303 到 /?token=…，跟随后 200
curl -s -o /dev/null -w '%{http_code}\n' https://dsh-bi-interface.management.code27.co/
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -u friddle:'<密码>' https://dsh-bi-interface.management.code27.co/
curl -sL -c /tmp/jar -b /tmp/jar -o /dev/null -w '%{http_code}\n' -u friddle:'<密码>' https://dsh-bi-interface.management.code27.co/

# 容器里的工具链 + kubeconfig 是否真的通
kubectl --kubeconfig ~/.kube/config_bi -n management exec deploy/dsh-bi-interface -- \
  bash -lc 'go version; kubectl get nodes | head -3; mysqldump --version; dsh --version'
```

## 外网入口（已通，全链路实测）

`https://dsh-bi-interface.management.code27.co` 现在从办公网直连可用，实测结果：

| 请求 | 结果 |
| --- | --- |
| `/` 不带凭据 | `401`（反代的 Basic Auth） |
| `/` 带账号密码 | `303 → /?token=…`，跟随后 `200` + SPA（27724 bytes） |
| `/?token=…` 带账号密码（wget/curl） | `200` + SPA |
| 带 session 的 `/api/*` | 非 401（session 生效） |
| 带 session 的 `/api/remote.mux` WebSocket | `101 Switching Protocols`（穿过外层网关 + ingress） |
| 不带 session 的 WebSocket | `401` |

```
# 两条都能拿到 SPA；带账号密码就够了，token 反代自己会引导
wget -O /tmp/ui.html --user=friddle --password='<密码>' https://dsh-bi-interface.management.code27.co/
wget -O /tmp/ui.html --user=friddle --password='<密码>' "https://dsh-bi-interface.management.code27.co/?token=<token>"
curl -sL -c /tmp/jar -b /tmp/jar -u friddle:'<密码>' https://dsh-bi-interface.management.code27.co/
```

注意：**`?token=` 不是替代账号密码的东西**。它是 dsh 自己的 per-boot session token
（每次重启都变，见 `kubectl logs` 里 `dsh web: http://…/?token=…`），单独拿它访问
会在反代这层被 `401` 挡掉；带上账号密码后，token 由反代自动完成引导。

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
  https://dsh-bi-interface.management.code27.co/     # 期望 401
```

返回 401 就说明 ingress/证书/后端都正常，问题在集群外那一跳。


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

## 已知限制

- **单副本 + `Recreate`**：harness 持有长连接 session，不做多副本；更新会短暂中断
  （Pod 重建后 dsh 的 token 也会换新，浏览器刷新即可）。
- **工作区绑定在单个节点**：见上一节，`hostPath` 跟着 `nodeSelector` 走，
  换节点要手动把目录准备好或改用 local PV。
- **dsh 的 token 每次重启都变**：这是 dsh 的设计，人不用管 —— 反代会自动
  引导浏览器走一遍 `/?token=`。代价是浏览器地址栏会短暂出现一次 `?token=…`。
- **Basic Auth 靠浏览器弹窗**：浏览器对同源 WebSocket 握手会复用 Basic 凭据
  （Chrome/Firefox 都如此，冒烟测试也覆盖了带 cookie 的 101）。要接 SSO 就把
  反代那层换掉，或者在 ingress 上做 `auth-type: basic`。
- `--host 0.0.0.0` 的限制没被绕过：dsh 仍然只监听 loopback，只有反代对外。
- 容器内是 root，且挂着集群 admin 证书、节点上的项目目录 —— 这是「让 harness
  能干活」的代价，请注意这个 URL 谁能访问。
