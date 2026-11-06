# KVM/QEMU 虚拟机安全迁移讨论汇总

> 本文汇总近期关于
> `virsh / libvirt / QEMU`、SSH、TLS、`exec`、非共享存储迁移、压缩机制以及国密安全迁移设计的讨论。重点是澄清"控制通道"和"数据通道"的区别，并形成后续实验与方案设计的基础。

## 1. 整体迁移架构

在 libvirt 管理的 KVM/QEMU 环境中，可以把主要组件理解为：

``` text
用户
 ↓
virsh
 ↓
libvirt
 ↓
QEMU
 ↓
KVM / Linux
```

-   **virsh**：用户使用的虚拟机管理命令行工具。
-   **libvirt**：负责虚拟机管理、迁移协调和 QEMU 生命周期管理。
-   **QEMU**：真正组织虚拟机运行状态，并产生/接收 migration stream。
-   **KVM**：Linux 内核中的硬件虚拟化加速能力。

热迁移时，真正需要迁移的运行状态主要包括：

``` text
VM
├── RAM
├── CPU state
├── Device state
└── Disk（非共享存储场景）
```

其中 RAM、CPU、设备状态主要进入 QEMU migration
stream；非共享存储下的磁盘数据还存在独立的 block/storage 数据路径。

------------------------------------------------------------------------

## 2. 对之前 qemu+ssh 实验的重新理解

之前容易形成这样的理解：

``` text
VM
 ↓
QEMU
 ↓
libvirt
 ↓
SSH
 ↓
所有迁移数据
 ↓
目标服务器
```

这个理解**不准确**。

例如使用：

``` bash
virsh migrate ... 'qemu+ssh://<目标主机>/system?...'
```

`qemu+ssh://` 首先表示：

> virsh/libvirt 通过 SSH 与目标端 libvirt 建立安全的远程管理连接。

更准确的结构是：

``` text
控制面：

virsh / libvirt-A
        │
        │ SSH
        ▼
     libvirt-B


数据面：

QEMU-A ─── migration channel ───► QEMU-B
   │
   └──── storage/block/NBD ─────► 目标存储
```

因此：

``` text
管理通道安全 ≠ 所有迁移数据自动安全
```

如果迁移命令只是使用 `qemu+ssh://`，而没有额外使用 tunnelled
migration、migration TLS 等机制，不能仅凭 SSH URI 就断言 RAM
和磁盘数据全部经过 SSH 加密。

之前实验更准确的描述是：

> 通过 SSH 安全连接目标端 libvirt，并由 libvirt 协调完成虚拟机迁移；但
> SSH 管理连接本身不能证明 QEMU migration stream
> 和非共享存储数据流均经过 SSH 加密。

------------------------------------------------------------------------

## 3. 控制通道与数据通道

可以把迁移划分成两个层面。

### 3.1 控制通道

负责：

-   发起迁移
-   创建目标 QEMU
-   设置迁移参数
-   查询迁移状态
-   完成/取消迁移
-   libvirt 间的协调

例如：

``` text
virsh
 ↓
libvirt-A
 ║
 ║ SSH
 ║
 ↓
libvirt-B
```

### 3.2 数据通道

负责真正的大量 VM 数据：

``` text
QEMU-A
 ├── RAM/CPU/Device ──► QEMU migration stream ──► QEMU-B
 │
 └── Disk ────────────► Block/NBD path ─────────► Target Disk
```

因此在非共享存储迁移中，可以重点研究两条逻辑数据路径：

1.  **QEMU migration stream**
    -   RAM
    -   CPU state
    -   Device state
2.  **Storage / Block / NBD stream**
    -   虚拟磁盘数据

具体连接数、端口和存储迁移实现会随 QEMU/libvirt
版本与配置变化，不能简单认为永远固定为两个 TCP 连接。

------------------------------------------------------------------------

## 4. QEMU Migration TLS

QEMU 原生的标准迁移数据安全机制主要是 TLS。

注意：

> **TLS 是安全协议，不是一种具体加密算法。**

