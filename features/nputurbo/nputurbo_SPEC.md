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

## 1. 目标

对慢卡(跑同一任务比正常卡慢,`score > 1.0`)升频。慢卡清单由**外部 straggler 检测器**产出:daemon 周期 HTTP GET `straggler_url` 拿检测结果(profiler doc);nputurbo 结合 `snapshot_npu.json` 里的实时/额定频率对每张慢卡算目标频率 B 并 exec `/home/jw/npu_turbo_one.sh inject -d <id> -f <B> -r <rated>` 升频(rated = 该卡额定频率)。清单内消失的卡(已恢复)触发 `clean`(恢复所有卡到基线)+ 对剩余慢卡重新 inject。

## 2. 架构

```
Controller.tick (interval):
  straggler.Fetch(straggler_url)              # HTTP GET → 响应体(profiler doc)
     → ParseSlowCards(body)                    # 详见 §3、§4
     → FreqProvider.DeviceFreqs()              # 读 snapshot_npu.json:
                                               #   A = aicore_freq(每 device,全局 id 对齐)
                                               #   rated = aicore_rated_freq → 查 ratedMaxBoost 表 → M
     → 对齐 desired state (device_id -> B):
         有卡恢复(曾 boost 且不在清单/清单内 score≤1) → clean(恢复全部) + 重新 inject 所有 desired
         无卡恢复 → 仅 inject 新卡 / B 变化的卡(幂等,稳定卡不动)
   emitMetrics → sink(/metrics + snapshot_nputurbo.json + jsonl)
```

依赖:`internal/source/straggler`(HTTP GET fetch)、`internal/source/npu_turbo`(exec inject / clean)、`features/snapshot`(只读 `snapshot_npu.json`)、`internal/metrics`(Filter)。A/M 来自 daemon 产出的快照——**启用前置:`snapshot.enabled: true`**(daemon 是唯一 snapshot 生产者;`startNputurbo` 校验,不满足不启动)。

## 3. 输入获取(straggler,HTTP)

控制器每 tick:① HTTP GET `straggler_url`(带 `straggler_timeout`);② `ParseSlowCards` 解析响应体(§4)。响应体格式与原 straggler 输出一致(`profiler.node_result`;顶层 `kpi` 块由 `json.Unmarshal` 自动忽略,§4)。

- HTTP 失败(超时 / 非 2xx / 网络错 / DNS)→ 本周期**完全 no-op**(不调频、**不 clean**——拿不到清单无法对齐 desired state)。
- 响应体为空 / 全行解析失败 → 完全 no-op。
- 解析成功但 `node_result` 为空 list(无慢卡)→ **不 inject**,但按 §7 clean(所有曾 boost 的卡恢复)。
- `straggler_url` 为空 → nputurbo 不启动(与 `enabled:false` 同效,启动日志报错)。

straggler 接口由 daemon HTTP GET 获取,与本仓库 `features/stragglerout/` 无关(后者是 KPI 输出,不影响本特性)。

## 4. 解析器(对 jsonl / 单 doc 都健壮)

straggler 写法可能是"一行整份 profiler doc"或"整文件一整份 JSON(可能 pretty-print)"。解析器:① 先 `json.Unmarshal(整个文件)`,成功 → 用其 `profiler.node_result`;② 失败 → 按行 split,每非空行 `json.Unmarshal`,取**最后一行**成功解析的(最新检测结果);③ 零成功行 → 返回 error(调用方 no-op)。

数据结构(原嵌套,`npu` 为 list):

```json
{"profiler":{"node_result":[{"hostname":"work4","npu":[{"id":1,"cal":{"score":1.234}}]}],"comm_domain_result":{}}}
```

展平:遍历 `node_result` 每 node → 遍历 `node.npu` 每卡 → `SlowCard{Hostname, ID, Score}`。hostname **不过滤**(全部纳入,假定文件为单节点或运行方按节点部署)。`id` = **全局 device id**(`npu_id × chips_per_card + chip_id`,即 stragglerout 喂给 straggler 检测器、检测器原样报回的编号),既用于 inject `-d <id>`,也用于与 `snapshot_npu.json` 对齐取 A/rated(见 §5)。

## 5. 公式

`B = round_step(min(A × score, M), step=50)`(四舍五入到 50MHz,ties go up)。

- **升频门槛:`score > 1`(唯一)。** score>1 且 A≤M → 计算 B 并 inject,即使 B=A(已 boost 到目标后快照 A 已反映 inject 结果,B 被封顶回 M=A)——卡持续留在 desired 集合,靠 `LastApplied==B` 幂等跳过,不重复 exec。
- `A` = 该 device 实时 `aicore_freq`(MHz,从 `snapshot_npu.json` 读取,按**全局 device id** `npu_id × chips_per_card + chip_id` 对齐 straggler id;`chips_per_card` 由快照 `max(chip_id)+1` 推导:单芯片 1、A3 双芯片 2,单芯片节点 device id == npu_id)。
- `M` = 静态映射表 `ratedMaxBoost[rated]`,`rated` 为该卡 `aicore_rated_freq`。当前仅 `1800 → 1850`。**未映射的额定频率 → 该卡跳过不 boost**(无 fallback,不查 DCMI、不用配置兜底)。
- **`A > M` → 不做任何操作**(绝不降频;卡仍算慢,不触发 clean)。
- `step_mhz` = 50(配置)。

