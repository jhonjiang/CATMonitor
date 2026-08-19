# nputurbo — NPU 慢卡升频执行器 — 技术规格 (nputurbo_SPEC)

> 文档定位:nputurbo 模块的设计与规格文档。
> 对应代码:`features/nputurbo/`(Go package `nputurbo`,`//go:build linux`,与主项目同一 Go module)。
> 零新依赖:仅用标准库。**默认关闭**:`nputurbo.enabled` 默认 false,且 `dry_run` 默认 true(judge+log,不执行)。

> **修订(2026-08-18):** 调频命令改为 `/home/jw/npu_turbo_one.sh inject -n <id> -f <MHz>`(单卡升频)+ `/home/jw/npu_turbo_one.sh clean`(恢复**所有**卡到基线)。clean 是 all-or-nothing,故 actuator 不再按卡保存原频 / 不再按卡 restore;reconcile 改为"有卡恢复 → clean + 重新 inject 剩余慢卡;无卡恢复 → 仅 inject 新/变卡(不 clean,避免稳定卡抖动)"。

## 1. 目标

对慢卡(跑同一任务比正常卡慢,`score > 1.0`)升频。慢卡清单由**外部 straggler 检测器**产出:daemon 周期 exec `straggler path=<result_path>`,straggler 把检测结果(jsonl)写到 `result_path`;nputurbo 读该文件,对每张慢卡算目标频率 B 并 exec `/home/jw/npu_turbo_one.sh inject -n <id> -f <B>` 升频。清单内消失的卡(已恢复)触发 `clean`(恢复所有卡到基线)+ 对剩余慢卡重新 inject。

## 2. 架构

```
Controller.tick (interval):
  straggler.Run(straggler_cmd, result_path)     # exec straggler, 写 result_path
    → 读 result_path(jsonl) → ParseSlowCards    # 详见 §3
    → readAicoreFreqs(snapshot_npu.json)         # 读每卡当前 A(MHz)
    → 对齐 desired state (id -> B):
        有卡恢复(曾 boost 且不在清单) → clean(恢复全部) + 重新 inject 所有 desired
        无卡恢复 → 仅 inject 新卡 / B 变化的卡(幂等,稳定卡不动)
  emitMetrics → sink(/metrics + snapshot_nputurbo.json + jsonl)
```

依赖:`internal/source/straggler`(exec straggler)、`internal/source/npu_turbo`(exec inject / clean)、`features/snapshot`(读 snapshot_npu.json)、`internal/metrics`(Filter)。

## 3. 输入获取(straggler)

控制器每 tick:① exec `straggler path=<result_path>`(带 `straggler_timeout`);② 读 `result_path` 文件(jsonl);③ 解析(§4)。

- exec 失败 / 非零退出 / 超时 → 本周期**完全 no-op**(不调频、**不 clean**——拿不到清单无法对齐 desired state)。
- exec 成功但文件缺失/全行解析失败 → 完全 no-op。
- 解析成功但 `node_result` 为空 list(无慢卡)→ **不 inject**,但按 §7 clean(所有曾 boost 的卡恢复)。

straggler 是外部命令,与本仓库 `features/stragglerout/` 无关(后者是 KPI 输出,不影响本特性)。

## 4. 解析器(对 jsonl / 单 doc 都健壮)

straggler 写法可能是"一行整份 profiler doc"或"整文件一整份 JSON(可能 pretty-print)"。解析器:① 先 `json.Unmarshal(整个文件)`,成功 → 用其 `profiler.node_result`;② 失败 → 按行 split,每非空行 `json.Unmarshal`,取**最后一行**成功解析的(最新检测结果);③ 零成功行 → 返回 error(调用方 no-op)。

数据结构(原嵌套,`npu` 为 list):

```json
{"profiler":{"node_result":[{"hostname":"work4","npu":[{"id":1,"cal":{"score":1.234}}]}],"comm_domain_result":{}}}
```

