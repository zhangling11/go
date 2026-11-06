# OpenStack Nova 热迁移做法记录

> 日期：2026-11-06
> 用途：调研对标材料（对应《虚拟机热迁移规范调研大纲》§3.6 产品功能对标）
> 记录原则：**逐条标注核实状态**。✅ 为本次实际抓取官方文档确认；⚠️ 为通用知识、本次未抓到原文，需自行核对后再引用。

---

## 0. 一句话概括

Nova 的热迁移 = **libvirt/QEMU 原生迁移能力 + 调度器选目标 + 一组开关式策略参数 + 完整的状态机与监控接口**。

它对我们的最大参考价值不在"底层怎么传内存"（那部分和自研 KVM 平台一样），而在三处上层设计：

1. **目标节点由调度器选，不由用户选**（对应我们的"系统推荐"）
2. **收敛失败/超时这类"卡住"的情况有明确的策略出口**（abort / force_complete）
3. **迁移是一定能被加密的，且官方明确否定了"SSH 隧道"这条省事路线**

---

## 1. 迁移方式：共享存储 vs 块迁移

| 方式 | 触发 | 说明 | 核实 |
|---|---|---|---|
| 默认（共享存储） | 直接 live-migrate | 只传内存 + 设备状态，磁盘不动 | ✅ |
| **块迁移（block migration）** | CLI 加 `--block-migration`（`openstack server migrate`）或 `-block-migrate`（`nova live-migration`） | 本地盘随 VM 一起迁，非共享存储场景 | ✅ |

> ✅ 官方原文特别提示：**块迁移期间磁盘字节进度可能一直显示为 0**，即使正在传。这是进度条设计的一个真实坑——我们界面做"已同步/剩余数据量"时要注意磁盘复制和内存复制的进度口径不同源。

**对我们的映射**：我们的平台是纯非共享存储（始终 `--copy-storage-inc`），等价于 Nova 的"块迁移恒开"。所以 Nova 的块迁移路径比它的默认路径更值得我们看。

---

## 2. 目标节点怎么定：两种模式

| 模式 | 做法 | 我们的对应 |
|---|---|---|
| **调度器选** | `nova live-migration <vm>` 不带 `--host`，由 Nova scheduler 按权重选节点 | ≈ 我们的【系统推荐】 |
| **手选** | `openstack server migrate --live-migration --host HostC` | ≈ 我们的场景 C 手选节点 |

> ✅ 官方文档把这两种并列成"manual / automatic destination selection"两个小标题，且**手选路径不会被禁用**——和我们设计原则 #7"推荐只是给出候选，不是代替选择"是同一个判断。

**注意差别**：Nova 的调度器在迁移场景下会考虑 `migrate` 相关的 filter（如 CPU 兼容性、资源余量），失败时返回 `NoValidHost`。它没有"多个候选卡片让用户挑"这一步——**我们做的"2–3 个候选 + 排序理由"是比 Nova 更友好的交互，属于我们的加分项，不是差距**。

---

## 3. 收敛与卡住时的策略出口（最值得抄的一块）

### 3.1 auto-converge（自动降速收敛）

- ✅ 开关：`live_migration_permit_auto_converge = true`（nova.conf，改完需重启所有 nova-compute）
- ✅ 作用：libvirt 检测到 guest 写内存速度快过迁移速度时，**主动放慢 guest 的 CPU**
- ✅ 官方明示代价："the instance runs slower"

> **对照我们**：`assess.go` 实现的是"脏页率超阈值 → 强制限 vCPU"，机制相同。差别在于我们是**平台主动判定并施加**，Nova 是**libvirt 自动施加、平台只给开关**。我们的做法用户感知更强（有告警条），但要小心"自动改用户 VM 的 CPU"这件事在合规上要不要留痕。

### 3.2 post-copy（后拷贝）

- ✅ 开关：`live_migration_permit_post_copy = true`
- ✅ 机制：**先在目标端把 VM 跑起来**，缺的内存页通过缺页异常回源端取
- ✅ 官方列出的三条风险：
  1. **节点间网络一断，无法解析的缺页 → VM 直接重启**（这是最狠的一条）
  2. 缺页率高时 VM 明显变慢
  3. **与 VFIO / 受控设备（vGPU）不兼容**，QEMU 直接拒绝；这类宿主机必须把开关改回 `false`

> **对照我们**：我们已列为范围外，判断正确。但报告里应当写明理由用的是 Nova 官方这三条，而不是"复杂所以不做"。

