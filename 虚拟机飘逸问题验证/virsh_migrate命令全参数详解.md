# virsh migrate 命令全参数详解

> 基于现代 libvirt（virsh ≥ 5.x）。整理自 libvirt 官方手册与源码（tools/virsh-domain.c `cmdMigrate`），作为《虚拟机热迁移调研记录文档》的配套参考资料。实际可用参数以本环境 `virsh migrate --help` 输出为准。

---

## 一、基础语法

```bash
virsh migrate [选项] <域名> <目标URI> [<迁移URI>]
```

- `域名`：虚拟机名（如 test2）
- `目标URI`：目标端 libvirt 连接（如 `qemu+ssh://root@IP/system?port=55555`）
- `迁移URI`：可选第三参数，**手动指定数据通道地址**（默认由两端 libvirtd 自动协商，一般无需指定）

---

## 二、模式类参数（决定"怎么迁"）

| 参数 | 作用 | 说明 |
|---|---|---|
| `--live` | 在线迁移 | VM 运行中迁移（热迁移核心参数） |
| `--offline` | 离线迁移 | 仅迁移持久化配置到目标端，VM 不启动 |
| `--p2p` | 点对点模式 | 由源端 libvirtd 全程代理协调目标端（**本环境实际模式**） |
| `--direct` | 直接模式 | 客户端分别协调两端（与 p2p 相对，现代 libvirt 混合场景自动处理） |
| `--persistent` | 目标端持久化定义 | 迁移后在目标端写入持久配置（本环境已使用） |
| `--undefinesource` | 删除源端定义 | 迁移成功后删除源端持久化定义（注意：**不删源磁盘文件**，源端数据残留问题见主文档 5.5/5.7 安全边界分析） |
| `--suspend` | 迁移后暂停 | 目标端恢复后保持 paused 状态（迁移但先不跑） |
| `--copy-storage-all` | 全量磁盘复制 | 非共享存储，磁盘全量随迁（**本环境已使用**） |
| `--copy-storage-inc` | 增量磁盘复制 | 目标端已有 backing file 时只传增量（配合外部快照链） |
| `--change-protection` | 禁止并发配置变更 | 迁移期间锁住域配置，防止中途 dumpxml/edit 干扰 |
| `--abort-on-error` | 错误即中止 | 任何子错误（如磁盘 I/O 错误）立即中止而非带病完成 |
| `--postcopy` | 启用后拷贝模式 | 先切换、后传内存（停机极短，但目标端中途故障 VM 即死，**不可回退**） |
| `--postcopy-after-precopy` | 预拷贝不收敛才转后拷贝 | 先正常预拷贝，收敛失败时自动转 postcopy |
| `--tunnelled` | 隧道模式 | 数据流复用 libvirt 控制连接（qemu+ssh 时即全程 SSH 加密；牺牲性能换安全） |
| `--parallel` | 并行多连接迁移 | 多线程多 TCP 连接传内存（multifd），大内存提速 |
| `--parallel-connections <n>` | 并行连接数 | 指定 multifd 通道数 |

---

## 三、性能调优类参数

| 参数 | 作用 | 说明 |
|---|---|---|
| `--bandwidth <MiB/s>` | 迁移带宽上限 | 限制迁移过程的网络占用（避免打满业务网） |
| `--timeout <秒>` | 迁移总超时 | 超时后按 `--timeout-action` 处理 |
| `--timeout-action <action>` | 超时动作 | `suspend`（暂停源端强行收敛）/ `abort` / `postcopy` |
| `--downtime <毫秒>` | 停机时间上限 | stop-and-copy 阶段允许的最大停机窗口 |
| `--set-cpuset <cpuset>` | 目标端 CPU 绑定 | 恢复时绑定到指定物理 CPU 集 |
| `--xml <文件>` | 目标端替代 XML | 目标端硬件不同时，用改过的 XML 起目标 QEMU（如不同网桥名、不同存储路径） |

---

## 四、压缩/加密类参数

| 参数 | 作用 | 说明 |
|---|---|---|
| `--compressed` | 启用内存压缩传输 | 配合下方方法/参数使用 |
| `--comp-methods <方法>` | 压缩方法 | `xbzrle`（脏页差分编码）/ `zlib` / `zstd`（zlib/zstd 与 multifd 搭配，与 xbzrle 思路不同） |
| `--comp-bitmap-size <n>` | xbzrle 缓存大小 | 重复写热点场景调大可显著减少传输量 |
| `--comp-mt-level <n>` | 压缩线程数/级别 | 多线程压缩的并行度 |
| `--comp-mt-threads <n>` | 压缩线程数 | — |
| `--comp-mt-dirty-bitmap-size <n>` | 单线程脏位图页数 | — |
| `--tls` | 数据面启用 TLS | QEMU 原生加密（配合 qemu.conf 的 TLS 证书配置） |
| `--tls-destination <主机名>` | TLS 目标名 | 指定证书校验的目标主机名 |

