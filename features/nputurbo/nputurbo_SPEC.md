# nputurbo — NPU 慢卡升频执行器 — 技术规格 (nputurbo_SPEC)

> 文档定位:nputurbo 模块的设计与规格文档。
> 对应代码:`features/nputurbo/`(Go package `nputurbo`,`//go:build linux`,与主项目同一 Go module)。
> 零新依赖:仅用标准库。**默认关闭**:`nputurbo.enabled` 默认 false,且 `dry_run` 默认 true(judge+log,不执行)。

> **修订(2026-08-18):** 调频命令改为 `/home/jw/npu_turbo_one.sh inject -d <id> -f <MHz>`(单卡升频)+ `/home/jw/npu_turbo_one.sh clean`(恢复**所有**卡到基线)。clean 是 all-or-nothing,故 actuator 不再按卡保存原频 / 不再按卡 restore;reconcile 改为"有卡恢复 → clean + 重新 inject 剩余慢卡;无卡恢复 → 仅 inject 新/变卡(不 clean,避免稳定卡抖动)"。

> **修订(2026-09-11):** A 与 M 改为动态获取——A = 该卡实时 `aicore_freq`、M = 由该卡 `aicore_rated_freq` 查**静态映射表** `ratedMaxBoost{1800→1850}`(未映射额定 → 该卡跳过不 boost,无 fallback)。两者均从 daemon 产出的 `snapshot_npu.json` 读取(`SnapshotFreqProvider`,按 `npu_id` 对齐,A3 双芯片取 min),故**前置条件恢复为 `snapshot.enabled: true`**;`max_freq_mhz` 配置项删除。
>
> **修订(2026-09-11 二次):** 升频判定简化——**`score > 1` 是唯一门槛**(另有 `A > M` 不操作的守卫):score>1 即 `B = round50(min(A×score, M))` 并 inject,即使 B=A(已在目标上)也不跳过——这保证已 boost 的卡持续留在 desired 集合,不被误判恢复。恢复判定:LastApplied 中的卡**不在清单或清单内 score≤1(等价于不在清单)→ clean + 重新 inject 剩余慢卡**;因缺频率数据/额定未映射/A>M 而被跳过的卡仍算慢,不触发 clean。
>
> **修订(2026-09-11 三次):** inject 标志全仓对齐为 `-d`(对齐 `npu_turbo_one.sh` 实际标志,补 `c9f05e1` 只改示例配置未改代码默认值的遗漏):Default() 默认命令、测试常量、注释与本文档统一为 `inject -d {id} -f {freq}`。
>
> **修订(2026-09-11 四次):** inject 命令追加额定频率:`inject -d {id} -f {freq} -r {rated}`。`{rated}` = 该卡 `aicore_rated_freq`(如 1800),从 BoostRow 透传。占位符可选——模板缺 `{rated}` 则不替换(旧配置零影响)。M(映射表上限)只用于 B 封顶与 `A>M` 跳过判定,**不传给脚本**。
>
> **修订(2026-09-14):** ① **device 对齐修复**:straggler 清单里的 id 确认为**全局 device id**(`npu_id × chips_per_card + chip_id`,即 stragglerout 喂给检测器的编号);`SnapshotFreqProvider` 改为按同一公式对齐(原按 `npu_id` 卡槽号对齐,双芯片节点会查不到/查错;单芯片两者重合,行为不变)。A3 双芯片不再"取 min"——每个 chip 是独立 device 条目。② **日志/CLI 人话化**:消息改为完整英文句子、面向运维(去掉 would_boost/desired/idempotent 等内部术语),全特性统一用 device 用语(与 `inject -d` 一致);`catmonitor nputurbo` 预览输出同步重写。
>
> **修订(2026-09-14 二次)——原生集成与无状态模型:** 基于外部 `npu_turbo_one.sh` + `dvfs.py` 源码分析,执行层重构:
> ① **原生集成**——dvfs.py 的 DSMI 调用（关/开 idle、设频、查额定、枚举设备）以 CGo+dlopen 原生实现（节点不再需要 python/脚本;非 NPU 节点优雅降级）;脚本编排逻辑进 controller/actuator;`npu_turbo_cmd`/`npu_turbo_clean_cmd` 配置删除,新增 `npu_turbo_bin`（全局抬频二进制路径,唯一残留 exec,默认 `/home/jw/npu_turbo`）。
> ② **无状态每轮重置**——删除 LastApplied/reconcile 全部状态;每轮有效 plan 后先 `CleanAll`（逐设备恢复到**各自**额定+重开 idle,修掉原脚本用 device 0 额定统一恢复的缺陷,混布额定节点也正确）,再按新清单注入。
> ③ **注入顺序**——先**一起**调 B>额定 组（一次 `npu_turbo -f maxB` 全局抬频 + 逐个非目标降到各自额定）,再逐张调 B≤额定 组（原生钉频）;顺序不可换——批量降额会覆盖先做的钉频。
>
> **修订(2026-09-14 三次):** `npu_dvfs` 来源包由 `internal/source/` 移至 **`features/nputurbo/dvfs`**(包名缩短为 `dvfs`)——确认知其仅被 nputurbo 使用,特性内聚优先;`internal/source/` 保留通用来源层(npu_turbo 全局抬频仍在其中)。
>
> **修订(2026-09-14 四次)——npu_turbo CWD 修复:** 节点实测发现 `npu_turbo` 按**进程 CWD**(非二进制目录)解析 `lptest` 助手——从无 `lptest` 的目录运行时 `cp lptest lptestN` 失败、`lptestN turbocfg` 全部 exit 127(TDP 恢复不完整)。原包装脚本靠 `cd $(dirname $0)` 规避,原生集成时丢失了该保护,daemon(systemd CWD=/)会以同样方式失败。修复:`realRun` exec 时以二进制目录为 CWD(bare PATH 名除外);部署要求 `npu_turbo_bin` 绝对路径 + `lptest` 同目录。
>
> **修订(2026-10-12)——轮次感知(去重 + 防过期):** straggler 结果 doc 顶层新增 `id`(检测轮次号,**检测器进程重启会从 1 重计**)与 `started_at`(该轮检测开始时间,RFC3339,**不随重启重置**)。① **选轮**——`ParseSlowCards` → `ParseLatestRound`:候选 doc 中带 `started_at` 者胜出,取最晚 `started_at`,平局取大 `id`(id 单独不可作序——重启重计,时间戳不会);全无 `started_at` 退化为取末行(旧格式行为)。② **跨 tick 去重**——轮次身份 = `(id, started_at)` 二元组(裸 id 会撞重启);与上次**完整成功**应用的轮次相同 → 本 tick 跳过全部 actuation(pin 在硬件上持续生效,省掉每 60s 的全设备 DSMI 写);actuation 任一步失败则不记录身份 → 下 tick 重试同轮次。③ **防过期**——最新轮次 `now - started_at > straggler_max_age`(新配置,默认 15m,YAML 里写 `0s` 关闭——duration 字段不接受裸 `0`,会解析报错)→ 仅 `CleanAll` 恢复额定、不注入(修掉"检测器停更 → 永久 boost"隐患)。`id`/`started_at` 任一缺失 → 不选轮、不去重、不检查(旧格式 = 原"无状态每轮重置"行为,零影响)。新指标:`nputurbo.round_id`、`nputurbo.result_age_seconds`(缺 started_at 发 -1)。

