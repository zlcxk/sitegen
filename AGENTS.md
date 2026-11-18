# AGENTS.md — 真实数据驱动的开发规则

本文件约束在本仓库中工作的 Coding Agent。目标只有一个：**不要在没有真实 Prometheus 证据的情况下编造指标、标签、Job、Instance、Pod 或容器参数。**

MCP 是开发阶段工具，不是 `sitegen` 运行时依赖。发布后的 CLI 必须只依赖自己的 Go Prometheus Client 独立运行。

---

## 1. 修改真实指标相关代码前，先获取证据

涉及以下内容时，必须先通过 `byok-observability-mcp`（或仓库内置的等价只读查询能力）确认真实环境：

- PromQL 模板或查询表达式；
- 应用 / 组件 Selector；
- `prometheus.labels` 与 `prometheus.targetLabels`；
- Kubernetes Pod 归属、Pod Regex、Container Matcher；
- Dashboard 变量查询；
- 站点 YAML 中的任何真实 Job / Instance / 标签值。

### 1.1 HTTP

先确认以下指标是否存在，以及 `application`、`namespace`、`job`、`method`、`status`、`uri` 等标签的实际结构：

```promql
count by (application, namespace, job) (http_server_requests_seconds_count)
count by (application, namespace, job) (http_server_requests_seconds_sum)
count by (application, namespace, job) (http_server_requests_seconds_bucket)
count by (method, status, uri) (http_server_requests_seconds_count{application="<app>", namespace="<ns>", job="<job>"})
```

### 1.2 JVM

```promql
count by (application, namespace, job, pod, area, id) (jvm_memory_used_bytes)
```

确认 `pod`、`area`、`id` 是否存在；如果不存在，不得在模板或 Selector 中假设。

### 1.3 Kubernetes / cAdvisor / kube-state-metrics

```promql
count by (job) (container_memory_working_set_bytes)
count by (job) (container_cpu_usage_seconds_total)
count by (job) (container_cpu_cfs_throttled_periods_total)
count by (job) (container_cpu_cfs_periods_total)
count by (job) (kube_pod_info)
count by (job) (kube_pod_status_phase)
count by (job) (kube_pod_labels)
```

进一步确认：

- cAdvisor 的实际 `job`（常见为 `kubelet`，但不得假设）；
- `namespace`、`pod` 结构；
- `container`、`image`、`name`、`id` 的实际取值；
- Pod 父级 cgroup、pause 容器、业务容器的标签差异；
- `kube_pod_labels` 是否存在。不存在时不得把它当作有效 Pod 归属配置。

### 1.4 组件

按模板注册表和目标环境确认：

```promql
count by (job, instance) (probe_success)
count by (job, instance) (probe_duration_seconds)
count by (job, instance) (mysql_up)
count by (job, instance) (redis_up)
count by (job) (kafka_brokers)
```

示例清单不是环境承诺。以目标 Prometheus 实际返回为准。

---

## 2. 先检查 MCP 工具定义，再调用

1. 读取 MCP Server 实际暴露的工具列表。
2. 检查具体工具的 JSON Schema（参数名、类型、必填项）。
3. 按实际 Schema 调用，不猜测工具名或参数名。
4. 保存本次关键查询结果作为开发依据。
5. 不得在代码、脚本或文档中虚构 MCP 工具调用。

本环境实际可用的工具名以 MCP Server 返回为准，通常包括：

- `prometheus_health`
- `prometheus_query`
- `prometheus_query_range`
- `prometheus_list_metrics`
- `prometheus_metric_metadata`
- `grafana_health`
- `grafana_list_datasources`
- `grafana_query_metrics`
- `grafana_list_dashboards`
- `grafana_get_dashboard`
- `obs_health_check`

工具名和参数可能随版本变化；调用前必须重新读取 Schema。

---

## 3. 修改结果必须有真实数据证据

提交说明或 PR 描述至少包含：

- 查询的指标名称；
- 实际执行的 Selector / PromQL；
- 返回序列数量；
- 关键标签及其实际取值；
- 哪些配置值来自真实环境；
- 哪些值仍是人工确认项；
- 是否执行了 Grafana 变量展开或 Dashboard 导入验证。

禁止把真实主机 IP、内部地址、Token、密码写入公共测试文件、代码注释、README 或站点 YAML。

---

## 4. MCP 不可用时必须明确降级

MCP 不可用或没有权限时：

- 可以继续完成纯逻辑单元测试；
- 可以使用脱敏、固定的测试 Fixture；
- 不得伪造真实查询结果；
- 不得把 Fixture 测试描述为“真实环境验证通过”；
- 必须在提交说明中标注尚未执行的真实环境验证。

---

## 5. 架构约束

- `sitegen` 不得要求用户安装 MCP；
- 不得把 MCP Client、Token、服务地址写入 `sitegen` 二进制或站点 YAML；
- `.mcp.json` 已被 `.gitignore` 排除，继续保持；
- Prometheus 访问统一复用 `internal/discovery` 的认证、TLS、超时和错误分类；
- Discovery 只读：不得修改 Prometheus、Kubernetes、Grafana 或告警配置；
- 真实环境验证必须区分：
  - 静态 PromQL 语法校验；
  - Prometheus API 实际执行；
  - Grafana 浏览器端变量展开；
  - Grafana Dashboard 导入和面板视觉验证。

前两项不能替代后两项。

---

## 6. Grafana 变量规则

生成 PromQL 时必须使用与语境匹配的变量格式：

| 语境 | 正确格式 |
| --- | --- |
| Prometheus 正则 Matcher | `pod=~"${k8s_pod:regex}"` |
| PromQL 字符串参数 | `label_replace(..., "${app_selector}", ...)` |
| JSON 数据结构 | 仅在确实需要 JSON 时使用 `:json` |
| 原始片段 | 仅在明确语法约束下使用 `:raw` |

禁止：

- 在 PromQL 中使用 `${var:json}`；
- 把 JSON 数组当作 PromQL 字符串字面量；
- 生成 `pod=~${var}` 这类未加引号的正则变量；
- 向 `match[]` 传递任意 PromQL 表达式而不是合法 Series Selector。

---

## 7. 推荐的验证顺序

```bash
# 1. 纯静态
go test ./...
go vet ./...

# 2. 站点配置与模板
./sitegen validate --site sites/<site>.yaml

# 3. 真实 Prometheus
./sitegen check --site sites/<site>.yaml --lookback 1h

# 4. 普通构建
./sitegen build --site sites/<site>.yaml

# 5. 严格构建（CI）
./sitegen build --site sites/<site>.yaml --strict-check

# 6. Grafana 导入后人工/浏览器验证
#    - 切换应用变量
#    - 切换 JVM/Kubernetes Pod
#    - 检查 All
#    - Query Inspector 查看最终 PromQL
#    - 确认没有 parse error
```

只有完成第 6 步，才能在提交说明中写“Grafana 变量链路验收通过”。