整体关系：

``` text
QEMU
 ↓
Migration Stream
 ↓
TLS
 ↓
TCP
 ↓
Network
```

TLS 负责：

-   身份认证
-   密钥协商
-   数据机密性
-   数据完整性

而具体密码算法由 TLS 协议及底层密码库能力决定。

例如可以抽象为：

``` text
TLS
├── Authentication
├── Key Exchange
├── Symmetric Encryption
└── Integrity / AEAD
```

所以更准确的说法是：

> QEMU 使用 TLS 协议保护 migration stream，而 TLS
> 再调用具体密码算法完成认证、密钥建立、数据加密与完整性保护。

------------------------------------------------------------------------

## 5. TLS 不负责决定迁移哪些数据

TLS 位于 QEMU migration 数据处理之后。

``` text
VM State
 ↓
QEMU Migration Logic
 ↓
数据选择 / 编码 / 优化 / 可选压缩
 ↓
Migration Stream
 ↓
TLS
 ↓
TCP
```

TLS 不需要理解：

-   这是不是 RAM
-   是不是 dirty page
-   是不是 zero page
-   是不是 CPU state
-   是否经过压缩

对于 TLS 来说，它们最终都是字节流。

------------------------------------------------------------------------

## 6. RAM 热迁移过程

典型 pre-copy 热迁移可以简化成：

``` text
第一轮：
大量 RAM
 ↓
目标端

VM继续运行
 ↓
部分页面被修改
 ↓
Dirty Pages

第二轮：
Dirty Pages
 ↓
目标端

第三轮：
新的 Dirty Pages
 ↓
目标端

...

最终：
暂停源VM
 ↓
最终Dirty Pages
+ CPU State
+ Device State
 ↓
目标端恢复
 ↓
目标VM继续运行
```

所以热迁移不是简单的一次：

``` text
RAM → Network
```

而是多轮同步。

------------------------------------------------------------------------

## 7. QEMU 对迁移数据的特殊处理

即使没有显式开启通用压缩，也不能说 QEMU 一定把所有 VM
数据逐字节原样发送。

### 7.1 Dirty Page

VM 在迁移过程中继续运行，被修改的内存页会被标记并在后续轮次重新发送。

``` text
RAM Page
 ↓
被VM修改
 ↓
Dirty
 ↓
下一轮重新发送
```

### 7.2 Zero Page

全零内存页可以进行特殊识别/编码，避免简单地发送完整的一页零字节。

``` text
4 KB Zero Page
 ↓
Zero Page Detection
 ↓
特殊表示
 ↓
Migration Stream
```

这属于迁移优化，不等同于普通压缩。

### 7.3 XBZRLE 等机制

对于反复修改但变化较小的内存页，可以使用差异编码等机制减少需要发送的数据。

概念上：

``` text
Old Page
 ↓
与 New Page 比较
 ↓
编码差异
 ↓
发送较少数据
```

具体可用能力取决于 QEMU/libvirt 版本和迁移配置。

------------------------------------------------------------------------

## 8. `--compressed` 的理解

普通迁移命令如果没有显式使用：

``` bash
--compressed
```

通常不能认为启用了通用 RAM compression。

例如：

``` bash
virsh migrate \
  --live \
  --persistent \
  --copy-storage-all \
  ...
```

没有 `--compressed`，因此不应该描述成"开启了迁移压缩"。

但：

``` text
没有 --compressed
       ≠
QEMU没有任何数据优化
```

Dirty-page tracking、zero-page handling 等与通用压缩是不同概念。

开启压缩后，数据路径可以理解为：

``` text
RAM
 ↓
QEMU Migration
 ↓
Compression / Encoding
 ↓
Migration Stream
 ↓
TLS（若开启）
 ↓
TCP
```

正确顺序应当是：

``` text
压缩 → 加密
```

而不是：

``` text
加密 → 压缩
```

因为加密后的数据通常接近高熵数据，再进行压缩一般难以取得明显收益。

------------------------------------------------------------------------

## 9. 非共享存储磁盘迁移