## 1. 目标

对慢卡(跑同一任务比正常卡慢,`score > 1.0`)升频。慢卡清单由**外部 straggler 检测器**产出:daemon 周期 HTTP GET `straggler_url` 拿检测结果(profiler doc,带轮次 `id`/`started_at`);nputurbo 结合 `snapshot_npu.json` 里的实时/额定频率对每张慢卡算目标频率 B,然后**每个新轮次先恢复所有设备到各自额定,再按新清单注入**——B>额定 的组一次批量全局抬频,B≤额定 的组逐张原生钉频;同轮次重复获取不重复注入,过期轮次仅恢复不注入。执行层为原生 DSMI 调用(`features/nputurbo/dvfs`,原 dvfs.py 语义)+ 唯一外部依赖 `npu_turbo` 二进制(全局抬频)。

## 2. 架构

```
Controller.tick (interval):
  straggler.Fetch(straggler_url)              # HTTP GET → 响应体(profiler doc)
     → ParseLatestRound(body)                 # 选最新轮次(§4):最晚 started_at,平局大 id
     → [轮次过期?] age > straggler_max_age → 仅 CleanAll,不注入(§7)
     → [轮次去重?] (id, started_at) == 上次完整应用 → 跳过全部 actuation(§7)
     → FreqProvider.DeviceFreqs()             # 读 snapshot_npu.json:
                                               #   A = aicore_freq(每 device,全局 id 对齐)
                                               #   rated = aicore_rated_freq → 查 ratedMaxBoost 表 → M
     → ① CleanAll(原生 DSMI): 逐设备 关idle→设各自额定→开idle
     → ② B>额定 组: npu_turbo -f maxB(全局抬频,唯一 exec)
                     + 逐个非目标 关idle→设各自额定→开idle
     → ③ B≤额定 组(爬坡): 逐张 关idle→设B(不重开 = 钉住)
   emitMetrics → sink(/metrics + snapshot_nputurbo.json + jsonl)
```