**压缩与加密的顺序恒为：压缩 → 加密**（加密后数据接近高熵，再压缩无收益）。未加 `--compressed` 时无通用压缩，但 dirty-page 追踪、zero-page 编码等内建优化仍然存在。

---

## 五、高级/杂项类参数

| 参数 | 作用 | 说明 |
|---|---|---|
| `--migrate-disks <盘列表>` | 指定迁移哪些盘 | `--copy-storage-all` 时排除某些盘（如数据盘不动） |
| `--graphics-listen <addr>` | 图形会话重定位 | 指定目标端图形监听地址（对应 cookie 的 GRAPHICS 字段） |
| `--print-xml` | 只打印不执行 | 输出迁移将使用的目标 XML（预演/dry-run） |
| `--verbose` | 显示进度条 | 迁移过程百分比实时显示（本环境已使用） |
| `--disco-defer` / `--disco-passthrough` | DISCO 相关 | 一次性密码迁移保护（实验性，需 QEMU 支持） |
| `--migration-hole-*` | postcopy 端口控制 | 后拷贝模式恢复端口管理（较新版本） |
| `--risk-behavior` | 跳过部分安全检查 | 绕过某些一致性预检（危险，勿用） |

---

## 六、配套同族命令

| 命令 | 作用 | 说明 |
|---|---|---|
| `virsh migrate-setmaxdowntime <域> <ms>` | 动态调停机上限 | 迁移进行中可调 |
| `virsh migrate-setspeed <域> <MiB/s>` | 动态限速 | 迁移进行中可调 |
| `virsh migrate-compcache <域> <大小>` | 调 xbzrle 缓存 | 迁移进行中可调 |
| `virsh domjobinfo <域>` | 查迁移进度/统计 | 本环境已使用（注意：不支持 `--live` 选项） |
| `virsh domjobabort <域>` | 中止迁移任务 | — |
| `virsh cpu-compare <XML>` | CPU 兼容性预检 | 迁移前强制预检项（本环境问题 3 的教训） |
| `virsh domblklist <域>` | 清点设备 | 迁移前检查光驱等不可迁移设备（本环境问题 1 的教训） |

---

## 七、本环境标准命令组合（带注释）

```bash
virsh migrate \
  --live \                                    # 热迁移
  --persistent \                              # 目标端持久化定义
  --copy-storage-all \                        # 非共享存储磁盘全量随迁
  --change-protection \                       # 禁止迁移中改配置（可选，推荐）
  --abort-on-error \                          # 任何错误立即中止（可选，推荐）
  --bandwidth 200 \                           # 限速 200MiB/s 保护业务网（按实际调）
  --downtime 500 \                            # 允许最长停机 500ms（按业务调）
  --timeout 1800 --timeout-action abort \    # 30 分钟收敛不了就中止回滚
  --verbose \
  test2 \
  'qemu+ssh://root@10.19.207.10/system?port=55555'
```

---

## 八、与调研任务直接相关的选型建议

1. **安全验证实验**：跑两组对比——`--tunnelled`（数据复用 SSH 控制连接，全程加密）vs 不加（数据 QEMU 直连，`ss -tnp` 应观察到 QEMU 间独立 TCP 连接），配合 `tcpdump` 抓包即可完成主文档 5.7（3-1）的数据面加密状态验证；
2. **性能实验**：大内存 VM 可加 `--parallel --parallel-connections 4`（multifd）或 `--compressed --comp-methods zstd`，记录迁移时间/CPU 开销对比（对应讨论汇总实验 D）；
3. **风险提示**：`--postcopy` 停机最短但**不可回退**（目标端中途故障 VM 即死）；`--risk-behavior` 跳过安全检查。二者可作为报告"权衡分析"素材，但实验环境不要对 test2 使用 postcopy。

---

*参考：libvirt 官方文档 https://libvirt.org/manpages/virsh.html 、源码 tools/virsh-domain.c `cmdMigrate`（本环境参数以 `virsh migrate --help` 实际输出为准）。*