使用：

``` bash
--copy-storage-all
```

表示需要迁移非共享存储中的虚拟磁盘。

但：

``` text
--copy-storage-all ≠ --compressed
```

不能理解成：

``` text
qcow2
 ↓
自动压缩
 ↓
发送
```

磁盘迁移更接近：

``` text
Disk
 ↓
QEMU Block Layer
 ↓
Block / NBD / Storage Migration
 ↓
Network
 ↓
Target Disk
```

根据具体机制，未分配区域、零块等可能得到特殊处理。

因此：

> "网络上传输的数据小于虚拟磁盘逻辑容量"不等于"磁盘被压缩了"。

------------------------------------------------------------------------

## 10. TLS + 非共享存储迁移

理想的安全非共享存储迁移可以抽象为：

``` text
                    VM
             ┌──────┴──────┐
             ▼             ▼
       RAM/CPU/Device      Disk
             │             │
      QEMU Migration     Block/NBD
             │             │
            TLS           TLS
             │             │
             └──────┬──────┘
                    ▼
                 Network
```

也就是说，不能只保护 migration stream，而忽略磁盘数据路径。

研究时应分别确认：

-   migration stream 是否加密；
-   block/NBD/storage stream 是否加密。

------------------------------------------------------------------------

## 11. QEMU 的 exec transport

QEMU migration 不只有 TCP transport，还存在 `exec`、Unix
socket、fd、file 等传输形式（具体支持和接口形式取决于版本）。

需要特别注意：

> **exec 是 transport，不是加密算法。**

它的基本思想：

``` text
QEMU
 ↓
Migration Stream
 ↓
pipe
 ↓
External Process stdin
```

因此可以把外部程序换成 SSH：

``` text
QEMU-A
 ↓
Migration Stream
 ↓
exec
 ↓
SSH
 ║
 ║ encrypted
 ║
 ↓
SSH
 ↓
目标接收路径
 ↓
QEMU-B
```

这与 `qemu+ssh://` 有本质区别。

### qemu+ssh

``` text
libvirt-A
 ↓
SSH
 ↓
libvirt-B
```

主要是远程 libvirt 管理连接。

### migration exec → ssh

``` text
QEMU Migration Stream
 ↓
exec
 ↓
SSH
```

这里 SSH 直接承担 migration stream 的外部传输保护。

因此不能把两者混为一谈。

------------------------------------------------------------------------

## 12. exec 对国密原型的意义

`exec` 的价值在于外部程序不一定必须是 SSH。

理论实验可以构造：

``` text
QEMU-A
 ↓
Migration Stream
 ↓
exec
 ↓
国密代理程序
 ↓
SM4等安全处理
 ↓
Network
 ↓
国密代理程序
 ↓
解密/验证
 ↓
QEMU-B
```

这样可以在第一阶段不修改 QEMU 的 RAM migration 算法，就验证：

> 是否能够在 QEMU migration stream 与网络之间加入国密安全层。

但需要注意：

``` text
exec保护Migration Stream
        ≠
自动保护非共享磁盘数据
```

磁盘/NBD 路径仍需要单独处理。

------------------------------------------------------------------------

## 13. 为什么更推荐从 TLS 层做国密改造

如果最终目标是工程化方案，更自然的方向不是：

``` text
QEMU → 自定义SM4协议 → TCP
```

而是：

``` text
QEMU
 ↓
Migration Stream
 ↓
TLS / 国密兼容TLS安全层
 ↓
底层密码能力
 ↓
SM2 / SM3 / SM4 等
 ↓
TCP
```

这样可以尽量保留 QEMU 已经成熟的：

-   pre-copy
-   dirty-page tracking
-   zero-page handling
-   compression
-   XBZRLE 等迁移优化
-   migration stream 格式
-   VM state save/restore

国密改造主要集中在安全层，而不是修改虚拟机迁移算法本身。

------------------------------------------------------------------------

## 14. 国密方案的初步设计

可以把未来方案抽象为：