依赖:`internal/source/straggler`(HTTP GET fetch)、`features/nputurbo/dvfs`(CGo+dlopen 绑定 `libdrvdsmi_host.so`,原生 DVFS)、`internal/source/npu_turbo`(exec `npu_turbo -f <MHz>` 全局抬频)、`features/snapshot`(只读 `snapshot_npu.json`)、`internal/metrics`(Filter)。A/M 来自 daemon 产出的快照——**启用前置:`snapshot.enabled: true`**(daemon 是唯一 snapshot 生产者;`startNputurbo` 校验,不满足不启动)。另需节点上有 Ascend driver(`libdrvdsmi_host.so`)与 `npu_turbo` 二进制(路径 `npu_turbo_bin`,仅 B>额定 时需要)。

## 3. 输入获取(straggler,HTTP)

控制器每 tick:① HTTP GET `straggler_url`(带 `straggler_timeout`);② `ParseLatestRound` 解析响应体(§4)。响应体格式与原 straggler 输出一致(`profiler.node_result`;顶层 `kpi` 块由 `json.Unmarshal` 自动忽略,§4),新版本顶层多 `id`(轮次号)与 `started_at`(轮次开始时间)。

- HTTP 失败(超时 / 非 2xx / 网络错 / DNS)→ 本周期**完全 no-op**(不调频、**不 clean**——拿不到清单无法对齐 desired state)。
- 响应体为空 / 全行解析失败 → 完全 no-op。
- 解析成功但 `node_result` 为空 list(无慢卡)→ **不 inject**,但按 §7 clean(所有曾 boost 的卡恢复)。
- 选中轮次 `started_at` 距今 > `straggler_max_age`(YAML 写 `0s` 关闭;doc 无 `started_at` 不检查)→ **仅 clean**(恢复全部到额定,不注入)——检测器停更后不能按其最后一词永久 boost。
- `straggler_url` 为空 → nputurbo 不启动(与 `enabled:false` 同效,启动日志报错)。

straggler 接口由 daemon HTTP GET 获取,与本仓库 `features/stragglerout/` 无关(后者是 KPI 输出,不影响本特性)。

## 4. 解析器(对 jsonl / 单 doc 都健壮)

straggler 写法可能是"一行整份 profiler doc"或"整文件一整份 JSON(可能 pretty-print)"。解析器:① 先 `json.Unmarshal(整个文件)`,成功 → 即该轮;② 失败 → 按行 split,每非空行 `json.Unmarshal`,在**全部成功行中选最新轮次**(规则见下);③ 零成功行 → 返回 error(调用方 no-op)。

**选轮规则**(`laterRound`):候选中带 `started_at` 的 doc 胜过不带的;带时间戳者取**最晚 `started_at`**,平局取**大 `id`**;全无 `started_at` 退化为取**末行**(旧格式追加序即最新)。`id` 单独不可作序——检测器进程重启后 id 从 1 重计,而 `started_at` 不会重置。

数据结构(原嵌套,`npu` 为 list;顶层 `id`/`started_at` 为 2026-10 新增,缺失容忍):

```json
{"id": 2, "started_at": "2026-10-09T08:57:15Z", "profiler": {"node_result": [{"hostname": "master", "npu": [{"id": 1, "cal": {"score": 1.3225}}]}], "comm_domain_result": {}}}
```