## 6. 执行器(actuator)

actuator 暴露两个操作(均经 `internal/source/npu_turbo` exec):

- **`Boost(ctx, id, targetB, ratedMHz)`** = exec `/home/jw/npu_turbo_one.sh inject -d {id} -f {targetB} -r {ratedMHz}`。成功后记 `lastApplied[id]=targetB`(幂等 + reconcile 用;rated 不入记忆)。**不保存原频**(clean 是 all-or-nothing,无需按卡恢复原值)。
- **`RestoreAll(ctx)`** = exec `/home/jw/npu_turbo_one.sh clean`(恢复**所有**卡到基线)。成功后清空 `lastApplied`。
- **`Available()`** = `exec.LookPath` inject 命令首 token(`/home/jw/npu_turbo_one.sh`),best-effort 提示(CLI 的 `actuator_ok`);真实 exec 失败以错误返回。
- **`Ok()`** = 最近一次 inject/clean 是否成功(失败置 false,下周期重试 self-heal)。

`Boost` 幂等:由 controller 在 `LastApplied(id) != B` 时才调用(见 §7),避免重复 inject。

## 7. 恢复策略(按当前清单对齐 desired state,all-or-nothing clean)

每 tick 解析完清单后构建 `desired = {id: B}`(score>1 且频率可处理的每张卡,含已在目标的卡),`current = actuator.LastAppliedMap()`:

- **恢复判定**:current 中的卡**不在清单里,或清单内 score≤1(等价于不在清单)** → 有卡恢复。因缺频率数据/额定未映射/A>M 被跳过的卡**仍算慢**,不触发恢复。
- 若 `current` 中存在恢复的 id 且 `len(current) > 0`:
  - `actuator.RestoreAll()`(clean,恢复全部,清空 lastApplied)
  - 对 `desired` 每卡 `actuator.Boost(id, B)`(重新 inject,含仍慢的卡)
- 否则(**无卡恢复**):对 `desired` 每卡,若 `LastApplied(id) != B` → `actuator.Boost(id, B)`(新卡或 B 变化才 inject;已 boost 到目标的卡 `LastApplied==B` 幂等跳过,零 exec)。
- `node_result` 空(无慢卡)→ `desired` 为空,`current` 非空 → "有卡恢复"分支 → clean(全部恢复)+ 无 inject。
- shutdown → `RestoreAll()`(clean 恢复所有,镜像 cpugov 安全默认);`restore_on_shutdown: false` 可关。

> 说明:NPU 升频长期保持只是更多功耗/热量(不像降频损害性能);clean 是 all-or-nothing,故单卡恢复时会连带 clean 仍慢的卡再重新 inject(短暂基线回落后重新升频,60s 周期下可接受)。

## 8. 配置

```yaml
snapshot:
  enabled: true              # 前置:daemon 产出 snapshot_npu.json
  dir: /var/lib/catmonitor/snapshot

nputurbo:
  enabled: false
  interval: 60s
  straggler_url: "http://127.0.0.1:15432/straggler/result"   # GET → profiler doc(顶层 kpi 块自动忽略)
  straggler_timeout: 10s                                    # HTTP GET 超时
  npu_turbo_cmd: "/home/jw/npu_turbo_one.sh inject -d {id} -f {freq} -r {rated}"  # {id}/{freq}/{rated} 替换(缺哪个占位符则不替换)
  npu_turbo_clean_cmd: "/home/jw/npu_turbo_one.sh clean"          # 原样执行(恢复全部卡)
  npu_turbo_timeout: 120s
  step_mhz: 50              # 取整步长
  dry_run: true             # 默认 judge+log;actuate 需 dry_run: false + root
  restore_on_shutdown: true
```

`{id}/{freq}/{rated}` 都 `strings.ReplaceAll` 替换;真实命令换参数名只改配置。M 不在配置里——来自静态映射表 `ratedMaxBoost`(代码内,加条目改代码),只用于 B 封顶与 `A>M` 跳过判定,不传给脚本;`max_freq_mhz` 配置项已删除(旧 YAML 残留该字段被 yaml.v3 忽略,无害)。启用前置:`snapshot.enabled: true`(A/rated 都从 `snapshot_npu.json` 读)。建议把 `nputurbo` 加入 `features:` 列表,使其 `metrics.yaml` 把 `npu.aicore_freq`/`npu.aicore_rated_freq`(输入)与 `nputurbo` 组件指标(输出)带入采集范围——features=[nputurbo] 单独成立时特性域自足。