展平:遍历 `node_result` 每 node → 遍历 `node.npu` 每卡 → `SlowCard{Hostname, ID, Score}`。hostname **不过滤**(全部纳入,假定文件为单节点或运行方按节点部署);`id` 本地不存在 → 警告并跳过。`id` → NPU card id(对齐 `aicore_freq` 的 `npu_id` 标签 = `cardID`,`dev=0`)。

## 5. 公式

`B = round_step(min(A × score, M), step=50)`(四舍五入到 50MHz,ties go up)。

- `A` = 该卡当前 `aicore_freq`(MHz,从 `snapshot_npu.json` 读)。
- `M` = 配置 `max_freq_mhz`(默认 **1900**,常量,不查 DCMI)。
- `step_mhz` = 50。
- `score ≤ 1` → 跳过(straggler 只记慢卡,正常不会出现)。
- `B ≤ A`(增益 <25MHz 被取整抹平)→ 跳过(无意义升频)。
- `B ≥ M` → 用 M。

## 6. 执行器(actuator)

actuator 暴露两个操作(均经 `internal/source/npu_turbo` exec):

- **`Boost(ctx, id, targetB)`** = exec `/home/jw/npu_turbo_one.sh inject -n {id} -f {targetB}`。成功后记 `lastApplied[id]=targetB`(幂等 + reconcile 用)。**不保存原频**(clean 是 all-or-nothing,无需按卡恢复原值)。
- **`RestoreAll(ctx)`** = exec `/home/jw/npu_turbo_one.sh clean`(恢复**所有**卡到基线)。成功后清空 `lastApplied`。
- **`Available()`** = `exec.LookPath` inject 命令首 token(`/home/jw/npu_turbo_one.sh`),best-effort 提示(CLI 的 `actuator_ok`);真实 exec 失败以错误返回。
- **`Ok()`** = 最近一次 inject/clean 是否成功(失败置 false,下周期重试 self-heal)。

`Boost` 幂等:由 controller 在 `LastApplied(id) != B` 时才调用(见 §7),避免重复 inject。

## 7. 恢复策略(按当前清单对齐 desired state,all-or-nothing clean)

每 tick 解析完清单后,`desired = {id: B}`(仅 `WouldBoost` 卡),`current = actuator.LastAppliedMap()`:

- 若 `current` 中存在不在 `desired` 的 id(**有卡恢复**)且 `len(current) > 0`:
  - `actuator.RestoreAll()`(clean,恢复全部,清空 lastApplied)
  - 对 `desired` 每卡 `actuator.Boost(id, B)`(重新 inject,含仍慢的卡)
- 否则(**无卡恢复**):对 `desired` 每卡,若 `LastApplied(id) != B` → `actuator.Boost(id, B)`(新卡或 B 变化才 inject;稳定卡不动,不 clean)。**不 clean**——避免稳定卡被 clean 抹到基线再重新 inject 的抖动。
- `node_result` 空(无慢卡)→ `desired` 为空,`current` 非空 → "有卡恢复"分支 → clean(全部恢复)+ 无 inject。
- shutdown → `RestoreAll()`(clean 恢复所有,镜像 cpugov 安全默认);`restore_on_shutdown: false` 可关。

> 说明:NPU 升频长期保持只是更多功耗/热量(不像降频损害性能);clean 是 all-or-nothing,故单卡恢复时会连带 clean 仍慢的卡再重新 inject(短暂基线回落后重新升频,60s 周期下可接受)。

## 8. 配置

```yaml
nputurbo:
  enabled: false
  interval: 60s
  straggler_cmd: "straggler path={path}"                          # {path} 替换为 result_path
  result_path: /var/lib/catmonitor/nputurbo/straggler_result.jsonl
  straggler_timeout: 60s
  npu_turbo_cmd: "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}"  # {id}/{freq} 替换
  npu_turbo_clean_cmd: "/home/jw/npu_turbo_one.sh clean"          # 原样执行(恢复全部卡)
  npu_turbo_timeout: 10s
  max_freq_mhz: 1900        # M
  step_mhz: 50              # 取整步长
  dry_run: true             # 默认 judge+log;actuate 需 dry_run: false + root
  restore_on_shutdown: true
```