``` text
Host A                                      Host B

QEMU-A                                      QEMU-B
  │                                           ▲
  ▼                                           │
Migration Stream                              │
  │                                           │
  ▼                                           │
┌────────────────┐                    ┌────────────────┐
│ 国密安全层      │ =================> │ 国密安全层      │
│                │                    │                │
│ SM2            │                    │ SM2            │
│ SM3            │                    │ SM3            │
│ SM4            │                    │ SM4            │
└────────────────┘                    └────────────────┘
```

概念性职责可以划分为：

-   **SM2**：身份认证、签名、密钥相关能力；
-   **SM3**：哈希/完整性相关密码能力；
-   **SM4**：对称数据加密。

实际协议设计不能简单等价为"直接用 SM4 加密"，还必须考虑：

-   双向身份认证；
-   会话密钥建立；
-   完整性/认证加密；
-   重放防护；
-   密钥生命周期；
-   migration 和 storage 通道的密钥隔离；
-   异常中止与恢复；
-   证书/凭据管理。

------------------------------------------------------------------------

## 15. 双通道国密安全迁移目标

对于非共享存储，可以形成更完整的目标：

``` text
                  VM
          ┌────────┴─────────┐
          ▼                  ▼
     RAM/CPU/Device         Disk
          │                  │
     Migration             NBD/Block
          │                  │
    国密安全通道A       国密安全通道B
          │                  │
          └────────┬─────────┘
                   ▼
                 Network
```

可以进一步考虑：

``` text
统一身份认证 / 密钥协商
          │
     ┌────┴────┐
     ▼         ▼
K_migration  K_storage
     │         │
     ▼         ▼
Migration    Storage
Channel      Channel
```

即两个数据通道使用独立会话密钥，同时由统一的身份认证/密钥管理体系管理。

------------------------------------------------------------------------

## 16. 当前可考虑的实验路线

### 实验 A：普通迁移

``` text
Migration Stream
 ↓
Plain Transport
```

作为性能和网络行为基线。

### 实验 B：SSH / Tunnel / exec 原型

验证 migration stream 是否真正进入安全隧道，并观察：

-   网络连接；
-   CPU 占用；
-   迁移时间；
-   网络传输量。

### 实验 C：标准 TLS

``` text
Migration
 ↓
TLS
 ↓
Network
```

建立标准安全迁移基线。

### 实验 D：TLS + Compression

``` text
Migration
 ↓
Compression
 ↓
TLS
 ↓
Network
```

研究压缩减少网络传输与增加 CPU 开销之间的权衡。

### 实验 E：国密原型

``` text
Migration
 ↓
国密安全层
 ↓
Network
```

先验证 migration stream 的可行性。

### 实验 F：完整非共享存储国密迁移

``` text
Migration ──► 国密安全通道
Storage   ──► 国密安全通道
```

形成完整方案。

------------------------------------------------------------------------

## 17. 推荐记录的实验指标

后续比较 Plain / SSH / TLS / TLS+Compression / 国密方案时，可以记录：

  指标                   作用
  ---------------------- --------------------------
  Total Migration Time   总迁移耗时
  Downtime               VM 最终切换停机时间
  Network Bytes          实际网络传输量
  Bandwidth              网络带宽占用
  CPU Usage              加密/压缩带来的 CPU 开销
  Disk I/O               非共享磁盘迁移开销
  Dirty Rate             VM 内存脏页产生速度
  Success Rate           不同负载下迁移稳定性
  Crypto Throughput      安全层加解密吞吐
  VM Service Impact      迁移过程中业务性能影响

------------------------------------------------------------------------

## 18. 验证之前 SSH 实验的方法

不要仅根据迁移命令判断所有数据是否经过 SSH。

迁移过程中可以观察连接：

``` bash
ss -tnp
```

也可以根据环境过滤 QEMU/libvirt/SSH 相关连接。

抓包可以用于判断是否存在 SSH 端口之外的独立数据连接：

``` bash
tcpdump -i any host <目标主机IP> -nn
```

需要重点确认：

