# nputurbo — NPU 慢卡升频执行器 — 设计文档

> 日期：2026-08-18
> 范围：新增 `features/nputurbo/`(linux-only) + `internal/source/straggler/` + `internal/source/npu_turbo/` + `cmd/catmonitor/nputurbo_{linux,other}.go`;改 `cmd/catmonitor/main.go`、`internal/config/config.go`、`configs/catmonitor.yaml`
> 模板：镜像 `features/cpugov/`(controller + actuator + cli + metrics.yaml + SPEC + 平台拆分)
> 约束：单外部依赖 `gopkg.in/yaml.v3` 不动;nputurbo **无 CGo**(频率输入来自 snapshot,actuate 走 exec)

> **修订(2026-08-18,实现期):** 调频命令改为 `/home/jw/npu_turbo_one.sh inject -n <id> -f <MHz>`(单卡升频)+ `/home/jw/npu_turbo_one.sh clean`(恢复**所有**卡)。clean 是 all-or-nothing → actuator 不再按卡保存原频 / 不再按卡 restore;reconcile 改为"有卡恢复 → clean + 重新 inject 剩余慢卡;无卡恢复 → 仅 inject 新/变卡(不 clean,避免稳定卡抖动)"。详见 `features/nputurbo/nputurbo_SPEC.md`(权威规格)。本设计文档下方为初版描述,命令相关细节以上述修订 + SPEC 为准。

## 1. 目标

对慢卡(跑同一任务比正常卡慢)升频。慢卡清单由**外部 straggler 检测器**产出:daemon 周期 exec `straggler path=<result_path>`,straggler 把检测结果(jsonl)写到 `result_path`;nputurbo 读该文件,对每张慢卡算目标频率 B 并 exec `/var/npu_turbo -i <id> -f <B>` 升频。清单内消失的卡(已恢复)自动恢复原频。

## 2. 背景 / 复用

- **cpugov 是近模板**:`features/cpugov/` 有 `controller.go`(控制循环 + snapshot 消费 + 状态机) / `actuator.go`(save orig + 幂等 apply + restore + self-heal) / `cli.go`(`RunOnce` dry-run 预览 + `FormatSnapshot`) / `metrics.yaml`(feature scope) / `*_SPEC.md` / 平台拆分 `cmd/catmonitor/energysave_{linux,other}.go`(签名必须同步,`main.go` 无条件调)。
- **NPU 频率已可读**:`npu` collector 经 DCMI 产出 `aicore_freq`(`freqAICoreCurrent=7`,MHz,标签 `npu_id=cardID`)与 `aicore_rated_freq`(`freqAICoreMax=9`)→ 落 `snapshot_npu.json`。nputurbo 从 snapshot 读 A,**无需 CGo**。
- **exec 源模板**:`internal/source/npu_smi/` = 单例 + `fetcher` seam + 5s 超时 + `Available()` via `LookPath`。`straggler` / `npu_turbo` 两源照此。
- **存储链 / sink**:`runDaemon` 末态 `sink`(经 `PerCompWriter → CachingStorage → JSONLStorage`)。nputurbo 的 `nputurbo.*` 状态指标写 `sink` → 进 `/metrics` + `snapshot_nputurbo.json` + jsonl(镜像 energysave 路由)。

## 3. 输入获取链路(straggler)

控制器每 tick:

1. exec `straggler path=<result_path>`(带 `straggler_timeout`,超时 → no-op)。
2. exec 失败 / 非零退出 → 日志 + no-op(不调频、不恢复)。
3. 读 `result_path` 文件,按 §4 解析为慢卡清单。
4. straggler exec 成功但文件缺失/全行解析失败 → 完全 no-op(拿不到清单无法对齐 desired state,不调频不恢复,等下周期重试)。
5. 解析成功但零慢卡(`node_result` 为空 list 或所有 `npu` 空)→ **不调频**,但按 §7 对"曾 boost 且不在清单"的卡 restore(node_result 空 ⇒ 全部 restore)。

straggler 是外部命令,与本仓库 `features/stragglerout/` 无关(后者是 KPI 输出,不影响本特性)。

## 4. 解析器(对 jsonl / 单 doc 都健壮)

straggler 写法可能是"一行整份 profiler doc"或"整文件一整份 JSON(可能 pretty-print)"。不依赖猜测:

1. 先 `json.Unmarshal(整个文件, &doc)`;成功 → 用该 `doc.Profiler.NodeResult`。
2. 失败 → 按 `\n` split,每非空行 `json.Unmarshal` 为同形 doc,取**最后一行**成功解析的(最新检测结果)。
3. 零成功行(全行解析失败)→ §3 步骤 4:完全 no-op。若至少一行成功但其 `node_result` 为空 → §3 步骤 5:不调频,restore 已 boost 卡。

数据结构(原嵌套,`npu` 为 list):

```json
{"profiler":{"node_result":[{"hostname":"work4","npu":[{"id":1,"cal":{"score":1.234}}]}],"comm_domain_result":{}}}
```

展平:遍历 `node_result` 每 node → 遍历 `node.npu` 每卡 → 收集 `SlowCard{Hostname, ID, Score}`。hostname **不过滤**(全部纳入,假定文件为单节点或运行方按节点部署);`id` 本地不存在 → 警告并跳过。

`id` → NPU card id(对齐 `aicore_freq` 的 `npu_id` 标签 = `cardID`,`dev=0`)。

## 5. 公式

`B = round_step(min(A × score, M), step=50)`(四舍五入到 50MHz)。

- `A` = 该卡当前 `aicore_freq`(MHz,从 `snapshot_npu.json` 读)。
- `M` = 配置 `max_freq_mhz`(默认 **1900**,常量,不查 DCMI)。
- `step_mhz` = 50。
- `score ≤ 1` 不会出现(straggler 只记慢卡);若出现 → 跳过(不降频)。
- `B ≤ A`(增益 <25MHz 被取整抹平)→ 跳过(无意义升频)。
- `B ≥ M` → 用 M。

## 6. actuator(镜像 cpugov actuator)

- 每卡首次 boost 时保存原频 `savedOrig[id] = A`(从 snapshot 读到的 boost 前值)。
- 幂等:记 `lastApplied[id] = B`;目标==上次 → 跳过 exec(仿 cpugov 比较后写)。
- 目标变(B 不同)→ 重新 apply(覆盖 `lastApplied`;`savedOrig` 不动,始终是 boost 前原值)。
- exec `/var/npu_turbo -i <id> -f <B>`(带 `npu_turbo_timeout`);失败 → 日志,`actuator_ok=false`,本周期不更新 `lastApplied`(下周期重试,self-heal)。

## 7. 恢复策略(最终,按当前清单对齐 desired state)

每 tick 解析完清单后:

- 清单内每卡 → §5/§6 boost 到 B(幂等 / 目标变则重 apply)。
- **清单外曾被 boost 的卡 → restore `savedOrig[id]` 原频,清 `lastApplied`/`savedOrig`**。
- `node_result` 空(无慢卡)→ 所有曾 boost 的卡都"消失" → 全部 restore(自然推论)。
- shutdown → restore 所有剩余已 boost 卡(镜像 cpugov `Restore()`)。

> 说明:NPU 升频长期保持只是更多功耗/热量(不像降频损害性能),但恢复原频仍是干净语义 + shutdown 安全默认。`restore_on_shutdown: false` 可关。

## 8. 配置

```yaml
nputurbo:
  enabled: false
  interval: 60s
  straggler_cmd: "straggler path={path}"           # {path} 替换为 result_path
  result_path: /var/lib/catmonitor/nputurbo/straggler_result.jsonl
  straggler_timeout: 60s
  npu_turbo_cmd: "/var/npu_turbo -i {id} -f {freq}"  # {id}/{freq} 替换
  npu_turbo_timeout: 10s
  max_freq_mhz: 1900        # M
  step_mhz: 50              # 取整步长
  dry_run: true             # 默认 judge+log;actuate 需 dry_run: false + root
  restore_on_shutdown: true
```

`{path}/{id}/{freq}` 都 `strings.ReplaceAll` 替换;真实命令换参数名只改配置。启用前置:`snapshot.enabled: true`(读 A 需要),否则 warn + no-op(镜像 energysave)。

## 9. 文件清单