`{path}/{id}/{freq}` 都 `strings.ReplaceAll` 替换;真实命令换参数名只改配置。启用前置:`snapshot.enabled: true`(读 A 需要),否则 warn + no-op(镜像 energysave)。建议把 `nputurbo` 加入 `features:` 列表,使其 `metrics.yaml` 把 `npu.aicore_freq` 带入采集范围(否则当 `features` 非空时 aicore_freq 可能被过滤出 snapshot)。

## 9. 指标(feature-scoped)

`features/nputurbo/metrics.yaml` 声明:`npu.aicore_freq`(输入)+ `nputurbo` 组件(`boost_active`/`boost_count`/`actuator_ok`,均 High)。写 `sink` → 经 `PerCompWriter` 落 `snapshot_nputurbo.json`,经 `CachingStorage` 进 `/metrics`。`configs/metrics.yaml` 手加 `nputurbo` 组件段(`gen_metrics_catalog.py` 不覆盖特性产出指标,手维护)。

## 10. CLI

`catmonitor nputurbo` — 读 `result_path` + `snapshot_npu.json`,算 B,打印每卡 `id/A/score/B/would_boost`,**强制 dry-run 不 exec inject/clean**(镜像 energysave CLI)。actuate 走 daemon `enabled: true` + `dry_run: false`。

## 11. 测试

- `internal/source/straggler/` + `npu_turbo/`:fetcher seam 注入假 exec,验证 `Run`/`SetFreq`/`Clean` 参数拼接 + 超时 + 非零退出。
- `features/nputurbo/input_test.go`:jsonl fixture 覆盖——单行整 doc、pretty-print 整 doc、多行追加(取末行)、`node_result` 空、多 node 多卡、坏行跳过、全坏返回 error。
- `features/nputurbo/formula_test.go`:边界(`A×score>M` 截断、取整 50 边界、`B≤A` 跳过、`score≤1` 跳过)。
- `features/nputurbo/actuator_test.go`:inject 成功记 lastApplied、inject 失败置 Ok=false 不更新、clean 清空 lastApplied、clean 失败保留状态、`Available` 检查 inject 二进制。
- `features/nputurbo/controller_test.go`:boost 多卡(无 clean)、清单内消失卡 → clean+reinject、`node_result` 空 → clean、score 变化 → 仅 reinject 不 clean、新卡出现 → 仅 inject 不 clean、dry_run 不 exec、straggler 失败完全 no-op(不动已 boost 状态)。
- `features/nputurbo/cli_test.go`:`RunOnce` 强制 dry-run、空清单、straggler 失败报错。
- 平台签名同步:`GOOS=windows go build ./cmd/catmonitor`(现有交叉编译检查覆盖)。
- 运行:`make lint && make test`(无 NPU/GPU 环境 mock + 优雅降级,项目既有约定)。

## 12. 不做 / 已知限制

- 不改 `features/stragglerout/`(无关)。
- 不查 DCMI(A 走 snapshot;若 `snapshot.enabled=false` warn + no-op)。
- `aicore_freq` 需在 nputurbo 的 feature scope 内(否则当 `features` 非空时 `metrics.Filter` 把它从 snapshot 过滤掉 → 读不到 → 跳过该卡)。建议启用时把 `nputurbo` 加入 `features:` 列表。
- 不处理 `score≤1`(straggler 只记慢卡)。
- clean 是 all-or-nothing:单卡恢复会连带 clean 仍慢的卡再重新 inject(可接受;若未来工具支持按卡 clean,可回到按卡 restore 以消除抖动)。
