# 在远程主机上跑 DSH + 本插件

这份文档记录了一次**实际验证过**的远程部署：在一台没有图形界面的 Linux 主机上
安装 `dsh`，装本插件，把 DSH Web 界面通过 piko 隧道暴露到公网，并拿到可访问的
地址与随机账号密码。

验证环境：Ubuntu 24.04 + Node v24.19.0 + `@deepseek-ai/dsh@0.1.5-rc.1`，
插件 `0.2.0`，piko 服务端 `https://clauded.friddle.me`（子域名模式）。

## 0. 前置条件

- 目标主机能出网（只需要出站 443，不需要公网 IP、不需要开端口）。
- 服务端已经具备：
  - 泛域名 DNS `*.<SUBDOMAIN_BASE>` 指向 piko 服务端；
  - 覆盖 `*.<SUBDOMAIN_BASE>` 的证书；
  - `SUBDOMAIN_BASE=clauded.friddle.me`（默认值）。

  用这条命令确认（应返回 400，表示确实路由到了 piko upstream）：

  ```bash
  curl -s -o /dev/null -w '%{http_code}\n' https://probe.clauded.friddle.me/piko/v1/upstream/probe
  ```

- 目标主机上有 Node（`^22.19.0 || >=24`）。低于这个范围 `npm i -g` 会装不上。

## 1. 安装 dsh

```bash
export PATH=$HOME/.nvm/versions/node/v24.19.0/bin:$PATH   # 或任意 >= 24 的 node
npm i -g @deepseek-ai/dsh@0.1.5-rc.1 pnpm
dsh --version
pnpm --version
```

`pnpm` 是必须的：`dsh plugin add` 会在 profile 目录里跑它。

## 2. 建一个 profile

不要直接改默认 profile，从自带的 `web` 模板生成一个：

```bash
dsh --profile piko --from-default-profile web --dump-config >/dev/null
ls ~/.dsh/profiles/piko
```

`--dump-config` 只打印组合结果然后退出，正好用来「只初始化、不启动」。

## 3. 装插件（用 tarball，不要用目录）

```bash
cd ~/dsh-plugin-piko-remote
scripts/build-helper.sh          # 需要 Go；或者把交叉编译好的 bin/ 放进来
npm pack                         # 生成 dsh-plugin-piko-remote-0.2.0.tgz（含 bin/）
dsh plugin --profile piko add ~/dsh-plugin-piko-remote/dsh-plugin-piko-remote-0.2.0.tgz
dsh plugin --profile piko add @deepseek-ai/dsh-tools@0.1.5-rc.1
```

两个坑，都踩过：

1. **用目录装会变成 `link:`**，Node 会从插件的真实路径解析裸 import，于是
   `@deepseek-ai/schemastery`、`@deepseek-ai/dsh-tools` 全都找不到，启动时报
   `ERR_MODULE_NOT_FOUND`。用 `npm pack` 出来的 tarball 安装才会在 profile 的
   `node_modules` 里放一份真实拷贝。
2. **`@deepseek-ai/dsh-tools` 要单独装**。它是本插件的 peerDependency，而 profile
   默认 `autoInstallPeers: false`；它是普通 npm 包（没有 `dsh.bundle`），所以
   `dsh plugin add` 只把它装成依赖、不会加进 `bundles`。

装完确认插件是**真实目录**而不是软链，并且带二进制：

```bash
ls -ld ~/.dsh/profiles/piko/node_modules/dsh-plugin-piko-remote
ls -la ~/.dsh/profiles/piko/node_modules/dsh-plugin-piko-remote/bin
```

## 4. 配置

写进 profile 的 `cordis.patch.yml`（后面的 patch 会**整体替换**该行的 config，
所以这里要写全）：

```yaml
# ~/.dsh/profiles/piko/cordis.patch.yml
- id: piko-remote
  config:
    remote: https://clauded.friddle.me
    endpointPrefix: dsh
    basicAuth: true
    urlMode: subdomain
    preserveHost: false        # 把 Host/Origin/Referer 本地化，省掉 --trusted-host
    allowDshUiExpose: true
    autoExpose: true
    defaultTtlMinutes: 480
    credentialsFile: /home/friddle/dsh-access.json
```

`credentialsFile` 是给「没有模型在环里」的自动暴露准备的：URL 和随机账号密码会以
`0600` 权限写进去，日志里只有路径，不出现 URL 和密码。

## 5. 启动

```bash
cat > ~/start-dsh.sh <<'SH'
#!/usr/bin/env bash
export PATH=$HOME/.nvm/versions/node/v24.19.0/bin:$PATH
cd $HOME
exec dsh --profile piko --no-open
SH
chmod +x ~/start-dsh.sh
tmux new-session -d -s dsh "bash ~/start-dsh.sh > ~/dsh-run.log 2>&1"
sleep 30
tail -5 ~/dsh-run.log      # 里面有 dsh web 的本地地址和 ?token=...
cat ~/dsh-access.json      # 里面有公网地址和 Basic Auth 账号密码
```

## 6. 验证

```bash
U=$(python3 -c 'import json;print(json.load(open("'$HOME'/dsh-access.json"))["remoteUrl"])')
R=$(python3 -c 'import json;d=json.load(open("'$HOME'/dsh-access.json"));print(d["authUser"]+":"+d["authPass"])')
T=$(grep -o 'token=[A-Za-z0-9_-]*' ~/dsh-run.log | head -1 | cut -d= -f2)

curl -s -o /dev/null -w 'no auth:   %{http_code}\n' "$U"                 # 401
curl -s -o /dev/null -w 'with auth: %{http_code}\n' -u "$R" "$U?token=$T" # 303 -> / (设置 cookie)
curl -s -L -u "$R" -c /tmp/cj -b /tmp/cj -o /dev/null -w 'follow:    %{http_code}\n' "$U?token=$T"  # 200
```

WebSocket（DSH 的流式通道，必须 101）：

```bash
curl -s -i -N --http1.1 -u "$R" -b /tmp/cj \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: x3JJHMbDL1EzLkh9GBhXDw==' \
  "$U/api/remote.mux" | head -3
```

## 7. 拿到给用户的地址

DSH Web 自己还有一道 token 门（`?token=...`，每次启动都会变），所以完整地址是：

```
https://<endpoint>.<base>/?token=<dsh 启动时打印的 token>
Basic Auth: <authUser> / <authPass>
```

token 从 `~/dsh-run.log` 里的 `dsh web: http://127.0.0.1:3080/?token=...` 那一行读。
重启 DSH 后 token 会变，Basic Auth 账号密码和 endpoint 也会变（autoExpose 每次随机）。

## 8. 停止 / 清理

```bash
tmux kill-session -t dsh        # 关掉 DSH；插件 dispose 会顺手杀掉 piko-expose
ps aux | grep piko-expose       # 应该没有残留
```

## 备注

- 想真正在远端对话，还需要给远端 DSH 配模型 key（本插件只负责把界面暴露出去）。
- `remote_expose` / `remote_status` / `remote_close` 三个工具在远端 DSH 里同样可用，
  也就是说你可以直接让远端那个 agent 自己开洞、报地址、关洞。
