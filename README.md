# Site Observability Generator（sitegen）

`sitegen` 是一个配置驱动的 Go CLI，用于把已有的 Prometheus、Blackbox Exporter、SkyWalking 和 Kubernetes 监控数据组织成一个**站点级 Grafana 全链路 Dashboard**。

它不采集数据，也不替代 Grafana / SkyWalking / Prometheus，只负责：

- 维护站点、应用、依赖组件之间的显式关系；
- 校验 YAML、指标模板和 Prometheus 数据；
- 根据统一的模板生成可重复、可导入的 Dashboard JSON；
- 输出校验报告，并安全地原子写入文件。

```text
请求 → 站点 → 应用 → 服务调用 → 依赖组件 → 运行环境
```

---

## 1. 快速开始

PRD 要求 Go 1.24+；当前解析的依赖要求 Go 1.27，项目以当前构建环境（Go 1.27）为准。依赖已固定，不使用 CGO。

```bash
# 编译（Windows 产物为 sitegen.exe）
make build

# 初始化目录和工具配置
./sitegen init

# 交互式创建站点
./sitegen create

# 只校验 YAML
./sitegen validate --site sites/order-admin-test.yaml

# 校验 Prometheus 真实指标与 PromQL
./sitegen check --site sites/order-admin-test.yaml

# 仅根据 YAML 生成 Dashboard
./sitegen generate --site sites/order-admin-test.yaml

# 完整校验并生成（配置 + Prometheus + Dashboard）
./sitegen build --site sites/order-admin-test.yaml

# 批量构建；某个站点失败不会中断其他站点
./sitegen build --all
```

产物：

```text
dashboards/<site-id>.json
reports/<site-id>-validation.txt
```

`sitegen init` 会创建 `sites/`、`dashboards/`、`reports/` 和 `sitegen.yaml`。已有文件不会被覆盖。

---

## 2. 目录结构

```text
site-observability/
├── cmd/sitegen/main.go
├── internal/
│   ├── cli/            # Cobra 命令、交互式向导、终端输入
│   ├── config/         # YAML 模型、严格解码、关联校验、原子写入
│   ├── discovery/      # Prometheus API、认证、TLS、超时
│   ├── metrics/        # 模板注册表、选择器、Prometheus 校验
│   ├── dashboard/      # 强类型 Grafana JSON 构建、变量、布局、链接
│   └── output/         # 原子文件写入、校验报告
├── templates/
│   ├── embed.go        # 模板嵌入二进制
│   ├── dashboard/      # 统一 Dashboard 基础模板
│   └── metrics/        # 指标模板（HTTP/JVM/K8s/中间件/Blackbox）
├── schemas/site.schema.json
├── sites/examples/order-admin-test.yaml
├── dashboards/
├── reports/
├── sitegen.example.yaml
├── Makefile
└── go.mod
```

模板通过 `//go:embed` 编译进二进制，运行时不会读取项目外的任意模板文件。

---

## 3. 站点 YAML

最小完整示例见 [sites/examples/order-admin-test.yaml](sites/examples/order-admin-test.yaml)。核心约束：

- 站点 ID、域名 ID、应用 ID、组件 ID **全局唯一**；
- 应用由 `application + namespace` 唯一确定，查询时还会同时使用该应用的 `job` 和全部额外标签；
- 域名的 `entryApplications` 显式声明外部入口应用；应用角色绝不根据名称推断；
- SkyWalking Service 默认自动生成 `{application}_{namespace}`，也可手动指定；
- 依赖必须显式引用已存在的应用或组件 ID，不根据名称推断；
- 所有指标模板 ID 必须存在于模板注册表；
- 未知字段直接报错并给出行号与字段路径；
- YAML 只允许一个文档，必须包含 `apiVersion` / `kind`。