### 3.3 超时策略（我们完全没有的一档）

- ✅ 两层参数：`libvirt.live_migration_completion_timeout`（多久算超时）+ `libvirt.live_migration_timeout_action`（超时后干什么）
- ✅ 动作两选一：
  - `abort`（**默认**）—— 取消迁移
  - `force_complete` —— **强制完成切换**：暂停 VM 或（开了 post-copy 时）转 post-copy

**为什么这条对我们重要**：我们的现状是"脏页不收敛就一直跑"，只有 CPU 节流一个手段。Nova 给出了第二种出口——"承认收不敛，选择缩短痛苦"。对一个密码业务平台，`force_complete` 意味着"接受更长的停机换确定性"，这是个**业务决策，不是技术决策**，应该在界面里以"最长等待时间"的形式让用户配置，而不是写死在代码里。

建议：这条进调研报告的"整改建议"，优先级中等（P2），因为它需要先有可信的预估剩余时间才谈得上设超时。

---

## 4. 监控与取消（接口层面的做法）

| 能力 | Nova 的做法 | 核实 |
|---|---|---|
| 查询迁移任务列表 | `nova server-migration-list` | ✅ |
| 查询单个迁移详情 | `nova server-migration-show` | ✅ |
| 判断"是不是卡住了" | 看 `remaining memory bytes` 是否**不再下降**；不下降则可能被 abort | ✅ |
| 中止迁移 | `DELETE /servers/{id}/migrations/{mid}` | ✅ |
| 强制完成 | `POST .../action` 带 `force_complete` | ✅ |

> **两条可直接借鉴的设计判断**：
> 1. **迁移被建模成一个独立的、有 ID 的资源**（migration 对象），而不是一个任务状态字段。好处是列表/详情/中止/强制完成都能挂在这个 ID 上，接口语义干净。
> 2. **"卡住"的判定标准是"剩余字节不再下降"**，不是"耗时超过 X 秒"。这是个比时间阈值更靠谱的判据——耗时阈值在不同磁盘规模下完全不可比。我们界面的"预计剩余时间持续增长"告警，本质是同一思路的弱化版，可以改成这个更强判据。

---

## 5. 加密：官方对 "SSH 隧道" 这条路的否定

这是整个 Nova 文档里对我们最有价值的一节。

### 5.1 官方对 tunnelled 迁移的评价（三条否定）

> ✅ 原文要点：旧的 tunnelled 方案**不够用**，因为
> 1. 它**没有保护所有的迁移流**（unsecured streams remain）
> 2. 它**不能处理块迁移**（block migration）
> 3. 它有**性能开销**

第 2 条和我们《需求确认表》§4 里的判断完全一致（tunnelled 与非共享存储互斥）——我们的结论不是自己推的，是社区共识。

### 5.2 QEMU 原生 TLS 的完整落地步骤

| 环节 | 要求 | 核实 |
|---|---|---|
| 版本前提 | **libvirt 4.4.0+、QEMU 2.11+** | ✅ |
| 前置条件 | 已配置 TLS 环境、免密 SSH、`/etc/libvirt/qemu.conf` 相关设置 | ✅ |
| 校验工具 | `virt-pki-validate` | ✅ |
| 证书权限 | **0640，属主 root:qemu** | ✅ |
| 开启方式 | `live_migration_with_native_tls = true` + `live_migration_scheme = tls` | ✅ |
| 互斥关系 | **不能与 tunnelled 选项同时使用** | ✅ |
| 生效方式 | 重启 nova-compute | ✅ |
| **防火墙端口** | **16514** + **49152–49215** | ✅ |

> ✅ 官方明确推荐 "Native TLS is recommended over the tunnelling option"。

**重要副产品**：这两条端口是 OpenStack 官方文档明确写出的，**比 libvirt 自己的文档更具体**。我们大纲 RQ-1-07 原本只能写"libvirt 官方未给具体段、待实测"，现在可以补一条可引用的文档源（OpenStack Nova 官方文档）。注意仍应标明这是 OpenStack 的部署约定，不等于 libvirt 的强制规定。

**对我们安全章（RQ-3-01/02）的意义**：整改路线从此有了完整可执行的方案，不是"建议上 TLS"这种空话——版本门槛、证书权限、开关名、端口全都有了。而且这条路线和我们的架构兼容：我们本来就是 native 迁移 + `--copy-storage-inc`，正是 TLS 方案覆盖的场景。

