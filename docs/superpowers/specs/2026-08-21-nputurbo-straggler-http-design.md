# nputurbo straggler 获取方式改为 HTTP — 设计文档

> 对 `2026-08-18-nputurbo-design.md` §3「输入获取链路」的修订：straggler 慢卡检测结果不再由 daemon 主动 exec 外部 straggler 命令获取，改为定期 HTTP GET 一个可配置接口获取。其余（公式、actuator、恢复策略、指标、CLI）不变。

## 1. 目标

- nputurbo 的慢卡清单来源从「exec straggler 二进制（写文件）」改为「HTTP GET 可配置接口（返回响应体）」。
- 传输层与解析层解耦：HTTP source 只负责取回 body，解析仍由 `features/nputurbo/input.go` 的 `ParseSlowCards` 负责（**响应格式不变**，复用现有解析器）。
- 失败语义与现行 exec 失败一致：HTTP 失败 → 本周期完全 no-op，不 inject、不 clean，已 boost 状态不动。
- 无认证：纯 GET，不带 header/token。

## 2. 背景与动因

- 现状：`Controller.tick` 每 `interval` exec `sh -c "straggler path={path}"`，straggler 二进制把结果写到 `result_path` 文件，controller 再 `os.ReadFile` 读回。
- 动因：straggler 检测能力已服务化，daemon 不必再本地 exec 该二进制；改为定期 GET 一个 HTTP 接口即可拿到同样的结果（格式不变）。简化部署、避免依赖本地二进制与文件中转。

## 3. 当前结构（exec-based，待改）

```
Controller.tick (interval)
  → StragglerSource.Run(ctx, straggler_cmd, result_path)   # exec: sh -c "straggler path={path}"
      ↓ straggler 二进制写结果到 result_path (jsonl)
  → os.ReadFile(result_path)
  → ParseSlowCards(data)
  → ComputeTargetB(1800, score, M, step) → BoostRow
  → reconcile (clean / inject)
```

- `internal/source/straggler/straggler.go`：exec source。`Source{ Available(); Run(ctx,cmdTemplate,resultPath) }`，替 `{path}` 后 `sh -c` exec，副作用写文件。singleton + mock 缝。
- `features/nputurbo/controller.go`：`StragglerSource` 接口同上；`planBoosts` 调 `Run` → `ReadFile` → `ParseSlowCards`。
- 配置：`straggler_cmd`、`result_path`、`straggler_timeout`(60s)。
- 已核实：`stragg.Available()` 无处调用（可删）；`result_path` 仅 nputurbo 用（可删）。

## 4. 新设计（HTTP-based）

### 4.1 接口与 source 层

重写 `internal/source/straggler/straggler.go` 为 HTTP GET 客户端（包路径不变，import 不动）：

```go
type Source interface {
    Fetch(ctx context.Context, url string) ([]byte, error)   // GET url，返回响应体
}
```

- `Default()` 返回 singleton，持有共享 `*http.Client`（无状态，URL 由调用方每次传入，对齐现行 exec source 把 `cmdTemplate`/`resultPath` 作参数传入 Run 的模式）。
- 超时由调用方经 `context.WithTimeout(straggler_timeout)` 注入（per-request），source 不自带固定 Timeout。
- `SetMock(fn func(ctx, url string) ([]byte, error))` + `ResetMock()`：沿用现有 singleton+mock 测试缝模式。
- 删除：`Available()`、`Run`、`{path}` 替换、`os/exec` 相关。
- 纯传输层：不解析响应体（不知 profiler 格式），只返回 body——解析仍由 `ParseSlowCards` 负责。

### 4.2 controller `planBoosts`

```
Controller.tick (interval)
  → StragglerSource.Fetch(ctx, straggler_url)   # HTTP GET → body
  → ParseSlowCards(body)                        # 不变
  → ComputeTargetB(1800, score, M, step) → BoostRow
  → reconcile (clean / inject)
```

- `Run` + `os.ReadFile` 两步合并为一次 `Fetch(ctx, c.cfg.StragglerURL)`。
- `Fetch` 返回 err → `planBoosts` 返回 err → tick 完全 no-op（不动已 boost），与现行 exec 失败一致。
- `os` import：`planBoosts` 不再 `ReadFile`；若 controller 其余处不用 `os` 则删该 import（实现时核对）。

### 4.3 配置（`NputurboConfig`）

| 字段 (yaml) | 动作 |
|---|---|
| `StragglerCmd` (`straggler_cmd`) | **删** |
| `ResultPath` (`result_path`) | **删** |
| `StragglerTimeout` (`straggler_timeout`) | **保留**，语义改为 HTTP GET 超时，默认 **10s** |
| `StragglerURL` (`straggler_url`) | **新增**，HTTP 接口地址 |