```yaml
apiVersion: site-observability/v1
kind: Site

metadata:
  id: order-admin-test
  name: 订单管理站点
  environment: test
  description: 订单管理测试环境

spec:
  domains:
    - id: admin
      url: https://admin-test.rtdgo.tech
      blackbox:
        job: blackbox-sites
        instance: https://admin-test.rtdgo.tech
      # 只有这里显式声明的应用才会计入站点外部请求量。
      entryApplications: [rtd-admin-test]

  applications:
    - id: rtd-admin-test
      application: rtd-admin
      namespace: test

      prometheus:
        job: rtd-admin
        labels:
          application: rtd-admin
          namespace: test

      skywalking:
        service: rtd-admin_test
        endpointFormat: "{method}:{uri}"

      metrics: [http, jvm, kubernetes]

      kubernetes:
        # cAdvisor 没有 application 标签，必须显式声明 Pod 归属。
        # 标签名必须根据目标 Prometheus 的真实 kube_pod_labels 结构确认。
        podSelector:
          mode: labels
          metric: kube_pod_labels
          matchers:
            label_app: rtd-admin
        # 可选。未配置时会输出 WARN，且不会硬编码 container 过滤条件。
        containerMatchers: {}

      dependencies: [user-center-test, mysql-test, redis-test]

  components:
    - id: mysql-test
      name: MySQL Test
      type: mysql
      prometheus:
        job: scrapeConfig/monitoring/mysql-exporter-test
        instance: "10.10.100.10:3307"
      metrics: [mysql-basic]
```

每个应用对象都以完整 Selector 参与查询：`application`、`namespace`、`job` 和该应用自己的全部额外标签。多应用聚合不会把标签正则简单拼接，而是先按应用对象逐一编译精确 Selector，再用 `or` 合并；因此不会出现 `app-a + dev` 这类配置中不存在的交叉组合。

Kubernetes 指标不使用 Namespace 全量查询。应用必须通过 `kubernetes.podSelector` 声明 Pod 归属，Builder 会使用 `kube_pod_labels`（或经过验证的 `pod-regex` 兼容模式）与 cAdvisor 指标按 `namespace + pod` 关联。未配置 Pod 归属的应用会跳过应用级 Kubernetes 面板并输出 WARN。

JSON Schema 位于 [schemas/site.schema.json](schemas/site.schema.json)，可用于编辑器补全。加载器本身仍会执行严格字段检查和跨对象校验。

---

## 4. CLI 命令

| 命令 | 说明 |
| --- | --- |
| `sitegen init` | 初始化目录和工具配置 |
| `sitegen create` | 交互式创建站点 YAML |
| `sitegen edit --site ...` | 修改已有站点；可保留或重建各列表 |
| `sitegen validate --site ...` | 校验 YAML、ID、依赖、模板引用 |
| `sitegen check --site ...` | 连接 Prometheus 校验指标、标签、Selector、PromQL |
| `sitegen generate --site ...` | 只生成 Dashboard（不访问 Prometheus） |
| `sitegen build --site ...` | 配置 + Prometheus + Dashboard 完整流程 |
| `sitegen list` | 列出 `sites/` 下的站点 |
| `sitegen version` | 版本与模板摘要 |

通用参数：

```text
--config           工具配置路径（默认 sitegen.yaml 或 ~/.sitegen/config.yaml）
--sites-dir        站点目录，默认 sites
--dashboards-dir   Dashboard 输出目录，默认 dashboards
--reports-dir      报告输出目录，默认 reports
--verbose          调试日志
```

`validate` / `generate` / `build` / `check` 支持 `--site FILE` 或 `--all`，两者互斥。批量处理会继续执行后续站点，最终只要有一个失败就返回非零退出码。

`build` 额外支持：

```text
--prometheus-url URL
--timeout 15s
--lookback 1h
--concurrency 4       # 1-16
--skip-prometheus
```

未配置 Prometheus URL 时，`build` 会在报告中标记 `SKIP` 并继续生成；`check` 则会明确失败。需要严格 CI 时请配置 Prometheus 或显式使用 `--skip-prometheus`。

### 交互式创建流程

`sitegen create` 依次收集：

1. 站点元数据；
2. 域名（可多个，至少一个），Blackbox Job / Instance；
3. 应用（可多个），兼容同名应用位于不同 Namespace；
4. 每个应用的 Prometheus Job、额外标签、SkyWalking Service（可自动生成）、Endpoint 格式和指标模板；
5. 依赖组件（MySQL / Redis / Kafka / Nacos / 自定义）；
6. 每个应用的显式依赖关系；
7. 配置预览和写入确认。