展平:遍历 `node_result` 每 node → 遍历 `node.npu` 每卡 → `SlowCard{Hostname, ID, Score}`;连同 `id`/`started_at` 组成 `Round{ID, StartedAt, Cards}`。hostname **不过滤**(全部纳入,假定文件为单节点或运行方按节点部署)。`id` = **全局 device id**(`npu_id × chips_per_card + chip_id`,即 stragglerout 喂给 straggler 检测器、检测器原样报回的编号),既用于 inject `-d <id>`,也用于与 `snapshot_npu.json` 对齐取 A/rated(见 §5)。注意区分:顶层 `id` 是**检测轮次号**,`npu[].id` 才是 device id。

## 5. 公式

`B = round_step(min(A × score, M), step=50)`(四舍五入到 50MHz,ties go up)。

- **升频门槛:`score > 1`(唯一)。** score>1 且 A≤M → 计算 B 并注入(见 §7 轮次去重模型——同一轮次不重复应用,新轮次恢复全部后按清单重新注入)。
- `A` = 该 device 实时 `aicore_freq`(MHz,从 `snapshot_npu.json` 读取,按**全局 device id** `npu_id × chips_per_card + chip_id` 对齐 straggler id;`chips_per_card` 由快照 `max(chip_id)+1` 推导:单芯片 1、A3 双芯片 2,单芯片节点 device id == npu_id)。**已知且接受的爬坡行为(2026-09-14 确认)**:闲置降频中的卡(如 A≈820)第一针 B = round50(A×score) 会低于额定(如 1150 < 1800),需 2-3 个周期才收敛到 M——首针可能短暂低于负载下的自然频率,不改(保留以实时频率为基数的语义,未改为按额定/直接封顶)。
- `M` = 静态映射表 `ratedMaxBoost[rated]`,`rated` 为该卡 `aicore_rated_freq`。当前仅 `1800 → 1850`。**未映射的额定频率 → 该卡跳过不 boost**(无 fallback,不用配置兜底)。
- **`A > M` → 不注入**(该卡每轮仍会被 CleanAll 恢复到额定,但不再往上加)。
- `step_mhz` = 50(配置)。

## 6. 执行器(actuator,原生集成)

actuator 持有两个执行源:**原生 DVFS**(`features/nputurbo/dvfs`,CGo+dlopen `libdrvdsmi_host.so`,原 dvfs.py 语义)与**全局抬频**(`internal/source/npu_turbo`,exec `npu_turbo -f <MHz>`)。三个操作:

- **`CleanAll()`**(原生):逐设备 关idle → 设**该设备自己的**额定(原脚本用 device 0 额定统一恢复,已修)→ 开idle。单设备失败记 warn 继续,不中断其余设备。
- **`BoostAbove(ctx, targetIDs, freq)`**(全局抬频 + 降其余):① exec `npu_turbo -f freq` 全局抬频(失败即中止,不降其余);② 枚举全部设备,非目标逐个 关idle→设各自额定→开idle(单点失败 warn 继续)。目标设备由抬频工具负责,actuator 不再触碰。
- **`BoostAtOrBelow(devID, freq)`**(原生钉频):关idle → 设 freq(B≤额定),不重开 idle = 钉住(原 case-1 语义)。
- **`Available()`** = 原生 DVFS 库可用(clean/钉频的硬依赖);**`TurboAvailable()`** = `npu_turbo` 二进制可执行(B>额定批量的软依赖,缺失时 clean/钉频仍可用,启动仅 WARN)。
- **`Ok()`** = 最近一次操作是否成功(失败置 false)。

npu_dvfs 的 DSMI 调用逐行对应 dvfs.py:关/开 idle = `dsmi_set_device_info(dev, main=8, sub=11, {idle_switch})`;设频 = `dsmi_set_device_info(dev, main=8, sub=12, {type=0(STRESS_ADJ_AIC), set_restore=2(STRESS_FREQ_SET), value})`;查额定 = `dsmi_get_device_frequency(dev, 9)`;枚举 = `dsmi_get_device_count` + `dsmi_list_device`。构建机无需 NPU 环境(运行时 dlopen),非 Linux/非 CGo 构建回退不可用 stub。

