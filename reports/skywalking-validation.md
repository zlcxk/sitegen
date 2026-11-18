# SkyWalking / Grafana 真实环境验证记录

- 验证时间：2026-11-18
- 目标 Grafana：`grafana-dev.rtdgo.tech`，v13.0.12，数据库状态 ok
- 业务 Prometheus：通过 `sitegen check` 实际连接
- SkyWalking Prometheus：Grafana 数据源 `skywalking`（UID 仅存在于本环境配置，不写入模板）
- SkyWalking 原生数据源：`apache-skywalking-datasource`

## 1. APM 服务指标（SkyWalking Prometheus）

使用生成模板中的实际查询，`service=rtd-admin_dev`、`layer=GENERAL`：

| 查询 | 结果 |
| --- | --- |
| `avg(service_apdex{layer="GENERAL", service="rtd-admin_dev"}) / 10000` | 1 个序列，值 1 |
| `avg(service_sla{layer="GENERAL", service="rtd-admin_dev"}) / 100` | 1 个序列，值 100（percent） |
| `avg(service_cpm{layer="GENERAL", service="rtd-admin_dev"})` | 1 个序列，值 13 calls/min |
| `service_resp_time{layer="GENERAL", service="rtd-admin_dev"}` | 1 个序列，值 9 ms |
| `service_percentile{labels="0,1,2,3,4",layer="GENERAL",relabels="P50,P75,P90,P95,P99",service="rtd-admin_dev"}` | 5 个序列：P50=0、P75=10、P90=20、P95=20、P99=40 |

## 2. 实例与 Endpoint

| 查询 | 结果 |
| --- | --- |
| `service_instance_cpm{layer="GENERAL",order="DES",parent_service="rtd-admin_dev",top_n="10"}` | 1 个实例 `rtd-admin-78b95d8c98-6mkww` |
| `service_instance_resp_time{...}` | 同上实例，值 7 ms |
| `endpoint_cpm{layer="GENERAL",order="DES",parent_service="rtd-admin_dev",top_n="10"}` | 3 个 Endpoint（actuator/health/readiness、liveness、prometheus） |
| `endpoint_resp_time{...}` | 3 个 Endpoint，19/1/1 ms |
| `endpoint_sla{...} / 100` | 3 个 Endpoint，均为 100（percent） |

实例标识与真实 Kubernetes Pod 名称一致，可用于实例 Dashboard 下钻。

`user-center-application_test` 的 `service_cpm` 返回 1 个序列；`user-center-application_dev` 当前窗口没有 CPM 序列，Dashboard 应显示空数据，而不是伪造 0 值或错误关联其他服务。

## 3. JVM 模板查询（业务 Prometheus）

从最终 `dashboards/admin-dev.json` 提取 JVM 面板表达式，替换 `$jvm_app_selector` 为 `"rtd-admin-dev"` 后直接执行：

| 面板 | 结果 |
| --- | --- |
| JVM Heap 使用量 | success，1 个序列 |
| JVM Heap 使用率 | success，1 个序列 |
| JVM Non-Heap 使用量 | success，1 个序列 |
| JVM Heap（按内存池） | success，3 个序列 |
| JVM GC 次数 | success，1 个序列 |
| JVM GC 暂停时间 | success，1 个序列 |
| JVM 活跃线程数 | success，1 个序列 |
| JVM 进程 CPU | success，1 个序列 |
| JVM 进程运行时长 | success，1 个序列 |

所有 JVM 查询均不再包含 `pod=~` Matcher，按 application + namespace 聚合。

## 4. Kubernetes Pod 变量错误与修复

旧版导入的 `site-admin-dev` Dashboard 中，`jvm_pod` / `k8s_pod` 变量把字符串 `query_result(...)` 当作 Series Query 发送：

```text
POST /api/datasources/uid/prometheus/resources/api/v1/series
match[]=query_result(( label_replace(...
```

Prometheus 返回：

```json
{"status":"error","errorType":"bad_data","error":"invalid parameter \"match[]\": 1:13: parse error: unexpected \"(\""}
```

修复后 `k8s_pod` 变量序列化为：