| 文件 | 职责 |
|------|------|
| `internal/source/straggler/straggler.go`(+`_test.go`) | exec 源:`Available()`(LookPath `straggler`)、`Run(resultPath) error`(exec `straggler path=<resultPath>`,超时)。fetcher seam 供测试(镜像 `npu_smi`) |
| `internal/source/npu_turbo/npu_turbo.go`(+`_test.go`) | exec 源:`Available()`(LookPath `/var/npu_turbo`)、`SetFreq(cardID, freqMHz int) error`。fetcher seam |
| `features/nputurbo/controller.go` | tick:straggler `Run` → 解析 jsonl → 读 `snapshot_npu.json` 取每卡 A → 算 B → actuator → emit 指标。`Config`/`Storage`(子集)/`NewController`/`Run`/`Restore`/`RunOnce` |
| `features/nputurbo/input.go` | §4 健壮解析器:`[]byte → []SlowCard{Hostname,ID,Score}` |
| `features/nputurbo/actuator.go` | §6/§7:按卡 save orig + 幂等 apply + reconcile restore + shutdown restore |
| `features/nputurbo/cli.go` | `RunOnce`(强制 `DryRun=true`) + `FormatSnapshot` |
| `features/nputurbo/metrics.yaml` | feature scope:`npu.aicore_freq`(输入)+ `nputurbo.*`(target_freq_mhz/current_freq_mhz/boost_active/actuator_ok/last_score/boost_count) |
| `features/nputurbo/nputurbo_SPEC.md` | 规格 |
| `features/nputurbo/*_test.go` | controller/actuator/解析器/公式 单测,mock 两源 + 本地 jsonl fixture |
| `cmd/catmonitor/nputurbo_linux.go` + `nputurbo_other.go` | 平台拆分,签名同步(`main.go` 无条件调) |
| `cmd/catmonitor/main.go`(改) | 新 `nputurbo` 子命令 + `runDaemon` 接 `startNputurbo`/`stopNputurbo` |
| `internal/config/config.go`(改) | 新 `NputurboConfig` + `Default()` |
| `configs/catmonitor.yaml`(改) | 新 `nputurbo:` 块 |

## 10. 指标(feature-scoped)

`features/nputurbo/metrics.yaml` 声明:

- `npu.aicore_freq`(输入,Medium/High——确保进 snapshot;若默认目录已 High 则无需提级)。
- `nputurbo` 组件:`target_freq_mhz`、`current_freq_mhz`、`boost_active`、`actuator_ok`、`last_score`、`boost_count`(均 High)。

写 `sink` → 经 `PerCompWriter` 落 `snapshot_nputurbo.json`,经 `CachingStorage` 进 `/metrics`。`configs/metrics.yaml` 手加 `nputurbo` 组件段(`gen_metrics_catalog.py` 不覆盖特性产出指标,手维护)。

## 11. CLI

`catmonitor nputurbo [-i <file>]` — 读 `result_path`(或 `-i` 覆盖)+ `snapshot_npu.json`,算 B,打印每卡 `id/hostname/score/A/B/是否生效`,**强制 dry-run 不 exec npu_turbo**(镜像 energysave CLI)。actuate 走 daemon `enabled: true` + `dry_run: false`。

## 12. 测试

- `internal/source/straggler/` + `npu_turbo/`:fetcher seam 注入假 exec,验证 `Available`/`Run`/`SetFreq` 参数拼接 + 超时 + 非零退出。
- `features/nputurbo/input_test.go`:jsonl fixture 覆盖——单行整 doc、pretty-print 整 doc、多行追加(取末行)、`node_result` 空、多 node 多卡、`id` 缺失、坏行跳过。
- `features/nputurbo/controller_test.go`:mock 两源 + 本地 snapshot;验证——boost 幂等、目标变重 apply、清单内消失卡 restore、清单空全 restore、空 `node_result` no-op、dry_run 不 exec。
- 公式边界单测:`A×score>M` 截断、取整 50 边界、`B≤A` 跳过、`score≤1` 跳过。
- 平台签名同步:`GOOS=windows go build ./cmd/catmonitor`(现有交叉编译检查覆盖)。
- 运行:`make lint && make test`(无 NPU/GPU 环境 mock + 优雅降级,项目既有约定)。

## 13. 不做 / 已知限制

- 不改 `features/stragglerout/`(无关)。
- 不查 DCMI(A 走 snapshot;若 `snapshot.enabled=false` warn + no-op)。
- `aicore_freq` 需在 nputurbo 的 feature scope 内(否则 `metrics.Filter` 把它从 snapshot 过滤掉,nputurbo 读不到 → 报 stale/unknown)。若默认目录已采 `aicore_freq` 则无需提级;否则 nputurbo `metrics.yaml` 声明提级。
- 不处理 `score≤1`(straggler 只记慢卡)。
- DCMI `ResourceInfoFull` stub 等 NPU 已知缺口与本特性无关(本特性不依赖 process 数据)。