## 7. 调频策略(轮次去重 + 防过期)

跨轮状态只剩一个:**上次完整成功应用(或 dry_run 下完整报告)的轮次身份** `lastKey = (id, started_at)`(裸 id 会在检测器重启后撞车,二元组不会——新轮次 `started_at` 必然更新)。每 tick 有效 plan 后按分支:

| 情形 | 行为 |
|---|---|
| 新轮次(身份变化,含重启后同 id 不同 `started_at`) | 走 ①→②→③ 完整流程,全步成功后记录身份 |
| 同身份重复获取(结果未更新) | **跳过全部 actuation**(仅日志+指标;pin 在硬件上持续生效) |
| 同身份但上次任一步失败 | 身份未记录 → 本 tick **重试**同轮次(自愈) |
| 轮次过期(`age > straggler_max_age`) | **仅 `CleanAll`** 恢复额定、不注入、不记录身份;每 tick 重复(自愈) |
| doc 缺 `id` 或 `started_at`(旧格式) | 不去重不检查 → 每 tick 完整流程(原"无状态每轮重置"行为,零影响) |
| plan 失败(拿不到/解析失败/快照空) | 整轮完全 no-op(不 clean 不注入:拿不到输入不动硬件) |

完整流程(新轮次):

1. **`CleanAll`**——恢复**所有**设备到各自额定。这一步天然处理了一切"恢复"场景:score≤1/从清单消失的卡恢复完就停在额定,无需专门判定。clean 失败仅记日志,不阻断后续注入(但计入"任一步失败",本轮次不记录身份 → 下 tick 重试)。
2. **B>额定 组,一次批量**——`BoostAbove(组内全部 id, maxB)`。当前映射表 {1800→1850} 下组内所有 B 恒等于 M(上限与额定差 ≤ 一个步进,(1800,1850] 内 50 倍数只有 1850),单次抬频频率天然覆盖全组。**必须在钉频之前执行**——批量的"降其余到额定"会覆盖先做的钉频。
3. **B≤额定 组(爬坡),逐张钉频**——`BoostAtOrBelow(id, B)`,原生调用无副作用、互不影响。

- 清单为空 / 全部跳过 → 仅 CleanAll(空清单轮次同样参与去重:同身份的空轮次不重复 clean)。
- shutdown → `CleanAll()`;`restore_on_shutdown: false` 可关。
- 稳态代价:同轮次内零硬件写(去重);新轮次每轮经历一次 额定→maxB 的秒级重建(1 次 CleanAll + 1 次批量抬频 + k 次钉频调用;DSMI 原生调用为微秒级,主要耗时是 npu_turbo 一次 exec)。
- 已知取舍:同轮次跳过期间,若有人手动改某设备频率,同轮次内不纠正(到下一轮次或过期才被 CleanAll 重置)——用去重换 DSMI 写次数减少的代价。

## 8. 配置

```yaml
snapshot:
  enabled: true              # 前置:daemon 产出 snapshot_npu.json
  dir: /var/lib/catmonitor/snapshot

nputurbo:
  enabled: false
  interval: 60s
  straggler_url: "http://127.0.0.1:15432/straggler/result/latest"   # GET → profiler doc(顶层 kpi 块自动忽略)
  straggler_timeout: 10s                                    # HTTP GET 超时
  straggler_max_age: 15m                                    # 轮次过期阈值:age > 此值 → 仅恢复额定不注入;0s 关闭(默认 15m;duration 必须带单位,裸 0 解析报错)
  npu_turbo_bin: "/home/jw/npu_turbo"                        # 全局抬频二进制(exec: <bin> -f <MHz>),仅 B>额定 需要
                                                                # 注意:必须绝对路径,且 lptest 助手与二进制同目录(见 §12)
  npu_turbo_timeout: 120s
  step_mhz: 50              # 取整步长
  dry_run: true             # 默认 judge+log;actuate 需 dry_run: false
  restore_on_shutdown: true
```