**频率缺失行为**:单卡在快照中无 `aicore_freq`/`aicore_rated_freq` → 跳过该卡 + warn(仍算慢、不触发 clean);整个 `snapshot_npu.json` 不可读(freqs map 为空)且清单非空 → 本周期完全 no-op(不 clean 不 inject,拿不到输入不动硬件)。

## 9. 指标(feature-scoped)

`features/nputurbo/metrics.yaml` 声明 `nputurbo` 组件(`boost_active`/`boost_count`/`actuator_ok`,均 High)**及输入** `npu.aicore_freq`/`npu.aicore_rated_freq`(均 Medium;rated 在默认目录为 Low,feature yaml 提升为 Medium 使其在 `min_priority: medium` 下必采)。写 `sink` → 经 `PerCompWriter` 落 `snapshot_nputurbo.json`,经 `CachingStorage` 进 `/metrics`。`configs/metrics.yaml` 的 `nputurbo` 组件段手维护(`gen_metrics_catalog.py` 不覆盖特性产出指标)。

## 10. CLI

`catmonitor nputurbo` — HTTP GET straggler 结果 + 读 `snapshot_npu.json`,逐卡算 B,打印只读预览(面向运维的完整句子;`would_boost` 等内部术语已移除):

```
CATMonitor nputurbo (read-only preview — no frequencies are changed)
  actuator available: true
  boost caps:         rated 1800 MHz → max 1850 MHz  (step 50 MHz)
  device 1: current 1800 MHz → target 1850 MHz  (score 1.10, rated 1800, cap 1850)
  device 2: skipped — rated 2000 MHz is not in the boost cap map
```

**强制 dry-run 不 exec inject/clean**(镜像 energysave CLI)。actuate 走 daemon `enabled: true` + `dry_run: false`。

## 11. 测试

- `internal/source/straggler/` + `npu_turbo/`:fetcher seam 注入假 exec,验证 `Run`/`SetFreq`/`Clean` 参数拼接 + 超时 + 非零退出。
- `features/nputurbo/input_test.go`:jsonl fixture 覆盖——单行整 doc、pretty-print 整 doc、多行追加(取末行)、`node_result` 空、多 node 多卡、坏行跳过、全坏返回 error。
- `features/nputurbo/freq_test.go`:映射表查找(1800→1850 命中/未映射 miss)、`SnapshotFreqProvider` 从 snapshot fixture 提取(单芯片 device id==npu_id / A3 双芯片全局 id `npu×2+chip` 每 chip 独立条目 / 缺 rated / 缺 chip_id 回退 npu_id / 缺文件空 map)。
- `features/nputurbo/formula_test.go`:边界(`A×score>M` 截断、取整 50 边界、`B≤A` 跳过、`score≤1` 跳过、A=M 幂等封顶)。
- `features/nputurbo/actuator_test.go`:inject 成功记 lastApplied、inject 失败置 Ok=false 不更新、clean 清空 lastApplied、clean 失败保留状态、`Available` 检查 inject 二进制。
- `features/nputurbo/controller_test.go`:注入 fake FreqProvider——boost 多卡(无 clean)、清单内消失卡 → clean+reinject、清单内 score≤1 → 等价消失 → clean+reinject、`node_result` 空 → clean、score 变化但 B 不变(封顶)→ 幂等不 reinject、已 boost 到目标(A=M)→ 留在 plan 零 exec、新卡出现 → 仅 inject 不 clean、A>M → 不操作(不 clean 不 inject)、dry_run 不 exec、straggler 失败完全 no-op、freqs 空 map 完全 no-op、单卡缺频率数据跳过、未映射额定跳过、A 取实时频率。
- `features/nputurbo/cli_test.go`:`RunOnce` 强制 dry-run、空清单、straggler 失败报错、skipped 卡展示。
- 平台签名同步:`GOOS=windows go build ./cmd/catmonitor`(现有交叉编译检查覆盖)。
- 运行:`make lint && make test`(无 NPU/GPU 环境 mock + 优雅降级,项目既有约定)。

## 12. 不做 / 已知限制

- 不改 `features/stragglerout/`(无关)。
- 不查 DCMI(A/M 均来自 `snapshot_npu.json`,无直接硬件访问)。
- 前置 `snapshot.enabled: true`;未启用时 daemon 不启动 nputurbo(启动日志报错)。
- 慢卡结果改 HTTP GET:不再 exec straggler 二进制、不再有 `result_path` 文件中转;`straggler_url` 空则不启动。无认证(纯 GET)。
- 不处理 `score≤1`(straggler 只记慢卡)。
- 静态映射表只覆盖已知额定频率(当前仅 1800→1850);未映射额定 → 跳过不 boost。
- straggler id 与 `snapshot_npu.json` 的对齐已适配全局 device id(`npu_id × chips_per_card + chip_id`,与 stragglerout 同公式);单芯片节点 device id == npu_id,行为不变。
- clean 是 all-or-nothing:单卡恢复会连带 clean 仍慢的卡再重新 inject(可接受;若未来工具支持按卡 clean,可回到按卡 restore 以消除抖动)。