---

## 6. Nova 的完整迁移能力清单（对照勾选用）

| 能力 | Nova | 我们平台 | 结论 |
|---|---|---|---|
| 在线迁移 | ✅ | ✅ | 齐 |
| 冷迁移 | ✅ | ✅ | 齐 |
| 非共享存储/块迁移 | ✅ | ✅（恒开） | 齐 |
| 带宽限速 | ⚠️（`live_migration_bandwidth`，未原文核实） | ✅ | 齐 |
| 迁移加密 | ✅ 原生 TLS（推荐）/ tunnelled | ❌ 数据面明文 | **差距（核心）** |
| 目标节点自动选择 | ✅ 调度器 | ✅ 系统推荐（更友好） | 我们更好 |
| 多候选供用户挑 | ❌ | ✅ | 我们更好 |
| auto-converge | ✅ 开关 | ✅ 平台主动 | 齐 |
| 超时策略 abort/force_complete | ✅ | ❌ | **差距（P2）** |
| post-copy | ✅ 开关 | ❌ 范围外 | 合理不做 |
| 迁移进度/详情查询 | ✅ 独立 migration 资源 | ✅ 任务 + SSE | 齐 |
| 中止迁移 | ✅ API | ✅ 任务取消 | 齐，但传导未验证（RQ-5-08） |
| 迁移前 CPU 兼容性检查 | ✅ 调度器 filter | ❌ 无预检 | **差距（核心）** |
| 批量/并发迁移 | ✅（默认并发数限制） | ❌ 范围外 | 合理不做 |

---

## 7. 我们从 Nova 该抄的三件事 / 不该抄的两件事

### 该抄

1. **"卡住"的判据用"剩余字节不再下降"**，不用耗时阈值（§4）。改动成本极低，判据质量提升明显。
2. **超时后的两种出口**（abort / force_complete）作为界面上的"最长等待"配置项（§3.3）。让业务方在"等下去"和"多停机一会儿"之间做选择，而不是平台替它选。
3. **原生 TLS 的完整落地清单**（§5.2）直接作为 RQ-3-01/02 的整改方案，版本门槛、证书权限、端口一个不缺。

### 不该抄

1. **post-copy**：官方三条风险（断网即重启、缺页拖慢、VFIO 不兼容）在密码业务平台上每条都致命，我们列范围外是对的。
2. **调度器全自动、用户不可干预**：Nova 面向的是成千上万 VM 的自动化云，管理员不该逐台挑机器；我们的场景是低频、高风险的密码业务迁移，人必须在环路里。这条差异要在报告里明说，不能简单写成"我们不如 Nova 自动"。

---

## 8. 出处

| 内容 | URL | 本次核实 |
|---|---|---|
| 迁移用法：块迁移、auto-converge、post-copy、超时动作、监控 API | https://docs.openstack.org/nova/latest/admin/live-migration-usage.html | ✅ 已抓取核对 |
| QEMU 原生 TLS：tunnelled 的不足、版本前提、证书权限、开关、端口 | https://docs.openstack.org/nova/latest/admin/secure-live-migration-with-qemu-native-tls.html | ✅ 已抓取核对 |
| Nova 管理文档目录（确认章节存在） | https://docs.openstack.org/nova/latest/admin/ | ✅ 已抓取核对 |
| 迁移配置项全集（本文档 §6 中标 ⚠️ 的项在此核对） | https://docs.openstack.org/nova/latest/admin/configuring-migrations.html | ⚠️ **本次抓取失败（限流），需自行打开核对** |
| 迁移权限排查 | https://docs.openstack.org/nova/latest/admin/support-compute.html | ⚠️ 未抓取，仅见目录 |

---

## 附：待自行核对的清单

打开 `configuring-migrations.html` 后，确认以下几项的确切名称与默认值（本文未敢写成事实）：

- `live_migration_uri` / `live_migration_scheme` 的取值格式
- `live_migration_inbound_addr`（指定目标端接收迁移流的网卡地址）
- `live_migration_bandwidth`（限速，单位与默认值）
- `live_migration_progress_timeout`（多久无进展判定为卡死）
- `live_migration_downtime` / `live_migration_downtime_steps` / `live_migration_downtime_delay`（停机时间上限及自动抬升策略）
- `block_migration` / `disk_over_commit` 的默认语义

核到之后把 ⚠️ 改成 ✅ 并补上原文关键句即可。