``` text
SSH connection
      │
      ├── libvirt控制？
      │
      └── migration data？

其他TCP连接
      │
      ├── QEMU migration？
      │
      └── NBD/storage？
```

如果 SSH 连接之外还有 QEMU 到目标端的独立 TCP
数据连接，就说明不能把整个迁移描述成"全部通过 SSH"。

同时还需要检查 libvirt/QEMU
配置，因为安全机制可能由系统配置强制启用，不能仅从命令行参数反推全部行为。

------------------------------------------------------------------------

## 19. 当前最重要的几个结论

### 结论一

``` text
qemu+ssh://
```

不等于：

``` text
所有VM迁移数据 → SSH
```

它首先代表 libvirt 的 SSH 远程连接方式。

### 结论二

``` text
Control Plane Security
          ≠
Migration Data Security
```

必须分别研究控制通道和数据通道。

### 结论三

QEMU migration 的原生标准安全保护重点是 TLS，而 TLS 不是单个加密算法。

### 结论四

QEMU 会先完成迁移数据的选择、编码和可选压缩，然后再把 migration stream
交给 TLS：

``` text
VM State
 ↓
QEMU Migration Logic
 ↓
Optimization / Compression
 ↓
Migration Stream
 ↓
TLS
 ↓
TCP
```

### 结论五

普通迁移没有 `--compressed` 时，不能认为启用了通用迁移压缩；但仍可能存在
dirty-page、zero-page 等迁移优化。

### 结论六

``` text
--copy-storage-all
```

表示非共享存储磁盘需要复制，但不代表磁盘自动进行了压缩或自动获得 SSH/TLS
加密。

### 结论七

非共享存储安全迁移应同时考虑：

``` text
Migration Stream Security
          +
Storage/NBD Security
```

### 结论八

`exec` 是 transport，不是加密协议。它可以把 migration stream 交给 SSH
或自定义国密代理，因此很适合做第一阶段原型验证。

### 结论九

如果最终目标是工程化国密迁移，优先研究：

``` text
现有QEMU迁移机制
        ↓
成熟安全协议层
        ↓
国密密码能力
```

比直接修改 QEMU 的 RAM/dirty-page 等迁移算法更合理。

------------------------------------------------------------------------

## 20. 后续研究主线

建议按以下顺序继续：

``` text
① 搞清当前普通迁移真实数据路径
             ↓
② 抓包 + ss 验证 SSH / Migration / Storage 连接
             ↓
③ 建立 Plain Migration 基线
             ↓
④ 配置并验证标准 TLS Migration
             ↓
⑤ 验证非共享磁盘数据路径的安全保护
             ↓
⑥ 比较 TLS / Compression / TLS+Compression
             ↓
⑦ exec + 国密代理做原型验证
             ↓
⑧ 研究 TLS/密码库层的国密适配
             ↓
⑨ Migration + Storage 双通道国密保护
             ↓
⑩ 性能、安全性、兼容性实验与汇报
```

最终可以形成的研究目标表述：

> **针对 KVM/QEMU 非共享存储热迁移场景，分析控制通道、QEMU migration
> stream 与存储迁移数据通道的安全边界，在保留 QEMU
> 原有热迁移、脏页跟踪和数据优化机制的基础上，为运行状态迁移与存储迁移分别建立国密安全保护机制，并对其机密性、完整性、身份认证能力及迁移性能开销进行验证。**

------------------------------------------------------------------------

## 参考官方文档

-   QEMU
    Migration：`https://www.qemu.org/docs/master/devel/migration/main.html`
-   QEMU TLS：`https://www.qemu.org/docs/master/system/tls.html`
-   QEMU NBD：`https://www.qemu.org/docs/master/interop/nbd.html`
-   libvirt Migration：`https://libvirt.org/migration.html`
-   virsh Manual：`https://www.libvirt.org/manpages/virsh.html`

> 注：具体参数、默认压缩方式、TLS/NBD 能力以及 migration transport
> 的命令形式可能随 QEMU/libvirt
> 版本变化。正式实验前应结合实验环境的实际版本确认。
