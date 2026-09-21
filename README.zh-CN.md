# knot

[English](./README.md)

[![ci](https://github.com/zbysir/knot/actions/workflows/ci.yml/badge.svg)](https://github.com/zbysir/knot/actions/workflows/ci.yml)
[![docker](https://img.shields.io/docker/v/bysir/knot?logo=docker&label=docker%20hub)](https://hub.docker.com/r/bysir/knot)

一个数据面走 **VLESS + Reality** 的小型组网工具。每个节点分一个固定内网 IP，
接入只需要一条 `docker run` 加一个 token。

业务直接连 `10.88.0.2:5672`，两端都不知道中间有隧道。而从外面看，
每台机器上只是跑着一个普通的 HTTPS 网站 —— 因为那本来就是真的。

```
                     ┌──────────────────────┐
                     │  中继                │
                     │  knot head  :8080    │  控制面，不在流量路径上
                     │  knot node  :443     │  Reality；非隧道流量
                     └──────┬───────┬───────┘  回落到一个真站点
                    Reality │       │ Reality
              ┌─────────────┘       └─────────────┐
              │                                   │
     ┌────────┴────────┐                 ┌────────┴────────┐
     │  叶子 A         │                 │  叶子 B         │
     │  对外零端口     │                 │  对外零端口     │
     └─────────────────┘                 └─────────────────┘
```

## 为什么是这个组合

三个需求同时成立时，选择其实很窄：

1. **跨境私有内网** —— 固定内网 IP 互访，业务不用挂代理、不用改协议
2. **无单点冗余** —— 两台之间的物理路径断了仍要能通，且不吊死在某个中继上
3. **流量不可识别** —— IP 池小，封一个少一个，宁可慢也不能被 DPI 认出是 VPN

第 3 条淘汰了 WireGuard / Tailscale（载荷里有明文协议特征），也淘汰了裸 VMess。
第 1 条淘汰了纯点对点隧道 —— 单独跑 xray 不叫组网。
剩下的答案是 **Reality 做传输 + TUN 做组网**。

### 为什么不直接用 xray？

问得好，而且这问题其实是反的：**Reality 是 xray 发明的**，sing-box 那边是移植。
所以 knot 跑的本来就是 xray 的协议，区别只在于用哪个实现来承载。

| | xray-core | sing-box |
|---|---|---|
| VLESS + Reality | ✅ **原创** | ✅ 移植 |
| XTLS-Vision | ✅ **原创** | ✅ |
| fallback 回落 | ✅ **更强** —— 能按 SNI / ALPN / path 分流 | ✅ 够用 |
| 路由规则 | ✅ | ✅ |
| 健康探测 + 故障切换 | ✅ `observatory` + `balancer` | ✅ `urltest` |
| **TUN** | ❌ **没有** | ✅ 内置 |

决定性的是最后一行。xray-core 不带 TUN inbound —— 生态里都是外挂
`tun2socks` 或 `hev-socks5-tunnel`，图形客户端也是自己在外面裹一层 TUN。

**没有 TUN 这就只是代理，不是组网。** 业务必须能直接打开 `10.88.0.2:5672` ——
RabbitMQ 客户端、数据库驱动这些根本没有代理配置项。必须有东西持有 mesh 网段、
把包交给隧道，而用 xray 的话那就是**第二个**子进程。

所以换过去换来的是更成熟的 Reality 实现和更灵活的 fallback，代价是多一个
运行部件。不划算。

> 这是按写作时的 xray-core 说的。真要动手前先去看一眼当前 release。

第三条路是彻底去掉子进程：
[`sing-tun`](https://github.com/sagernet/sing-tun) 管网卡，配一个 Reality 库
（比如 [`utls`](https://github.com/metacubex/utls)）。这条记在
[DESIGN-relay.md](./DESIGN-relay.md) 里，**有意推迟** —— sing-box 的 CLI 和
配置 schema 是稳定接口，它的 Go API 不是。

把 sing-box 放在进程外还有个当初没预料到的好处：它是 GPL-3.0，
而调用一个独立二进制不会把这个许可传染到 knot 自己身上。

## 组件

| 组件 | 作用 |
|---|---|
| `knot head` | 控制面：节点注册、路由策略、令牌、Web 面板。**不在流量路径上** |
| `knot node` | 每台机器上的 agent：拿 token 接入，同步配置，托管 sing-box |
| `knot connect` | 客户端：连进网络但不成为节点，无 TUN，把本机端口接到网络里的地址上 |
| sing-box | 数据面：Reality 收发、TUN、以及带健康检查的中继故障切换 |

sing-box 以**子进程**方式跑在同一个容器里，不做 library 嵌入 —— 它的 Go API
在版本间会变，而 CLI 和配置 schema 是稳定接口。升级只需要改 Dockerfile 里
一个版本号。

### 在 sing-box 之上做了什么

sing-box 是引擎，knot 是把 N 台机器变成一张受管网络的那部分。
**不是 fork** —— 容器里装的是上游原版二进制，锁了版本，没打任何补丁。

**1. 配置编译器**（`internal/sb`，约 300 行）。sing-box 要的是每台机器一份手写
JSON，而且这些文件互相耦合：

- 中继的 `users` 必须列**全部**节点的 UUID —— 漏一个，那个节点握手失败，
  报的却是 `processed invalid connection`，指向完全错误的方向
- 每个叶子要为每个可能拨的中继生成 outbound 加路由规则
- 多个中继要包进 `urltest` 组
- 路由规则**不能**加 `inbound` 过滤，否则中继转发的流量落到 `final: direct`，
  中继会去拨自己
- 走中继的 UDP 必须显式 block，否则每个数据报都变成一条
  `socks5: request rejected, code=7`

这些每一条都是实际踩过的坑，现在全部编码在生成器里。加一台机器不用碰任何配置。

**2. 身份与密钥的生命周期**（`internal/head`）。Reality 密钥对、shortID、UUID、
node key 集中生成，按需下发。包括一个不读 sing-box 源码根本发现不了的细节：
**x25519 私钥必须 clamp**。sing-box 是拿存储的字节直接用的，没 clamp 的私钥
推导出的公钥和你分发出去的那个对不上。

**3. 双向中继**（`internal/relay` + `node/relayd`，约 700 行）。
**这是 sing-box 完全没有的部分。** VLESS 单向，中继拨不回叶子，
`叶子A → 中继 → 叶子B` 根本走不通。knot 在 sing-box 已建好的 Reality 隧道里
叠一层 yamux，让叶子复用自己拨出去的连接做双向通信。这是协议，不是配置技巧 ——
也正是叶子能保持零开放端口的原因。

**4. agent 生命周期**（`internal/node`）。配置用 ETag 轮询，替换前先过
`sing-box check`，所以坏配置永远不会让节点失去数据面。响应里的三样东西 ——
sing-box 配置、`/etc/hosts` 块、中继计划 —— 分别比对分别应用，所以一次变更只付
它真正动到的代价：新节点加入会更新每个中继的对端表，但不打断任何一条会话。
sing-box 配置真变了才换进程，而且**先等旧进程被回收**再起新的；子进程的存活由
独立的守护逻辑负责，不依赖 head。对端名字维护在 `/etc/hosts` 里。

外加 Web 面板：节点表、路由矩阵、令牌。

## 叶子永远不开端口

sing-box 的 VLESS/Reality 是严格单向的：只能客户端拨服务端。照搬的话中继
**拨不回**叶子，`叶子A → 中继 → 叶子B` 走不通。

给每个叶子都配 endpoint 能解决，但那等于每台机器都开一个入站隧道口，
违背整个方案的出发点。

所以叶子**复用自己拨出去的那条连接做双向多路复用** —— DERP 就是这么干的：

```
叶子A ──Reality──> 中继 <──Reality── 叶子B
       <═════════        ═════════>
       同一条连接上双向多路复用
```

叶子开机就在每个能拨到的中继上「安家」，保持一条空闲会话。中继要把流量推给谁，
就在那条现成的会话上开一个新 stream。**叶子只有 `127.0.0.1:9998` 一个本地监听。**

中继侧的 `:9997` 也**只绑在自己的 mesh 地址上**（如 `10.88.0.1:9997`），
公网够不到 —— 对外唯一开着的端口始终只有 443。会话认证是拿 head 下发的
node key 哈希比对，不是「非空即通过」。

多路复用用 yamux，跑在 sing-box 已经建好的 Reality 隧道里 —— 所以
`internal/relay/` 里没有一行 TLS 代码。推导过程见
[DESIGN-relay.md](./DESIGN-relay.md)。

## 两个必须知道的限制

**1. `ping` 不通。** 这是**代理型 mesh**，不是真 L3 VPN。TCP 完全正常
（HTTP、数据库、消息队列都没问题），但 VLESS 承载的是流，不是原始 IP 包。
验证连通性用 `nc` / `curl`。

**2. 经中继转发的只有 TCP。** 中继会话搬运的是流不是数据报，走中继的 UDP 会被
直接丢弃（直连的 UDP 正常）。实践中唯一会撞上这条的是另一套 overlay：
`tailscaled` 会把 knot 的 TUN 地址当成 WireGuard 端点每 5 秒探一次，
它自己会切走。

## 跑起来

### 1. 控制面

```bash
docker run -d --name knot-head --restart=always \
  -p 8080:8080 \
  -e KNOT_PASSWORD=<面板密码> \
  -v knot-head:/var/lib/knot \
  bysir/knot:latest head
```

打开 `http://<head>:8080` 登录。

> 密码用 `KNOT_PASSWORD` 设，**不要用 `knot passwd`**。那个子命令直接写状态文件，
> 正在跑的 server 会把它覆盖掉 —— `knot passwd` 只在 head 停着的时候有意义。

> 面板和节点接入接口共用这个端口。**暴露前放到反向代理后面加 TLS**，
> 或者用 `KNOT_TLS_CERT` / `KNOT_TLS_KEY` 让它自己终结。
> 能连上它的人可以发接入令牌、读到所有节点的 Reality 私钥。

也可以用 [deploy/docker-compose.yml](./deploy/docker-compose.yml) /
[deploy/swarm-stack.yml](./deploy/swarm-stack.yml) —— 见
[deploy/README.md](./deploy/README.md)，里面讲了 swarm 上两个会**静默失败**的替换。

### 2. 接节点

面板上生成令牌，然后在每台机器上：

```bash
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=https://head.example.com \
  -e KNOT_TOKEN=<令牌> \
  -e KNOT_NAME=$(hostname) \
  bysir/knot:latest node
```

`KNOT_TOKEN` 只有首次需要，之后节点用自己的 key 认证。

**要让这台机器能被别人拨入（即成为中继），多加一个 endpoint：**

```bash
  -e KNOT_ENDPOINT=relay.example.com:443
```

head 会自动生成它的 Reality 密钥对和 shortId，并把公钥下发给需要拨它的节点。

### 3. 配路由

不配就是默认策略：**能直连就直连，否则任选一个中继。** 大多数情况这就够了。

要指定的话，面板的路由矩阵里每一格对应一个有序对，填**有序**的备选路径列表：

- 排在前面的优先
- 填多个时自动做健康探测，挑第一条活着的
- 留空 = 默认策略

> **空路径列表不是「用默认」**，而是「没有可用路径」，会直接切断这一对。
> 「用默认」是整条规则不存在。

### 让节点经 mesh 访问 head

节点接入之后就不再需要 head 的公网地址了 —— 它可以走 mesh。这样 head 的公网
端口就可以彻底关掉。

```bash
# 1. 先正常接入，用一个这台新机器确实够得着的地址
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=http://<公网地址>:8080 \
  -e KNOT_TOKEN=<令牌> \
  bysir/knot:latest node

# 2. 起来并拿到 mesh 地址后，改指向 head 的 mesh 地址重建。不用再带 KNOT_TOKEN
docker rm -f knot-node
docker run -d --name knot-node --restart=always \
  --network host --cap-add NET_ADMIN --device /dev/net/tun \
  -v /etc/hosts:/etc/hosts -v knot-node:/var/lib/knot \
  -e KNOT_HEAD=http://10.88.0.1:8080 \
  bysir/knot:latest node
```

`KNOT_HEAD` 会覆盖接入时记下的地址，所以第 2 步只是改个环境变量。

启动日志会是下面这样，**每一行都是预期的**：

```
knot: head address changed http://<公网地址>:8080 -> http://10.88.0.1:8080
knot: initial sync failed (... context deadline exceeded)   <- mesh 还没起来
knot: starting from cached config                            <- 所以用上次的配置
knot: relay: session to 10.88.0.1:9997 up                    <- mesh 起来了
                                                             <- 下一轮轮询成功
```

**前提是这个节点至少成功接入过一次。** 没有缓存配置就没有东西能把 mesh 拉起来，
把一台全新机器直接指向 mesh 地址是死锁：head 要 mesh，mesh 要配置，配置要 head。

由于只有中继是所有节点都够得着的，**head 应该和中继放在一起** ——
两者的可达性要求完全相同，所以同机部署是零成本的。

## 客户端（Mac App）

有时候一台机器要用到这张网，但**没有理由成为它的成员**：它只需要本机的某几个
端口能读到网络里的某几个地址 —— 就是 `ssh -L`，只不过隧道是 Reality。

本机连线上数据库是一种用法，一台服务器只要访问网络里的一个端口、却不该被别人
拨到，也是一种。用途不重要，重要的是这台机器**连进来但不加入**。

`knot connect` 就是第三种角色，它把原本绑在一起的三个属性拆开了：

| | 有网内地址 | 能被别人拨 | 参与转发 |
|---|---|---|---|
| 中继 | ✅ | ✅ | ✅ |
| 叶子 | ✅ | ✅（靠自己拨出去的会话反向推） | ❌ |
| **客户端** | **❌** | **❌** | ❌ |

客户端不分配 VIP，不进任何节点的 hosts 和路由表，也不在中继的 `PeerKeys` 里 ——
中继**没有地方能查到它**，所以推不了流给它。这不是客户端的自觉，是结构上做不到。

它也**不建 TUN 虚拟网卡**，所以不需要 root、不装 utun、不动路由表，
**不和 Tailscale 或公司 VPN 打架**（Mac 上这才是真正的麻烦）。退出就什么都不剩。

代价是它只能走**预先配好的端口转发**，不能用 IP 直接访问整个网段。对「连线上
库看一眼」这个场景，这正是够用的形状。

### 签发和吊销为什么不打扰任何人

中继的 Reality 用户表在 sing-box 配置文件里，改它就要重启 sing-box，
当时全网隧道都会断。所以**客户端凭证不进那张表**。

那张表里只多了一个**固定不变**的条目 `knot-client`，所有客户端共用它。
它是一道门，不是钥匙 —— 中继的路由规则把这个用户锁死在一个目的地上：

```jsonc
// 生成的中继配置，这两条在所有目的地规则之前
{ "auth_user": ["knot-client"], "ip_cidr": ["10.88.0.1/32"], "port": [9997], "outbound": "direct" },
{ "auth_user": ["knot-client"], "action": "reject" }
```

拿到这个共享 UUID，你能做的只有敲 knot 自己的端口。**真正的身份在门后校验**：
客户端发 `Hello{节点ID, Key}`，relayd 拿 `ClientKeys` 比对 —— 而那张表走的是
**relay plan**，`applyPlan` 原地换掉，不重启任何东西。

于是：

- **签发** = plan 里多一个哈希。中继下次轮询（10 秒）生效，零重启。
- **吊销** = 少一个哈希。除了不让重连，`setPlan` 还会**立刻关掉在线会话** ——
  `Auth` 只在握手时跑一次，不主动踢的话，已经连着的笔记本能一直用到它自己断线。
- 全程中继的 sing-box 配置**一个字节都没变**，别的节点更是毫不知情。

### 装

两样东西都要：`sing-box` 做传输，knot 本体做别的。

```bash
brew install sing-box
```

**命令行**（面板在浏览器里开）：

```bash
curl -fsSL https://github.com/zbysir/knot/releases/latest/download/knot_darwin_arm64 -o /usr/local/bin/knot
chmod +x /usr/local/bin/knot
knot connect
```

**Mac App**：从 [Releases](https://github.com/zbysir/knot/releases) 下 `Knot.app.zip`，
拖进「应用程序」，然后解除一次隔离标记：

```bash
xattr -dr com.apple.quarantine /Applications/Knot.app
```

> 这个 App 只有临时签名，没有 Apple 公证（那需要 $99/年的开发者账号）。
> **浏览器下载的文件**会被打上 `com.apple.quarantine`，双击会弹「Apple 无法验证…」。
> 上面那条命令解除一次就好。
>
> 注意这和「是不是 App」无关：同一个命令行二进制，用浏览器下载一样会被拦，
> 用 curl 下载就不会 —— 区别在下载工具有没有打标记，不在文件本身。

**自己构建**（需要 Xcode 命令行工具，`xcode-select --install`）：

```bash
./dist/macos/build-app.sh        # 产出 dist/macos/build/Knot.app
cp -R dist/macos/build/Knot.app /Applications/
```

本地构建出来的没有隔离标记，不需要上面那条 `xattr`。
想连 sing-box 一起打进去（目标机器不装 brew）：`./dist/macos/build-app.sh --with-singbox`
—— 注意 sing-box 有 81 MB，App 会从 15 MB 涨到 ~96 MB，而且只有一个架构。

装好在 Launchpad 里搜 **Knot**。这是一个普通的 Mac App：Dock 里有图标，
⌘Q 退出。**关掉窗口不会断隧道** —— 转发继续跑，点 Dock 图标或 ⌘0 把窗口叫回来。

bundle 里是两个可执行文件：`Knot` 是 AppKit 外壳（窗口 + 菜单），
`knot-helper` 是它拉起来的 knot 本体。外壳退出时会结束 helper；
反过来 helper 也盯着父进程，外壳崩了它自己也走，不会留下一个占着端口、
却没有任何窗口能关掉它的孤儿进程。

不想用 App 的话，同一个二进制在终端里跑是一样的：

```bash
knot connect [--ui 127.0.0.1:8765] [--data ~/.knot] [--singbox PATH] [--no-open]
```

App 也会把启动参数透传给 helper，可以开一个配置独立的实例：

```bash
open -a Knot --args --data ~/.knot-staging --ui 127.0.0.1:8766
```

### 在服务器上当客户端

同样的东西在 Linux 上也跑 —— 一台机器只需要访问网络里的一个端口、却不该被网络
里的任何东西拨到，这是个正当用法，不是退而求其次。

```bash
curl -fsSL https://github.com/zbysir/knot/releases/latest/download/knot_linux_amd64 -o /usr/local/bin/knot
knot connect --no-open          # 没有浏览器可开，也不该开
```

面板只监听回环，所以要看的话走 SSH 隧道：`ssh -N -L 8765:127.0.0.1:8765 <host>`。
转发配好之后存在 `~/.knot/state.json`，进程起来就自动恢复，可以直接丢给 systemd。

> **升级顺序：先 head，再中继。** 客户端要等中继也升级完才能接入 —— 旧的中继
> 二进制不认识 `ClientKeys`，会在握手时拒绝它。
>
> 升级 head 这一下**会让中继重启一次 sing-box**（配置里多了那道门和两条规则），
> 当时的隧道会重连。这是一次性的：之后签发和吊销客户端就都是零重启了。
> 叶子的配置不受影响。

### 用

1. head 面板 →「客户端」→ 填有效期 → **签发客户端凭证**
2. 把它给出的 head 地址和令牌填进 App
3. 「新建转发」，一条转发就是：**本地端口 → 哪台机器 → 它去连的地址:端口**

「哪台机器」可以是网络里任何节点，中继和叶子都行 —— 它就是替你去拨的那台。
两种日常用法其实是同一件事：

| 想连的东西 | 哪台机器 | 它去连的地址 |
|---|---|---|
| pg 上的数据库 | `pg` | `127.0.0.1:5432` |
| hk 那边 VPC 里的托管库 | `hk` | `rds-prod.internal:3306` |

关键是**地址由那台机器去解析和连接，本机不需要能访问它** ——
内网域名和 VPC 私网地址直接可用，域名原样带过去，不会走你本地
（咖啡馆 wifi 给的）DNS。

然后把数据库客户端指向 `127.0.0.1:<本地端口>` 就行。

### 几件要知道的事

- **只有 TCP。** 整条链路运的是流不是包；到叶子的 UDP 在配置里是显式丢弃的。
- **这份凭证等于「那台机器能访问的一切」。** 客户端的权限是「让某个节点替它去连
  某个地址」，所以凭证丢了等于那些节点的网络暴露了。用完就在面板里吊销，
  签发时也建议带上有效期。（想再收紧的话，中继侧按 `auth_user` 做目的地 ACL
  是现成的扩展点，relayd 也已经知道是哪个客户端在问。）
- **转发只监听 127.0.0.1**，本地端口必须 ≥1024（低端口要 root，那就把这个 App
  不需要权限的最大好处弄没了）。
- 增删一条转发**不会重启 sing-box**：转发监听在 knot 自己手里，sing-box 只负责
  传输，其它转发上正在跑的连接不受影响。

## 和已有站点共用 443

Reality **不能**挂在 nginx/traefik 后面 —— 它必须自己握 TLS 才能伪装。
但可以反过来：Reality 占 443，把非隧道流量转发给你原有的 web 服务。

```
:443 → Reality
        ├── 握手认证通过 → 隧道
        └── 其它一切     → 转发到该节点的「回落」地址
```

在面板里把节点的**回落**设成本机真实站点（比如 traefik 挪过去的
`127.0.0.1:8443`），**伪装 SNI** 设成那个站点的域名。这样主动探测你的 443，
看到的是一个**真实可用、证书有效**的网站 —— 因为它本来就是。
这比借用 `www.microsoft.com` 可信得多。

### ⚠️ 回落站点的证书链不能太大

Reality 要把回落站点的握手通过一个定长缓冲区转发一遍。证书链太大会撑爆它，
而且失败现象和原因完全对不上：**连接是在 Reality 认证成功之后才死的**，
报一个笼统的 `REALITY: processed invalid connection`。
要开 `log.level: trace` 才看得见真正的原因。

定下来之前先量一下：

```bash
openssl s_client -connect HOST:443 -servername HOST -tls1_3 </dev/null |
  awk '/BEGIN CERT/,/END CERT/' | wc -c
```

| 站点 | 字节 | 能用 |
|---|---|---|
| 一般的单域名站点 | ~1800 | ✅ |
| `dl.google.com` | 2732 | ✅ |
| `www.microsoft.com` | 8273 | ❌ |

## 环境变量

### `knot head`

| 变量 | 作用 |
|---|---|
| `KNOT_LISTEN` | 监听地址，默认 `:8080` |
| `KNOT_PASSWORD` | 面板密码，启动时应用 |
| `KNOT_TLS_CERT` / `KNOT_TLS_KEY` | 自己终结 TLS |
| `KNOT_DATA` | 状态目录，默认 `/var/lib/knot` |

### `knot node`

| 变量 | 作用 |
|---|---|
| `KNOT_HEAD` | head 地址 —— **必填** |
| `KNOT_TOKEN` | 接入令牌，只有首次需要 |
| `KNOT_NAME` | 节点名，默认取 hostname |
| `KNOT_ENDPOINT` | 对外 `host:port` —— **设了才是中继** |
| `KNOT_DATA` | 状态目录，默认 `/var/lib/knot` |
| `KNOT_SINGBOX` | sing-box 路径，默认 `sing-box` |

## 运行时行为

- 节点每 **10 秒**拉一次配置（`KNOT_POLL`）；sing-box 配置、`/etc/hosts` 块、中继计划三样分别按
  字节比对、分别应用，所以**加一个节点既不动 sing-box 也不断任何中继会话**
- sing-box 配置真变了才换进程，而且**先等旧进程被回收**再起新的。信号是异步的，
  tun 设备要到内核把进程彻底拆完才释放，早一步起新进程就会被
  `TUNSETIFF: device or resource busy` 打死。**不用 SIGHUP**：sing-box 支持它，
  但 `instance.Close()` 释放不干净 tun inbound，同进程重建撞同一个错误，
  而 SIGHUP 分支会丢掉 Close 的错误直接退出 —— 等于一次配置变更把 sing-box 弄死
- sing-box 的存活由独立的守护逻辑负责，head 不可达时子进程死了照样能拉起来 ——
  中继上的 head 是**穿过**这个子进程访问的，没有别的东西能发现它死了
- 新配置先经 `sing-box check` 校验才替换，坏配置不会让节点失去数据面
- head 说不认识这个节点（401）时，agent 会用 `KNOT_TOKEN` 重新接入，
  而不是拿着一个已经失效的凭证一直轮询
- mesh 变更之后的一切都卡在轮询间隔上：中继没轮询到就不可能接受一个新节点。
  30 秒的时候意味着新节点要被中继拒绝最多半分钟，所以间隔是 10 秒 —— 一次轮询就是
  一个条件 GET，命中 304 不碰磁盘。`LastSeen` 故意不落盘：每次轮询都盖一次时间戳，
  以前等于把整个 state 文件重写一遍
- knot 自己的日志带时间戳（和 sing-box 同格式），并且剥掉 sing-box 的颜色转义，
  所以一次 `docker logs` 读起来是一份日志而不是两份
- Reality 对每个未认证的 `:443` 连接都回 `processed invalid connection`，级别是
  ERROR，而公网中继是被持续扫描的 —— 我们自己的机器六分钟内 53 个不同源 IP。
  这些行被折叠成每 5 分钟一条摘要。**是计数不是丢弃**：几十个 IP 各来一次是
  互联网，一个 IP 反复来就是你自己某个节点的 Reality 公钥过期了，摘要会点名
- 中继会话断了自动重连，退避封顶 60 秒 —— 中继停一小时再回来，
  一分钟内自动恢复
- 转发前先看目标中继的会话活没活：主中继挂了直接跳下一条，
  不用先付一次拨号超时
- **head 挂了不影响转发**：节点继续用手里的配置跑，只是拿不到变更
- 节点名写在 `/etc/hosts` 的 knot 标记块里，`<name>.knot` 和裸名都能解析

> 名字解析只对宿主机有效。**容器有自己的 `/etc/hosts`**，读不到 ——
> 容器里请直接写 mesh IP。

## 开发

```bash
go build ./...      # go.mod 声明 1.24.7，Go 1.21+ 会自动切换工具链
go vet ./...
docker build -t knot .
```

状态是单个 JSON 文件（`$KNOT_DATA/state.json`），可以直接读和手改。

小内存机器建议交叉编译 —— 从源码编 sing-box 需要的内存超过 2GB 机器能给的：

```bash
docker buildx build --platform linux/amd64 -t <registry>/knot:latest --push .
```

## 许可

[MIT](./LICENSE)

knot 建立在 [sing-box](https://github.com/SagerNet/sing-box)（数据面）和
[yamux](https://github.com/hashicorp/yamux)（中继多路复用）之上。