配置先完整校验，再通过临时文件 + 原子替换写入；覆盖已有文件时保留 `.bak` 备份。用户中断不会写出半成品。

---

## 5. 指标模板

模板定义在 `templates/metrics/*.yaml`，每个 profile 由多个面板查询组成。PromQL 不散落在站点 YAML 中，也不在 Builder 中大量硬编码。

| Profile | 对象 | 已使用指标 | 说明 |
| --- | --- | --- | --- |
| `blackbox`（自动） | 域名 | `probe_success`, `probe_duration_seconds` | 可用性与探测耗时 |
| `platform`（自动） | 应用 | `up` | 按 Job 展示 Prometheus 指标采集状态（不代表业务健康） |
| `http` | 应用 | `http_server_requests_seconds_count/sum/bucket` | RPS、2xx/3xx/4xx/5xx 比例、毫秒延迟、状态码、慢接口、Top Endpoint |
| `jvm` | 应用 | `jvm_memory_used_bytes` | 仅使用已确认的 Heap 指标，按 Pod 筛选 |
| `kubernetes` | 应用 | `container_cpu_usage_seconds_total`, `container_memory_working_set_bytes`, `container_cpu_cfs_throttled_periods_total`, `container_cpu_cfs_periods_total`, `kube_pod_status_phase`, `kube_pod_info`, `kube_pod_labels` | 通过 `podSelector` 约束 Pod 归属；不硬编码未验证的 container 过滤 |
| `mysql-basic` | MySQL | `mysql_up` | 其他性能指标待真实环境确认 |
| `redis-basic` | Redis | `redis_up` | 其他性能指标待真实环境确认 |
| `kafka-basic` | Kafka | `kafka_brokers` | Broker 数不能代表完整健康；Consumer Lag 后续增强 |
| `nacos-basic` | Nacos | `up` | 仅表示 exporter 抓取可达，不代表 Nacos 集群健康 |
| `custom-up` | 自定义组件 | `up` | 仅表示 exporter 抓取可达 |

约束：

- **不虚构指标名**。未在目标 Prometheus 中确认的指标不加入模板；JVM GC/Threads 等暂不提供面板。
- 所有模板都有 `requiredMetrics` / `requiredLabels`，`check` 会按标签统计实际覆盖数量；必需标签不完整时直接 FAIL。
- HTTP 查询以应用对象为最小单位，使用完整精确 Selector；站点总览只汇总 `entryApplications` 中启用 HTTP 的入口应用。
- 慢接口 Top 10 使用 `application + namespace + method + uri` 的 P95，并通过速率阈值（> 1/60 reqps）过滤低流量接口；OPTIONS 默认排除。
- 延迟统一以毫秒展示；PromQL 已乘以 1000，Grafana 单位固定为 `ms`。
- 组件查询使用 YAML 中的 Job、Instance 和额外标签精确筛选。
- 指标模板按应用启用范围生成：未启用 HTTP 的应用不会进入 HTTP 查询；未启用 JVM 的应用不会进入 JVM 查询。
- 站点级汇总不使用 `$app_selector`，应用级详细面板使用单选 `$app_selector`；第一版不允许应用对象再次被独立组合。

---

## 6. Prometheus 校验

### 6.1 工具配置

复制 [sitegen.example.yaml](sitegen.example.yaml) 为 `sitegen.yaml`，或使用 `--config`：

```yaml
prometheus:
  url: "http://prometheus:9090"
  timeout: 15s
  tlsCAFile: ""
  tlsInsecureSkipVerify: false

links:
  skywalking: "http://skywalking-ui"
  prometheus: "http://prometheus"
  grafana: "http://grafana"
  skywalkingServiceURLTemplate: "http://skywalking-ui/service?service={service}"
  skywalkingEndpointURLTemplate: "http://skywalking-ui/endpoint?service={service}"
  componentDashboards:
    mysql: "mysql-overview"
```

### 6.2 凭据（只从环境变量读取）

