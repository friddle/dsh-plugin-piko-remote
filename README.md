# dsh-plugin-piko-remote

把本机端口通过 [piko](https://github.com/andydunstall/piko) 隧道暴露到远程服务器，
并把公网访问地址交还给用户和模型。调用方式与
[opencode-piko-remote](https://github.com/friddle/opencode-piko-remote) 完全一致：
每个 endpoint 一条**只出站**的上游连接，前面挂一个反向代理指向本地目标。

它是 [gotty-piko](https://github.com/friddle/gotty-piko) 服务端的插件侧对偶——
服务端负责按 endpoint 路由，插件负责把本地端口接上去。

## 状态

🚧 早期骨架。当前只有加载自检，隧道逻辑见 [PLAN.md](./PLAN.md)。

## 安装

```bash
dsh plugin add github:friddle/dsh-plugin-piko-remote
# 或本地开发
dsh plugin add /absolute/path/to/dsh-plugin-piko-remote
```

`dsh plugin add` 会在 profile 目录跑 pnpm，然后按「已安装包是否声明
`dsh.bundle`」把包写进 `dsh.profile.bundles`。**改完插件记得重启 DSH。**

安装后在「设置 → 插件 → 插件列表」里应该能看到 `piko-remote` 这一行。

## 卸载

```bash
dsh plugin remove dsh-plugin-piko-remote
```

## 计划中的能力

| 工具 | 作用 |
|---|---|
| `remote_expose` | 暴露一个本地端口，返回 `https://<endpoint>.<base>/` |
| `remote_status` | 列出当前隧道 |
| `remote_close` | 关闭隧道 |

以及一个设置页卡片（地址、复制、二维码、启停）。

## 安全

暴露 DSH 自己的 Web 界面等于**把本机 Agent 的控制权交给任何拿到该 URL 的人**。
所以计划里 Basic Auth 默认开、TTL 默认非空、`allowDshUiExpose` 默认关。
详见 PLAN.md 的风险表。

## License

MIT