频率控制为原生 DSMI,无脚本/python/命令模板配置;`npu_turbo_cmd`/`npu_turbo_clean_cmd` 已删除(旧 YAML 残留被 yaml.v3 忽略,无害)。M 不在配置里——来自静态映射表 `ratedMaxBoost`(代码内,加条目改代码),只用于 B 封顶与 `A>M` 跳过判定。启用前置:`snapshot.enabled: true`(A/rated 都从 `snapshot_npu.json` 读)+ 节点有 Ascend driver(`libdrvdsmi_host.so`)+ `npu_turbo_bin` 就位(仅 B>额定 需要)。建议把 `nputurbo` 加入 `features:` 列表,使其 `metrics.yaml` 把 `npu.aicore_freq`/`npu.aicore_rated_freq`(输入)与 `nputurbo` 组件指标(输出)带入采集范围——features=[nputurbo] 单独成立时特性域自足。

**频率缺失行为**:单卡在快照中无 `aicore_freq`/`aicore_rated_freq` → 跳过该卡 + warn(每轮仍会被 CleanAll 恢复);整个 `snapshot_npu.json` 不可读(freqs map 为空)且清单非空 → 本周期完全 no-op(不 clean 不注入,拿不到输入不动硬件)。

## 9. 指标(feature-scoped)

`features/nputurbo/metrics.yaml` 声明 `nputurbo` 组件(`boost_active`/`boost_count`/`actuator_ok`/`round_id`/`result_age_seconds`,均 High;`round_id` = 选中轮次号,`result_age_seconds` = 结果年龄秒数,缺 `started_at` 发 -1)**及输入** `npu.aicore_freq`/`npu.aicore_rated_freq`(均 Medium;rated 在默认目录为 Low,feature yaml 提升为 Medium 使其在 `min_priority: medium` 下必采)。写 `sink` → 经 `PerCompWriter` 落 `snapshot_nputurbo.json`,经 `CachingStorage` 进 `/metrics`。`configs/metrics.yaml` 的 `nputurbo` 组件段手维护(`gen_metrics_catalog.py` 不覆盖特性产出指标)。

## 10. CLI

`catmonitor nputurbo` — HTTP GET straggler 结果 + 读 `snapshot_npu.json`,逐卡算 B,打印只读预览(面向运维的完整句子;`would_boost` 等内部术语已移除):

```
CATMonitor nputurbo (read-only preview — no frequencies are changed)
  actuator available: true
  npu_turbo binary:   true
  boost caps:         rated 1800 MHz → max 1850 MHz  (step 50 MHz)
  straggler round:   id 2, started 2026-10-09T08:57:15Z (age 5m10s)
  device 1: current 1800 MHz → target 1850 MHz  (score 1.10, rated 1800, cap 1850)
  device 2: skipped — rated 2000 MHz is not in the boost cap map
```

轮次行仅在新格式(有 `id`/`started_at`)时出现;过期时追加 WARNING 行(`the daemon would restore all devices to rated and boost nothing`)。**强制 dry-run 不碰硬件**(镜像 energysave CLI)。actuate 走 daemon `enabled: true` + `dry_run: false`。

## 11. 测试