```text
SITEGEN_PROMETHEUS_URL
SITEGEN_PROMETHEUS_TIMEOUT
SITEGEN_PROMETHEUS_USERNAME
SITEGEN_PROMETHEUS_PASSWORD
SITEGEN_PROMETHEUS_TOKEN
SITEGEN_PROMETHEUS_TLS_CA
SITEGEN_PROMETHEUS_TLS_SKIP_VERIFY
SITEGEN_CONFIG
```

- Basic Auth 和 Bearer Token 互斥；
- `--prometheus-url` 优先于环境变量和配置文件；
- `links.prometheus` 只是 Dashboard 页面链接，不会作为校验目标；校验目标仅取自工具配置、环境变量或 `--prometheus-url`；
- `generate` 不需要 Prometheus 连通性，也不会因为工具配置中的 Prometheus 段不可用而失败；
- TLS 跳过校验默认关闭；
- 认证信息不会写入站点 YAML、报告或日志；
- 仅允许访问配置的 Prometheus 主机，跨主机重定向会被拒绝；
- 每个请求都有超时，校验并发度限制为 1-16。

### 6.3 校验结果

| 状态 | 含义 |
| --- | --- |
| PASS | 指标、必需标签、Selector、PromQL 正常且有数据 |
| WARN | 指标存在但当前窗口无序列、可选标签缺失、查询无数据 |
| FAIL | 指标不存在、必需标签缺失、Selector 无效、PromQL 错误、认证/连接失败 |
| SKIP | 用户关闭的指标模板，或未配置/显式跳过 Prometheus |

指标存在性通过 `__name__` 标签值检查（近 7 天），序列与标签通过用户可配置的 `--lookback` 窗口检查。无数据不等于配置错误，因此单独归为 WARN。

`build` 在 Prometheus 校验出现 FAIL 时**不会覆盖**已有 Dashboard；原文件保持不变。

---

## 7. Dashboard 设计

统一基础模板位于 `templates/dashboard/site-overview.json`，由强类型 Builder 填充行、面板、变量和链接。

### 行

1. **站点健康总览**（默认展开）：域名可用性/探测耗时、入口应用请求量、2xx/P95/5xx、应用指标采集状态、依赖组件状态。
2. **HTTP 请求分析**：当前所选应用的 RPS、2xx/3xx/4xx/5xx 比例、平均延迟、P95/P99、状态码分布、慢接口 Top 10、高请求量 Endpoint、按 Method 的 RPS。
3. **应用与 JVM**：当前所选应用的 JVM Heap（按 Pod / 内存池）。
4. **服务调用关系**：SkyWalking Service 映射与下钻、YAML 配置依赖；不伪造实测拓扑。
5. **依赖组件**：组件清单、专业 Dashboard 下钻；状态面板只出现在总览一次。
6. **Kubernetes 运行环境**：Pod 状态、Node 分布、Pod CPU/内存/Throttling。JVM Heap 不在此重复。

除总览外，其余 Row 默认折叠，首屏即可看到站点和依赖健康。

### 变量

- `$datasource`：Prometheus 数据源。
- `$app_selector`：**单选**应用实例，每个选项对应一个唯一的 Application/Namespace/Job/额外标签组合；应用级面板必须引用它。
- `$pod`：由当前 `$app_selector` 关联的 Pod 集合产生（JVM 的 `pod` 标签，以及 `kube_pod_labels` / `pod-regex` 对应的容器序列）；JVM 与 Kubernetes 面板共享同一 Pod 归属。仅有 HTTP 的站点不会生成无用的 `$pod` 变量。

不生成 `$component`、`$instance` 等无实际查询作用的变量。Dashboard 校验会拒绝未被任何面板或变量查询引用的变量。

应用级查询在生成时把每个应用对象的完整精确 Selector 编译进 PromQL，并通过派生标签 `sitegen_app` 过滤 `$app_selector`。变量值使用 Grafana 的 JSON 转义格式插值，URL 手工覆盖既不能注入 PromQL，也无法匹配站点之外的应用，只会得到空结果。默认时间范围 1 小时，刷新 30 秒。

### 稳定性和安全

