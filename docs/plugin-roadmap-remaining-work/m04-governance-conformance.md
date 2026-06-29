# M4：Governance 和 Conformance 未完成工作

返回：[Roadmap 实施计划](../plugin-roadmap-implementation-plan.md)

## 阶段目标

把治理从模型和局部门禁推进为所有高风险操作的统一 release gate，并让 conformance 从声明文件变成可执行验证。

## 未完成工作

1. 补可执行 conformance：
   - protocol-proxy、route、status、rule、middleware 和 governance gate 都要从 `conformance.json` 执行真实输入。
   - invalid config 和 missing secret 不能只依赖 manifest 静态判断，需覆盖运行时 dry-run 结果。
   - 输出稳定 JSON，适合 CI golden 比对。
2. 逐步收紧 missing fixture 策略：
   - 当前兼容历史包的 missing fixture 行为需要保留迁移窗口。
   - release/CI profile 应能把缺失 `conformance.json` 升级为 blocking。
   - strict gate 成为默认发布策略前，需要给已有示例和官方插件补齐 fixture。
3. 完善统一 governance 评估路径：
   - enable、rollback、repository import apply、promotion apply、instrumentation metadata 都必须走同一评估语义。
   - repository apply 的跨节点编排和自动集群级控制面仍需补齐。
4. 补高风险操作证据：
   - review 指纹绑定 artifact、config、scope、rollout、runtime limits、features 和 policy hash。
   - warning override TTL 过期后重新阻断。
   - benchmark 超阈值进入 warning/blocking，override 行为可审计。
5. 补 advisory/quarantine 验收：
   - revoke/quarantine 命中后移除 upstream dispatch 和 extension dispatch。
   - 清空 route cache，停止插件后台任务，runtime 标记为 draining。
   - rollback 到受影响 artifact 必须被阻断。

## 验收

- `go run ./cmd/gateway plugin conformance ...` 有稳定 JSON 输出。
- packaged conformance fixture 失败时，preflight/governance 出现 blocking。
- strict fixture gate 打开时，缺失 `conformance.json` 出现 blocking。
- 高风险 protocol-proxy 未 review 不能在 prod 启用。
- `go test ./internal/pluginmanager ./cmd/gateway ./plugin/api` 通过。

## 回滚边界

- conformance 命令可先作为 CI/开发工具，不改变默认运行路径。
- 新 blocking 项必须有 preview/warning 解释，避免无解释地阻断已有操作。