- `internal/source/straggler/`:fetcher seam 注入假响应,验证 fetch 超时/错误传播。
- `features/nputurbo/dvfs/`:只读冒烟(Available/DeviceIDs/RatedFreq,skip 守卫);**测试绝不调用写操作**(SetAicFreq/CloseIdle/OpenIdle 会动真机硬件)。
- `internal/source/npu_turbo/`:RaiseAll 参数拼接(`<bin> -f <MHz>`)、错误与输出透传。
- `features/nputurbo/input_test.go`:jsonl fixture 覆盖——单行整 doc、pretty-print 整 doc、多行追加、`node_result` 空、多 node 多卡、坏行跳过、全坏返回 error;**轮次元数据**——带 `id`/`started_at` 的 doc 解析、按最晚 `started_at` 选轮、重启场景(同 `id` 不同 `started_at`)、乱序追加(旧大 id 在前新小 id 在后,新 `started_at` 胜)、同 `started_at` 平局取大 id、旧格式(无元数据)回退末行。
- `features/nputurbo/freq_test.go`:映射表查找(1800→1850 命中/未映射 miss)、`SnapshotFreqProvider` 从 snapshot fixture 提取(单芯片 device id==npu_id / A3 双芯片全局 id `npu×2+chip` 每 chip 独立条目 / 缺 rated / 缺 chip_id 回退 npu_id / 缺文件空 map)。
- `features/nputurbo/formula_test.go`:边界(`A×score>M` 截断、取整 50 边界、`score≤1` 跳过、`A>M` 不操作、A=M 封顶)。
- `features/nputurbo/actuator_test.go`(fakeDVFS/fakeRaiser):CleanAll 用**各自额定**(混布 1800/2000)、单设备失败隔离;BoostAbove 先抬频后降非目标、目标不被 dvfs 触碰、抬频失败中止;BoostAtOrBelow 钉住(不重开 idle);Available/TurboAvailable。
- `features/nputurbo/controller_test.go`:clean→批量→钉频**顺序**断言、多卡>额定单次批量、空清单仅 clean、全部跳过仅 clean、score≤1/A>M/缺频率数据不注入、straggler 失败/空 freqs 整轮 no-op、clean 失败仍注入、dry_run 零硬件调用;**轮次语义**——同身份二次 tick 零硬件调用、新轮次(含重启同 id 新 `started_at`)重新执行、actuation 失败后同身份重试、过期仅 CleanAll(`straggler_max_age`=0s 关闭、缺 `started_at` 不检查)、去重跳过仍发指标。
- `features/nputurbo/cli_test.go`:`RunOnce` 强制 dry-run、空清单、straggler 失败报错、skipped 展示、npu_turbo binary 行、轮次行/过期 WARNING 展示。
- 平台签名同步:`GOOS=windows go build ./cmd/catmonitor`(npu_dvfs stub 路径)。
- 运行:`make lint && make test`(无 NPU/GPU 环境 mock + 优雅降级,项目既有约定)。

## 12. 不做 / 已知限制

- 不改 `features/stragglerout/`(无关)。
- 不查 DCMI 采集面(A/M 均来自 `snapshot_npu.json`);执行面走 DSMI(与采集面独立的库)。
- 前置 `snapshot.enabled: true`;未启用时 daemon 不启动 nputurbo(启动日志报错)。
- 慢卡结果改 HTTP GET;`straggler_url` 空则不启动。无认证(纯 GET)。
- 静态映射表只覆盖已知额定频率(当前仅 1800→1850);未映射额定 → 跳过不 boost。
- straggler id 与 `snapshot_npu.json` 的对齐已适配全局 device id(`npu_id × chips_per_card + chip_id`,与 stragglerout 同公式);单芯片节点 device id == npu_id,行为不变。
- **per-device DSMI 设 >额定 不被支持**(驱动限制,原 dvfs.py 作者即为此引入 npu_turbo 全局抬频);全局抬频仍依赖外部 `npu_turbo` 二进制(黑盒,路径 `npu_turbo_bin`)。若未来验证 per-device 可超额定(需真机实验),可去掉该依赖。
- **npu_turbo 按进程 CWD 解析助手文件**(`lptest` 及其生成的 `lptest1..15`,工具用 `ls/cp/turbocfg` 相对路径操作它们)——与二进制所在目录无关。故 exec 时**强制以二进制目录为 CWD**(复刻原包装脚本的 `cd $(dirname $0)`;`npu_turbo.go realRun` 实现)。部署要求:`npu_turbo_bin` 用绝对路径且 `lptest` 与二进制同目录;手工运行时须先 cd 到该目录(否则 DSMI 部分成功、`lptestN turbocfg` 全部 exit 127,与 2026-09-14 节点实测一致)。
- **主从 die 约束**(chip1 ≤ 同卡 chip0)由全局抬频天然满足;若改 per-device 方案需自行处理设置顺序。
- **>额定批量假设组内 B 相同**:当前映射 {1800→1850} 恒成立(上限=额定+50);若未来映射条目差值 >50 步进,组内会出现不同 B,单次 `-f maxB` 会把低 B 目标抬过头,需重新设计(分组多次调用会互相破坏,已验证不可行)。
- 每轮 CleanAll 是全局重置:稳定慢卡每轮经历额定→maxB 的秒级重建(接受);正常卡每轮被设回额定+重开 idle(其 DVFS 空闲降频立即恢复,无长期锁定)。