### 4.4 启动前置（`nputurbo_linux.go`）

- `straggler_url` 为空 → nputurbo **不启动**（与 `enabled:false` 同效），启动日志报错（非 panic）。
- `toNputurboConfig`：删 `StragglerCmd`/`ResultPath` 映射，加 `StragglerURL`。
- `straggler.Default()` 调用不变（现返回 HTTP source）。

### 4.5 失败语义

- HTTP 失败（超时 / 非 2xx / 网络错 / DNS）→ `Fetch` 返 err → `planBoosts` 返 err → tick 完全 no-op：不 inject、不 clean，已 boost 状态原样保留。
- 与现行 exec 失败语义一致；不加连续失败计数器（YAGNI）。

## 5. 文件清单

- `internal/source/straggler/straggler.go` — 重写为 HTTP GET source（`Fetch` 接口 + `Default/SetMock/ResetMock`）。
- `internal/source/straggler/straggler_test.go` — 重写：`httptest.NewServer` 验证 Fetch 返回 body、非 2xx/超时返错。
- `features/nputurbo/controller.go` — `StragglerSource` 接口改 `Fetch(ctx,url)`；`Config` 结构 `StragglerCmd`/`ResultPath` → `StragglerURL`；`planBoosts` 用 `c.stragg.Fetch(ctx, c.cfg.StragglerURL)` 替 `Run+ReadFile`；核对 `os` import。
- `features/nputurbo/controller_test.go` — `fakeStraggler` 改 `Fetch(ctx)([]byte,error)`；`testConfig` 删 cmd/result_path、加 url。
- `internal/config/config.go` — `NputurboConfig` 删 `StragglerCmd`/`ResultPath`，加 `StragglerURL`；`StragglerTimeout` 默认 60s→10s；默认值块同步。
- `internal/config/config_test.go` — 若有 straggler_cmd/result_path 断言则改（核对）。
- `cmd/catmonitor/nputurbo_linux.go` — `startNputurbo` 加 `straggler_url` 空检查；`toNputurboConfig` 字段映射更新。
- `configs/catmonitor.yaml` — `nputurbo` 块：删 `straggler_cmd`/`result_path`，加 `straggler_url`，`straggler_timeout` 默认示例改 10s。
- `features/nputurbo/nputurbo_SPEC.md` — §3 改 HTTP GET；架构图 `straggler.Run`→`straggler.Fetch`；依赖去 exec 加 net/http；配置示例与 §12 限制更新。
- `AGENTS.md`(本地 gitignored) — 顺手更新 straggler 描述。
- `features/nputurbo/metrics.yaml` — 不动（与 source 无关）。
- `features/nputurbo/input.go`/`formula.go`/`actuator.go`/`cli.go` — 不动（解析/公式/执行器/CLI 均不变）。

## 6. 测试

- source 单测：`httptest.NewServer` 返回固定 profiler doc → `Fetch(ctx, srv.URL)` 返回该 body；返回非 2xx → err；server 慢（sleep）触发 ctx 超时 → err。
- controller：`fakeStraggler` 实现 `Fetch(ctx, url)([]byte,error)` 直接返回 payload（或 err），其余 boost/恢复/幂等/no-op 断言不变（与 source 无关）。
- 配置：`Default().Nputurbo.StragglerURL` 默认空、`StragglerTimeout` 默认 10s。
- 平台签名同步：`GOOS=windows go build ./cmd/catmonitor`。
- 运行：`make lint && make test`。

## 7. 配置示例（改后）

```yaml
nputurbo:
  enabled: false
  interval: 60s
  straggler_url: "http://127.0.0.1:8080/straggler/result"   # 慢卡检测结果 HTTP 接口(GET, 返回 profiler doc)
  straggler_timeout: 10s                                      # HTTP GET 超时
  npu_turbo_cmd: "/home/jw/npu_turbo_one.sh inject -n {id} -f {freq}"
  npu_turbo_clean_cmd: "/home/jw/npu_turbo_one.sh clean"
  npu_turbo_timeout: 120s
  max_freq_mhz: 1900
  step_mhz: 50
  dry_run: true
  restore_on_shutdown: true
```

## 8. 不做 / 已知限制

- 不做认证：纯 GET，无 header/token/TLS。
- 不做重试/连续失败计数：单次失败即 no-op（与现行一致）。
- 不做结果缓存/staleness：每 tick 现取，同现行每 tick 现写。
- 不改公式/解析/actuator/恢复策略/指标/CLI。
- 响应格式须与现行 straggler 输出一致（`{"profiler":{"node_result":[...]}}`），否则 ParseSlowCards 复用失效——此为前置假设。