- Dashboard UID：`site-<site-id>`，稳定且不超过 40 字符；
- 相同 YAML + 相同模板生成完全一致的 JSON；
- Panel ID / Row ID 全局唯一，网格布局无重叠，生成后重新解析校验；
- 未启用的指标模板不生成面板；
- 不写入 Grafana 数据库，不自动发布；
- 覆盖旧 Dashboard 前保留 `.bak`，失败时不替换；
- 指标模板版本（语义版本 + 内容摘要）与 Dashboard 基础模板摘要写入 tags；报告同时记录指标模板版本。

---

## 8. 校验报告

报告示例：

```text
[PASS] 模板 / 指标模板注册表: 版本 1.0.0，摘要 ...
[PASS] 配置 / Schema 与关联校验: 配置合法
[WARN] Prometheus / 应用 rtd-admin-test/http_server_requests_seconds_count: 指标存在，但 selector 在近 1h 内没有序列
[SKIP] 指标模板 / rtd-admin-test/kubernetes: 站点未启用该指标模板
```

报告不含任何认证信息。批量构建会打印每个站点的汇总和最终成功/失败数量。

---

## 9. 开发与测试

```bash
make test        # go test ./...
make vet         # go vet ./...
make build       # 单二进制
make dist        # linux/amd64, linux/arm64, windows/amd64
make generate-example
```

当前测试覆盖：

- YAML 严格解码、未知字段路径、ID/依赖/SkyWalking 校验、原子写入与备份；
- 模板注册表加载与占位符校验；
- Dashboard 确定性、变量、面板 ID、布局、JSON 校验；
- Prometheus 客户端认证、401、超时/端点校验；
- 校验器对缺失指标、无数据、标签缺失、PromQL 错误的分类；
- CLI 批量失败继续、build 失败保留旧 Dashboard、交互式创建流程。

---

## 10. 版本与限制

当前为 V1：

- 不提供 Web 后台、数据库、告警规则生成或自动发布；
- 不修改 Prometheus Operator / ServiceMonitor / ScrapeConfig；
- 不修改 SkyWalking 配置；
- SkyWalking 仅提供稳定的页面入口和 YAML 服务映射，不伪造数据源；
- JVM 只包含已确认的 Heap 指标，GC/Threads 待真实环境验证后加入；
- K8s 使用标准 cAdvisor / kube-state-metrics 指标，但仍应通过 `sitegen check` 在目标 Prometheus 上确认；
- Nacos 仅验证 exporter 抓取状态，Kafka 仅展示 Broker 数，后续再扩展性能指标；
- 应用由完整对象标识（application + namespace + job + 额外标签）；多应用聚合按对象逐一编译，不使用独立标签正则组合；
- 站点级外部请求汇总只统计域名 `entryApplications`，下游应用内部调用不会重复计入；
- Kubernetes 应用级面板要求显式 `kubernetes.podSelector`；无法建立 Pod 归属时跳过面板并输出 WARN，绝不退化为 Namespace 全量查询；
- Windows 下 cAdvisor/Java 标签结构差异较大，`containerMatchers` 必须根据目标 Prometheus 真实标签确认；
- Grafana 11.4 / 真实 Prometheus 的导入与查询验证必须在目标环境执行；没有真实 URL 时不能把 `generate` 或 `--skip-prometheus` 视为环境验收通过。

### V1.0.1 配置迁移提示

- 如果站点启用了 `http`，在域名下补充 `entryApplications`；否则会输出 WARN，并跳过站点级外部请求汇总面板。
- 如果应用启用了 `kubernetes`，补充 `kubernetes.podSelector`；否则该应用的 Kubernetes 面板会被跳过，避免 Namespace 全量查询。
- `containerMatchers` 必须按目标 Prometheus 的真实标签结构配置；未配置时保留 WARN 提示。
- Dashboard 变量已由 `$application` / `$namespace` 调整为单选 `$app_selector`，并删除 `$component` / `$instance`。重新生成后旧变量不再存在。
- 旧 YAML 不会自动改写；`sitegen validate` 只输出提示，`sitegen create/edit` 才会在确认后写入。

推荐上线流程：

```bash
sitegen validate --site sites/<site>.yaml
sitegen check --site sites/<site>.yaml
sitegen build --site sites/<site>.yaml
# 在 Grafana 导入 dashboards/<site-id>.json
# 导入后在顶部“数据源”变量中选择 Prometheus，再检查应用实例/Pod 联动与链接
```