```json
"query": {
  "qryType": 3,
  "query": "query_result(...)",
  "refId": "PrometheusVariableQueryEditor-VariableQuery"
}
```

`qryType=3` 是 Grafana Prometheus 数据源的 `VarQueryResult`。但仅设置 `qryType` 还不够：Grafana 的 `PrometheusQueryResultRegex` 是：

```js
/^query_result\((.+)\)\s*$/
```

它没有 `s` 标志，`.` 不匹配换行。原来的变量表达式是多行格式，因此即使 `qryType=3`，也会落入 Series Query 分支并请求：

```text
/api/v1/series?match[]=query_result((...
```

最终修复同时做了两件事：

1. `query` 使用对象 `{"qryType":3,"query":"query_result(...)"}`；
2. 将 `query_result(...)` 压成单行（移除 CR/LF，保留空格），与 Grafana 变量编辑器保存时的 `removeLineBreaks()` 行为一致。

修复后的 Pod 变量内层 PromQL 直接在目标 Prometheus 执行成功，返回应用 Pod 序列。新 Dashboard 需要重新导入后才能在 Grafana 中看到该修复。

## 5. CLI 通过 Grafana 数据源代理执行 APM 检查

利用 Grafana 数据源代理（只读）可以直接用 SkyWalking Prometheus 数据源执行 `sitegen check`：

```bash
SITEGEN_SKYWALKING_PROMETHEUS_TOKEN=*** \
./sitegen check \
  --site sites/admin.yaml \
  --prometheus-url http://10.10.111.214:30001 \
  --skywalking-prometheus-url \
  https://grafana-dev.rtdgo.tech/api/datasources/uid/eg3rhqcz0lvr4d/resources \
  --lookback 1h
```

结果：

- `rtd-admin-dev` 的 `service_apdex`、`service_sla`、`service_cpm`、`service_resp_time`、`service_percentile` 全部 PASS；
- `service_instance_*`、`endpoint_*` 全部 PASS（带 `order` 参数的真实查询）；
- `user-center-application-dev` 当前窗口没有 APM 序列，按设计输出 WARN，不影响业务 Prometheus/JVM/组件检查；
- 原生 Node Graph 保持 WARN，直到在目标 Grafana 实际导入验证。

最终报告：PASS=119 WARN=19 FAIL=0 SKIP=1。

## 6. Grafana 导入状态

当前提供的 Grafana API Token 只有读权限，创建/更新 Dashboard 返回：

```text
Access denied: Permissions needed: any of dashboards:create, dashboards:write
```

因此本次无法自动导入 `dashboards/admin-dev.json`。已完成：

- Grafana Dashboard JSON 结构与变量模型核对；
- 三类数据源实际查询验证（业务 Prometheus、SkyWalking Prometheus、原生拓扑配置）；
- 旧 Dashboard 的 `match[]` 错误复现与根因定位；
- 生成模型的自动化测试；
- CLI 通过 Grafana 数据源代理执行 APM 检查。

待人工完成：使用有 `dashboards:write` 权限的账号导入新 Dashboard，切换 `service`、`jvm_app_selector`、`k8s_app_selector`、`k8s_pod`，并在 Query Inspector 确认最终 PromQL 和 Node Graph 拓扑。

## 7. Dashboard 结构优化决策

- **删除静态文本**：“SkyWalking 服务映射与下钻”和“YAML 配置依赖”不再生成；真实拓扑只保留 SkyWalking Node Graph。
- **删除依赖组件详细静态清单**：MySQL/Redis/Kafka 状态仍在站点健康总览，依赖组件行只保留指向专业 Dashboard 的精简导航。
- **链接专业 Dashboard，而不是复制面板**：
  - SkyWalking Service Dashboard（携带 `$service`、`$layer`、两个数据源变量和当前时间范围）；
  - Kubernetes 集群、Blackbox Prober；
  - MySQL / Redis 专业 Dashboard，组件状态面板也带对应实例参数链接；
  - Kafka / Node Exporter 等可通过 `links.dashboards` 继续追加。
- **域名上下文**：Dashboard 标题、描述和 Blackbox 面板标题均包含域名 URL。
