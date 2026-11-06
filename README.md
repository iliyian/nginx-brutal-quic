# nginx-brutal-quic

为 nginx 的 QUIC / HTTP3 栈加入 **Brutal 拥塞控制**（移植自 hysteria2）。

> 一句话：让 nginx 在丢包的跨境线路上不再崩速。2% 丢包下，CUBIC 从 53 Mbps 掉到 2.2 Mbps，
> 启用 Brutal 后保持 50 Mbps。

---

## 目录

- [这是什么](#这是什么)
- [会不会影响原有功能](#会不会影响原有功能)
- [快速开始](#快速开始)
- [配置参考](#配置参考)
- [实测数据](#实测数据)
- [原理](#原理)
- [已知限制](#已知限制)
- [架构与改动](#架构与改动)
- [构建](#构建)
- [测试](#测试)
- [维护上游](#维护上游)
- [许可](#许可)

---

## 这是什么

nginx 的 QUIC 实现里，拥塞控制只有 CUBIC 一种，而且是硬编码的：

- `ngx_quic_congestion_t` 是一个纯数据结构，不是接口
- `ngx_quic_congestion_ack()` / `_lost()` 里直接写死了 CUBIC 的增窗和乘性减
- `ngx_quic_congestion_lost()` 甚至声明为 `static`，外部无法链接
- 没有任何模块扩展点（`ngx_quic_module` 是个空壳 core module，directives 全 `NULL`）

所以 Brutal 只能以**源码补丁**的形式加入，做成模块不可能。

Brutal 的思路和 TCP 完全不同：**发送端自己声明带宽，按这个速率发，丢包当没看见。**

| | CUBIC | Brutal |
|---|---|---|
| 速率来源 | 探测网络 | 配置文件写死 |
| 丢包反应 | 窗口砍 30%，重新爬 | 不反应，继续发 |
| 丢包补偿 | 无 | ack 率下降 → 发送速率**反向上升** |
| 适用场景 | 通用 | 跨境 / 弱网 / 高丢包 |

---

## 会不会影响原有功能

**不会。这是本补丁最重要的设计约束，并且经过实测验证。**

### 默认关闭

不加 `quic_brutal_tx` 这一行，拥塞控制行为和原版 nginx 相同：

```c
/* 所有改动都是这个形状：入口加判断，命中就 goto done */
if (cg->brutal.enabled) {
    goto done;
}
/* ↓ 以下 CUBIC 代码原样保留，一行没删 */
```

`brutal.enabled` 只在连接建立时读一次配置赋值，之后是常量。未配置时它是 0，所有分支都不成立。

### 改动只增不删

对比上游 nginx 1.31.8，CUBIC 的实现代码**没有任何一行被删除或修改**，只有：

- 前置的分支判断（纯新增）
- 新文件 `ngx_event_quic_brutal.{c,h}`
- 结构体末尾追加一个成员
- 一个配置指令注册

这保证了：
1. 不配置 = 不生效，零风险
2. 未来 rebase 到新版 nginx 冲突面极小
3. 出问题可以立即回退（删掉配置行即可，不用换二进制）

### 实测回归

同一个二进制、同一条 60 Mbit / 40ms RTT 链路：

| 配置 | 吞吐 |
|---|---|
| 原始 nginx 1.31.8 | 53.73 Mbps |
| 本补丁，**未配置** | 53.93 Mbps |

差异 0.4%，在噪声范围内。

### 按需开启的范围

`quic_brutal_tx` 是 `http` / `server` 级指令，可以：

- 全站开启
- 只在某个 `server {}` 块开启（其它 server 继续走 CUBIC）

```nginx
http {
    # 默认：所有 server 都是 CUBIC

    server {
        listen 443 quic reuseport;
        server_name tunnel.example.com;
        quic_brutal_tx 20m;          # 只有这个站点用 Brutal
    }

    server {
        listen 443 quic reuseport;
        server_name www.example.com;  # 没有这一行 → 正常 CUBIC
    }
}
```

> ⚠️ 一个需要注意的 nginx 既有行为：`quic_brutal_tx` 取值来自 **listen socket 的默认 server**，
> 在 SNI 之前解析。所以多个 `server` 共享同一个 `listen` 时，会统一采用默认 server 的值。
> 这和 nginx 自己的 `quic_gso` / `quic_retry` 行为一致，不是本补丁引入的。
> 需要按站点区分时，请给不同站点使用不同的 `listen` 端口或地址。

---

## 快速开始

### 用预构建镜像

```bash
docker pull ghcr.io/iliyian/nginx-brutal-quic:latest
```

这是 `nginx:mainline` 的直接替换品：模块集、编译路径、`--with-compat` ABI 都一致。
把现有容器编排文件里的 image 换掉、重新 `up -d` 即可，配置和挂载都不用改。

```yaml
services:
  nginx:
    image: ghcr.io/iliyian/nginx-brutal-quic:latest   # ← 只改这一行
    network_mode: host
    restart: unless-stopped
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
      - ./certs:/etc/nginx/certs:ro
      - ./logs:/var/log/nginx
```

### 启用

在需要加速的 `server {}` 块里加一行：

```nginx
server {
    listen 443 quic reuseport;
    server_name example.com;

    ssl_certificate     /etc/nginx/certs/fullchain.pem;
    ssl_certificate_key /etc/nginx/certs/privkey.pem;

    http3 on;
    http3_stream_buffer_size 1m;   # 强烈建议，见下方说明
    quic_brutal_tx 20m;            # ← 就这一行

    location / {
        # 换成你自己的上游；Unix socket、h2 后端、静态文件都行
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
    }
}
```

```bash
nginx -t && nginx -s reload
```

---

## 配置参考

### `quic_brutal_tx`

| 项 | 值 |
|---|---|
| 语法 | `quic_brutal_tx <size>;` |
| 默认 | `0`（关闭，走 CUBIC） |
| 上下文 | `http`, `server` |
| 单位 | **字节/秒**，支持 `k` / `m` 后缀，**只接受整数** |
| 生效时机 | 配置重载后建立的**新连接**；已存在的连接不受影响 |

`quic_brutal_tx` 走 nginx 的 `ngx_parse_size`，后缀是 1024 进制，而且整数部分
只认数字 —— 小数点、`g` 后缀都会让 `nginx -t` 报 `invalid value`。
换算公式：**字节/秒 = Mbps × 125000**。

**常用换算：**

| 想要的带宽 | 配置值 | 字节/秒 |
|---|---|---|
| 20 Mbps | `2500k` | 2,560,000 |
| 50 Mbps | `6250000`（或 `6103k`） | 6,250,000 |
| 100 Mbps | `12500000`（或 `12207k`） | 12,500,000 |
| 200 Mbps | `25000000`（或 `24414k`） | 25,000,000 |
| 500 Mbps | `62500000`（或 `61035k`） | 62,500,000 |
| 1 Gbps | `125000000` | 125,000,000 |

> `m` = 1024×1024 = 1,048,576，所以 `20m` 是 20,971,520 B/s ≈ **168 Mbps**，
> `45m` ≈ 378 Mbps，`200m` ≈ 1.68 Gbps。写 `m` 时要自己按这个倍率折算。
>
> ⚠️ 以下写法**都会报错**：`6.25m`、`12.5m`、`62.5m`、`1g`。

**怎么选值：**

1. 实测线路可用带宽（`iperf3` 或实际下载），单位 Mbps
2. **乘 0.7**，再乘 125000，得到字节/秒
3. 观察一周，延迟正常再逐步上调

宁可填低。填低只是浪费一点带宽；填高会顶高队列延迟（见[实测数据](#实测数据)最后一组）。

### `http3_stream_buffer_size`

不是本补丁的指令，但强烈建议一起调：

```nginx
http3_stream_buffer_size 1m;
```

nginx 默认值只有 **64 KB**（`ngx_http_v3_module.c`）。XHTTP 这类单流承载整条连接的场景，
高 BDP 链路上 64 KB 缓冲区会成为瓶颈，Brutal 设再高也跑不上去。

---

## 实测数据

测试环境：单台 Ubuntu 26.04 / aarch64，`veth` 对 + `tc tbf 60Mbit` + `netem delay 20ms`（RTT 40ms），
客户端 `h3probe` 单连接 1 流，服务端返回 4 GiB 流式响应，上游走 Unix socket（排除磁盘和 TCP 干扰）。

| # | 拥塞控制 | 丢包 | 吞吐 | 说明 |
|---|---|---|---|---|
| 1 | CUBIC | 0% | **53.7 Mbps** | 基线，基本吃满 60Mbit |
| 2 | CUBIC | 2% | **2.18 Mbps** | 跌 96% |
| 3 | Brutal `45m`（≈378 Mbps，6.3×） | 2% | **50.9 Mbps** | 基本不受影响 |
| 4 | CUBIC | 10% | **0.81 Mbps** | 彻底崩 |
| 5 | Brutal `45m`（≈378 Mbps，6.3×） | 10% | **47.2 Mbps** | 吞吐是 CUBIC 的 **58 倍** |
| 6 | Brutal `200m`（≈1.68 Gbps，28×） | 0% | 55.2 Mbps | 故意大幅超发 |

**第 2 组说明了问题所在**：CUBIC 每次丢包砍掉 30% 窗口，2% 丢包意味着每 50 个包挨一刀，
窗口根本爬不起来。这是所有基于丢包的 CC 在跨境线路上的通病。

**第 6 组说明了参数敏感性**：速率填到链路容量的约 28 倍，吞吐并没有变快（55.2 vs 53.7），
但 `tc` 统计显示积压了 **988 KB / 698 个包**在队列里 —— 也就是延迟暴涨。
它被打服了，不会雪崩，但你会难受。

> ⚠️ **这组数据的配置值远高于链路容量，不能直接当作"推荐值"来理解。**
> 第 3、5 组用了 `45m`，也就是约 378 Mbps，在 60 Mbit 链路上是 6.3 倍超发；
> 第 6 组 `200m` 是 28 倍。也就是说这些"丢包下仍保持 50 Mbps"的结果，
> 是在大幅超发的前提下测出来的，并没有验证"实测带宽 × 0.7"这种合规配置的效果。
> 合规配置下的抗丢包能力还需要补测。
>
> 早期设计文档里我担心"超发会正反馈自激"。实测证伪了这一点：
> 超出容量时吞吐稳定在链路极限，只是队列堆积。但这个结论只在这个拓扑下验证过，
> 真实运营商链路上是否会有更差的表现，没有数据。

---

## 原理

### 窗口

```
cwnd = brutaltx × srtt × 2 / ack_rate
```

和 hysteria2 的 `GetCongestionWindow()` 一致。`srtt` 是 nginx 已有的平滑 RTT 采样。

`ack_rate` 是最近 5 秒的 `ack / (ack + loss)`，夹在 `[0.8, 1.0]`。样本少于 50 个时固定为 1.0。

**注意方向**：`ack_rate` 下降会让速率**上升**（除以一个小于 1 的数）。这是丢包补偿，
不是退避。hysteria2 的 `disableLossCompensation` 开关就是干这个的。

### Pacer

只有一个窗口还不够 —— nginx 原本是 `while (in_flight < window)` 背靠背灌满，
窗口一大就会瞬间喷出整个窗口的量，中间队列直接爆。

所以加了令牌桶：

```
rate  = brutaltx / ack_rate
burst = max(rate × 4ms, 10 × MTU)
```

预算不足一个 MTU 时，算出差多少毫秒能攒够，挂一个定时器再回来发。
预算不足**并不完全停止发送**：ACK-only 报文仍然会立刻发出，否则对端的发送会被我们的
令牌桶一起拖住（详见下文"两条发送路径都要扣预算"）。

**这里有个 nginx 的坑**：`ngx_add_timer()` 在 `src/event/ngx_event_timer.h:56` 有个 300ms 的合并逻辑 ——
如果新定时器的 key 距离已装载的那个不到 300ms，它**静默忽略**新值，保留旧的。

所以不能用 `qc->push`（那个已经被延迟 ACK、socket 重试、帧队列共用）。Brutal 用了自己独立的
`pace` 事件，并且每次都先 `ngx_del_timer()` 再 `ngx_add_timer()` 绕开合并。

### 路径迁移：定时器必须先摘下来

`ngx_quic_handle_path_response_frame()` 在客户端地址变化时（WiFi ↔ 4G）会执行

```c
ngx_memzero(&qc->congestion, sizeof(ngx_quic_congestion_t));
```

`pace` 事件是 `ngx_quic_congestion_t` 的成员，节点正挂在全局 timer 红黑树上。
如果直接清零：

- 树上留下一个 `left` / `right` 被清成 NULL 的节点，而 sentinel 不是 NULL，
  `ngx_event_expire_timers()` 里的 `ngx_rbtree_min()` 会一路走到 NULL 解引用
  —— **下一个事件循环 worker 就段错误**；
- 即使没炸，`pace_arm` 看到 `timer_set == 0`，会把同一个节点再插一次。

所以调用方必须在清零**之前**调 `ngx_quic_brutal_cleanup()`（关闭连接路径同理）：

```c
ngx_quic_brutal_cleanup(qc);      /* del_timer + delete_posted_event */

ngx_memzero(&qc->congestion, sizeof(ngx_quic_congestion_t));
...
ngx_quic_brutal_reset(c, qc);     /* 用 qc->conf->brutal_tx 重建 */
```

### 其它必须处理的边界

1. **`cg->mtu` 只在丢包路径更新**（`ngx_event_quic_ack.c:903`）。Brutal 永远不进丢包路径，
   mtu 会永久停在 1200。所以 Brutal 的代码一律读 `qc->path->mtu`，并每次算窗口时
   同步回 `cg->mtu`。

2. **`ngx_quic_congestion_idle()`** 在 CUBIC 下会改写 `k` / `idle_start`。Brutal 下整段跳过：
   这两个字段只被 CUBIC 的增窗逻辑读，而 Brutal 的窗口每次 ACK 都从 srtt 重算。
   （跳过本身无害，但别指望它是"防止窗口被打回初值"——窗口并不会被打回。）

3. **`ngx_quic_persistent_congestion()`** 会把窗口砸到 `2 × mtu`。这个函数恰好在
   "长时间没有 ACK" 时触发 —— 而 Brutal 的设计就是要无视这种情况，必须提前 return。

4. **`ngx_quic_get_connection(c)` 在 `ngx_quic_new_connection()` 执行期间返回 NULL**，
   因为反向指针 `qsock->quic = qc` 要到之后的 `ngx_quic_listen()` 才赋值。所以
   `ngx_quic_brutal_init()` 显式接收 `qc` 指针，不走那个宏。

### 两条发送路径都要扣预算

- **单报文路径**（`ngx_quic_create_datagrams`，`quic_gso` 关闭时的默认路径）：
  预算不足时不再直接 `break`，而是照常走一趟、只允许发出 ACK-only 报文，再挂 pacing 定时器。
  否则纯 ACK 也会被压到攒够一个 MTU 之后，对端会跟着停等。
- **GSO 聚合路径**（`ngx_quic_create_segments`）：预算在整批 `sendmsg()` **之后**才扣，
  所以循环里的检查必须问"当前已攒的 + 下一个 segment"够不够，只问 `segsize`
  会让整批（最多 64 个 datagram）凭一个 segment 的预算通过。`quic_gso` 默认关闭，
  但开了就必须是对的。

---

## 已知限制

1. **速率是死的，不会自适应。**
   hysteria2 有客户端上报带宽的机制（`Hysteria-CC-RX` 请求头，服务端取 `min(serverTx, clientRx)`）。
   这里客户端是标准 HTTP/3，不上报，所以 nginx 只能按配置写死。
   **用户换了网络环境（WiFi → 4G），这个值就错了。**

2. **不是热更新。**
   改速率要 `nginx -s reload`，且只影响 reload 之后的新连接。

3. **可能影响同机其它连接。**
   每条连接独立按配置速率发，没有全局仲裁。一条 `quic_brutal_tx 125000000`（1 Gbps）的连接
   可以饿死同机的 CUBIC 连接。这是 hysteria 这类方案的固有问题。

4. **定时器开销。**
   `ngx_add_timer` 精度是毫秒，所以 pace 定时器最快 1ms 一次。预算不足的连接大约每秒唤醒
   `max(1000, rate/mtu)` 次。适合少量大流量连接，不适合上万并发。

5. **已在 1.31.8 上验证。**
   其它版本需要确认补丁能否干净应用（CI 里会做 `patch --dry-run`）。

6. **`quic_bpf` / 多地址迁移场景未做压力测试。**
   路径迁移的重建逻辑写了（`ngx_quic_brutal_cleanup()` + `ngx_quic_brutal_reset()`），
   但只有代码路径核对，没有实战验证。

7. **不提供 RFC 9221 QUIC DATAGRAM 支持。**
   nginx 的 QUIC 栈本身就没有 DATAGRAM 帧（`NGX_QUIC_FT_*` 里没有 `0x30/0x31`，
   传输参数里也没有 `max_datagram_frame_size`）。Brutal 只是替换拥塞控制，
   数据仍然走 HTTP/3 stream。注意这和 hysteria2 的用法不同 ——
   那边 Brutal 跑在它自己的 DATAGRAM 协议上。

---

## 架构与改动

### 新增文件

```
src/event/quic/ngx_event_quic_brutal.h   数据结构 + 公开接口
src/event/quic/ngx_event_quic_brutal.c   全部 Brutal 逻辑（窗口、ack率、令牌桶、定时器）
```

### 修改文件

| 文件 | 改动 |
|---|---|
| `src/event/quic/ngx_event_quic.h` | `ngx_quic_conf_t` 加 `brutal_tx` |
| `src/event/quic/ngx_event_quic_connection.h` | `ngx_quic_congestion_t` 末尾加 `brutal` 子结构 + include |
| `src/event/quic/ngx_event_quic.c` | 连接建立时初始化；关闭时清理私有定时器 |
| `src/event/quic/ngx_event_quic_ack.c` | 4 个分流点：ack / lost / idle / persistent_congestion；acked 包计数 |
| `src/event/quic/ngx_event_quic_output.c` | pacer 门控（单报文路径 + GSO 路径 + `allow_segmentation`） |
| `src/event/quic/ngx_event_quic_migration.c` | 路径迁移：`memzero` 前先 `ngx_quic_brutal_cleanup()`，之后 `_reset()` |
| `src/http/v3/ngx_http_v3_module.c` | 注册 `quic_brutal_tx` 指令 |
| `auto/modules` | 新文件加入 `ngx_event_quic_module` 的编译列表 |

完整 diff 见 [`patches/nginx-brutal-quic.patch`](patches/nginx-brutal-quic.patch)。

### 数据结构

```c
typedef struct {
    ngx_uint_t      enabled;         /* 建立时置一次，之后是常量 */
    ngx_uint_t      ack_rate_milli;  /* 800..1000 */
    size_t          bps;             /* 字节/秒 */

    time_t          slot_sec[5];     /* 5 秒滑窗 */
    uint64_t        slot_ack[5];
    uint64_t        slot_lost[5];

    uint64_t        budget_mb;       /* 令牌桶，字节×1000 保留毫秒精度 */
    ngx_msec_t      budget_msec;
    ngx_msec_t      last_sent;

    ngx_event_t     pace;            /* 私有定时器，刻意不用 qc->push */
} ngx_quic_brutal_t;
```

独立子结构而非平铺到 `ngx_quic_congestion_t`，是为了：

- 未启用时整体全零，CUBIC 路径完全不受影响
- 未来 rebase 上游时，冲突只落在单行 guard 上

代价是路径迁移的 `ngx_memzero()` 会把 `pace` 的 `timer_set` / `rbtree` 链接一起清掉，
所以清理、重建必须成对出现（见上文"路径迁移"）。

---

## 构建

### Docker

```bash
docker build -t nginx-brutal-quic .
```

### 手动

```bash
VERSION=1.31.8
SHA256=0652b760b3b772d2ec17b47c2fc15d9a95e6fe35cd5b877eadc1a62d9bc75af1

curl -fsSL -O "https://nginx.org/download/nginx-${VERSION}.tar.gz"
echo "${SHA256}  nginx-${VERSION}.tar.gz" | sha256sum -c -
tar xzf "nginx-${VERSION}.tar.gz"
cd "nginx-${VERSION}"
patch -p1 < ../patches/nginx-brutal-quic.patch

# 按你的需要裁剪模块；下面这组和官方镜像一致
./configure \
    --prefix=/etc/nginx --sbin-path=/usr/sbin/nginx \
    --with-compat --with-threads --with-file-aio \
    --with-http_ssl_module --with-http_v2_module --with-http_v3_module \
    --with-http_stub_status_module --with-http_realip_module \
    --with-stream --with-stream_ssl_module --with-stream_ssl_preread_module \
    --with-cc-opt="-O2 -Werror -U_FORTIFY_SOURCE \
                   -Wp,-D_FORTIFY_SOURCE=2 -fstack-protector-strong" \
    --with-ld-opt="-Wl,-z,relro -Wl,-z,now"

make -j"$(nproc)"
```

> nginx 1.31 的 QUIC 需要 **OpenSSL ≥ 3.5**（走 `SSL_set_quic_tls_cbs`）或 BoringSSL/quictls。
> Debian 13 / Ubuntu 26.04 自带的 OpenSSL 3.5.x 满足要求。
>
> 加固参数（`FORTIFY_SOURCE` / `stack-protector-strong` / `relro` / `now`）和官方
> nginx 镜像保持一致。`-U_FORTIFY_SOURCE` 不能省：gcc 15 起默认预定义
> `_FORTIFY_SOURCE=3`，重复定义在 `-Werror` 下会直接把 configure 打挂。

### CI

`.github/workflows/build.yml` 在每次 push 时：

1. 下载干净的 nginx 源码，校验 SHA-256，`patch --dry-run` 验证补丁能应用
2. 多架构构建（`linux/amd64`, `linux/arm64`）
3. 推到 `ghcr.io/iliyian/nginx-brutal-quic`

---

## 测试

`tools/` 下两个小工具，不依赖 curl 的 HTTP/3 支持（大多数发行版的 curl 没编译进去）。

两个工具各自是独立的 Go module，根目录的 `go.work` 把它们串起来，
所以在仓库根目录就能直接 `go run` / `go build`。`h3probe` 的 `go.sum` 已经提交；
如果你改了 `go.mod`，跑一次 `go mod tidy` 重新生成。

### h3upstream

一次性 HTTP 上游，监听 Unix socket，返回指定大小的流式响应。
**刻意独立于任何生产服务**，避免测试污染。

```bash
go run ./tools/h3upstream -addr unix:/tmp/h3up.sock -size 4G
```

### h3probe

HTTP/3 测试客户端。单 QUIC 连接开 N 条流，报告吞吐。

```bash
go run ./tools/h3probe -url https://127.0.0.1:8443/big -n 1 -secs 10 -label BRUTAL
```

### 搭建隔离的丢包环境

不要在生产主机的 `eth0` 上直接加 `tc`。用独立 netns：

```bash
ip netns add brt
ip link add veth-h type veth peer name veth-n
ip link set veth-n netns brt
ip addr add 10.99.0.1/24 dev veth-h
ip link set veth-h up
ip netns exec brt ip link set lo up
ip netns exec brt ip addr add 10.99.0.2/24 dev veth-n
ip netns exec brt ip link set veth-n up

# 60Mbit + 20ms 单向延迟（RTT 40ms）
tc qdisc add dev veth-h root handle 1: tbf rate 60mbit burst 32kbit latency 20ms
tc qdisc add dev veth-h parent 1:1 handle 10: netem delay 20ms
ip netns exec brt tc qdisc add dev veth-n root netem delay 20ms

# 加丢包
tc qdisc change dev veth-h parent 1:1 handle 10: netem delay 20ms loss 2%

# 从 netns 里发请求
ip netns exec brt /tmp/h3probe -url https://10.99.0.1:8443/big -secs 10
```

清理：

```bash
tc qdisc del dev veth-h root
ip link del veth-h
ip netns del brt
```

### 诊断

编译时加 `--with-debug`，并在 `error_log` 里用 `debug` 级别：

```nginx
error_log /var/log/nginx/error.log debug;
```

然后可以观察：

```bash
grep "quic brutal" /var/log/nginx/error.log
```

```
quic brutal init bps:52428800 mtu:1200
quic brutal ackev acked:2 lost:0 rate:1000 win:314572
quic brutal window srtt:3 ack:1000 cwnd:314572 rate:52428800
quic brutal pace arm d:1 budget:39000
```

> ⚠️ **不要把日志加进 `ngx_quic_brutal_has_budget()`**。那个函数每发一个包调用一次，
> 我在开发时在那里打了日志，直接把吞吐从 400 Mbps 砍到 125 Mbps。

---

## 维护上游

补丁刻意做成"前置 guard"的形状，rebase 成本很低：

```bash
git clone https://github.com/nginx/nginx.git nginx
cd nginx
git checkout -b brutal <对应版本的 tag>
patch -p1 < ../patches/nginx-brutal-quic.patch
```

升级到新版本时，如果 `ngx_add_timer` 的合并逻辑、`congestion_*` 函数签名、
或 `ngx_quic_congestion_t` 布局有变化，需要重新核对：

- `a.c`: `ngx_quic_congestion_ack` / `_lost` / `_idle` / `_persistent_congestion` 的入参
- `output.c`: `ngx_quic_create_datagrams` / `_segments` 的发送循环结构
- `quic.c`: `ngx_quic_new_connection` 里 `qc->congestion` 的初始化块

---

## 参考

- [hysteria2 brutal.go](https://github.com/apernet/hysteria/blob/master/core/internal/congestion/brutal/brutal.go) — 原始实现
- [hysteria2 pacer.go](https://github.com/apernet/hysteria/blob/master/core/internal/congestion/common/pacer.go) — 令牌桶
- [RFC 9002](https://www.rfc-editor.org/rfc/rfc9002) — QUIC 丢包检测与拥塞控制
- [RFC 9438](https://www.rfc-editor.org/rfc/rfc9438) — CUBIC

---

## 许可

本仓库采用 **BSD-2-Clause**，全文见 [`LICENSE`](LICENSE)。

补丁是 nginx 的衍生作品，因此 nginx 原有的版权声明随补丁一并保留；
无论以何种方式分发编译产物，nginx 的 BSD-2-Clause 条款都同样适用。

`ngx_event_quic_brutal.{c,h}` 是新写的代码，版权归本仓库作者，
文件头标注了 `Copyright (C) 2026 iliyian`。

第三方组件的署名集中列在 [`NOTICE`](NOTICE)：nginx、apernet/hysteria，
以及 `tools/` 里用到的 quic-go。

Brutal 算法移植自 [apernet/hysteria](https://github.com/apernet/hysteria)，
该项目以 **MIT** 许可发布（Copyright 2023 Toby）。MIT 与 BSD-2-Clause 兼容，
本仓库在 `LICENSE` 里附上了 MIT 全文与版权声明，以履行署名义务。
