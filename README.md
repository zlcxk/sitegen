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
./sitegen validate --site sites/admin.yaml

# 环境验收：连接 Prometheus 校验指标、Selector、标签、Pod 归属和 PromQL
./sitegen check --site sites/admin.yaml --lookback 1h

# 普通构建：只做静态校验（YAML、配置语义、PromQL 语法、Dashboard 结构）
./sitegen build --site sites/admin.yaml

# 严格构建：实时检查失败时阻止构建（用于 CI）
./sitegen build --site sites/admin.yaml --strict-check

# 仅根据 YAML 生成 Dashboard（不访问 Prometheus）
./sitegen generate --site sites/admin.yaml

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
│   ├── metrics/        # 模板注册表、Selector、PromQL AST 改写、Prometheus 校验
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
        # 仅用于 up 指标的目标标签；默认只使用 job。
        # 不要把 application/namespace 放进 targetLabels。
        targetLabels:
          cluster: test-cluster

      skywalking:
        service: rtd-admin_test
        endpointFormat: "{method}:{uri}"
        # 可选：应用级 SkyWalking UI 下钻配置，优先于全局 links。
        ui:
          baseURL: "https://skywalking.example.com"
          serviceURLTemplate: "https://skywalking.example.com/service?name={service}"
          endpointURLTemplate: "https://skywalking.example.com/endpoint?name={service}"

      metrics: [http, jvm, kubernetes]

      kubernetes:
        # cAdvisor 没有 application 标签，必须显式声明 Pod 归属。
        # 标签名必须根据目标 Prometheus 的真实 kube_pod_labels 结构确认。
        podSelector:
          mode: labels
          metric: kube_pod_labels
          matchers:
            label_app: rtd-admin
        # 支持 equal/not_equal/regex/not_regex；旧的 map 写法仍然兼容。
        containerMatchers:
          - name: container
            operator: not_equal
            value: POD
          - name: image
            operator: not_equal
            value: ""

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

每个应用对象都以完整 Selector 参与查询：`application`、`namespace`、`job` 和该应用自己的全部额外标签。多应用聚合通过 PromQL AST 改写完成：先按应用对象逐一复制原始函数（如 `rate()`/`increase()`），保留原始 Range Vector Selector，再合并各应用的即时向量，最后由外层 `sum`/`sum by`/`topk`/`histogram_quantile` 统一聚合。**不会**把 `[5m]` 改写成 `[5m:]` Subquery，也不会把全局聚合变成分应用聚合。

`up` 指标只使用 `job` 和显式配置的 `targetLabels`。`application`、`namespace` 属于应用指标身份标签，不会被自动加入 `up` 查询，因此 Checker 与 Dashboard 的采集状态选择条件保持一致。

Kubernetes 指标不使用 Namespace 全量查询。应用必须通过 `kubernetes.podSelector` 声明 Pod 归属，Builder 会使用 `kube_pod_labels`（或经过验证的 `pod-regex` 兼容模式）与 cAdvisor 指标按 `namespace + pod` 关联。`containerMatchers` 支持 `equal`、`not_equal`、`regex`、`not_regex`；旧版 `{container: main}` 会被识别为等值匹配。未配置 Pod 归属的应用会跳过应用级 Kubernetes 面板并输出 WARN，且不会重复输出容器过滤警告。

JSON Schema 位于 [schemas/site.schema.json](schemas/site.schema.json)，可用于编辑器补全。加载器本身仍会执行严格字段检查和跨对象校验。

---

## 4. CLI 命令

| 命令 | 说明 |
| --- | --- |
| `sitegen init` | 初始化目录和工具配置 |
| `sitegen create` | 交互式创建站点 YAML；支持回退、重新编辑、跳过、草稿恢复 |
| `sitegen edit --site ...` | 按配置分区编辑已有站点，只修改选中的分区 |
| `sitegen validate --site ...` | 校验 YAML、ID、依赖、模板引用、UTF-8 |
| `sitegen check --site ...` | 连接 Prometheus 校验指标、标签、Selector、Pod 归属、PromQL |
| `sitegen generate --site ...` | 只生成 Dashboard（不访问 Prometheus） |
| `sitegen build --site ...` | 静态校验 + Dashboard 生成；实时检查交给 `check` |
| `sitegen build --site ... --strict-check` | 严格模式：实时检查失败时阻止构建 |
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

`build` 与 `check` 的职责严格分离：

| 检查类型 | 普通 `build` | `build --strict-check` | `check` |
| --- | --- | --- | --- |
| YAML 格式 / 必需字段 | 阻止 | 阻止 | 阻止 |
| PromQL 语法 / 非法 UTF-8 | 阻止 | 阻止 | 阻止 |
| Dashboard 结构 | 阻止 | 阻止 | 不生成 |
| 指标近 1h 无数据 | 不阻止 | 阻止 | 报告 WARN/FAIL |
| 组件暂时无数据 | 不阻止 | 阻止 | 报告 WARN |
| Pod 归属未配置 | 不阻止，跳过相关面板 | 不阻止，报告风险 | 报告 WARN/FAIL |
| Pod 归属 FAIL 导致下游无法判断 | 不阻止 | 阻止 | 下游记为 SKIP |

普通 `build` 成功后会明确提示：

```text
Dashboard 已生成。
环境数据检查尚未通过，部分面板可能暂时没有数据。
请执行 sitegen check 进行环境验收。
```

`check` / `build --strict-check` 额外支持：

```text
--prometheus-url URL
--timeout 15s
--lookback 1h
--concurrency 4       # 1-16
```

`--skip-prometheus` 保留为兼容参数；普通 `build` 默认已经不执行实时数据检查。

### 交互式创建流程

每个配置阶段都显示导航菜单：

```text
[Enter] 继续  [e] 重新编辑当前步骤  [b] 返回上一步  [s] 跳过  [q] 退出并保存草稿
```

任何输入处也可以用 `:b` / `:e` / `:s` / `:q` 执行相同操作。返回上一步不会清除后续已经填写的内容；输入不合法时留在当前问题重试。

基础创建只收集生成 Dashboard 所必需的信息：

1. 站点 ID、名称、环境；
2. 域名 URL、Blackbox Job/Instance（提供默认值）；
3. 应用 Application、Namespace、Prometheus Job（默认等于 Application）、指标模板；
4. 启用 `kubernetes` 的应用选择 Pod 归属方式：
   - Pod 标签匹配：默认 `kube_pod_labels`，连接 Prometheus 时先检查指标是否存在；
   - Pod 名称正则：连接 Prometheus 时试查询 cAdvisor，展示匹配 Pod 数量和样例；
   - 暂不配置：跳过应用级 Kubernetes 面板；
5. 入口应用：单个 HTTP 应用时默认选中并确认，多应用时多选，每个域名独立选择。

以下高级项按阶段询问，默认不启用：Target Labels、Kubernetes 容器过滤、SkyWalking UI 下钻、依赖组件、应用依赖关系。Kubernetes 容器过滤会先展示实际发现的 `container/image/name/id` 标签，并明确提示 `job/instance/namespace/pod` 不能作为容器过滤条件。

### 草稿与恢复

每完成一个阶段自动写入：

```text
sites/
  admin.yaml
  .drafts/
    admin.yaml.draft
```

草稿记录下一个待执行阶段。中断后再次执行 `sitegen create` 会询问恢复、重新开始或删除草稿。草稿不参与 Dashboard 生成，也不会覆盖正式 YAML；只有最终预览确认后才原子替换正式文件。

### 分区编辑

`sitegen edit --site sites/admin.yaml` 先显示分区菜单：

```text
1. 站点基础信息
2. 域名和入口应用
3. 应用和指标模板
4. Kubernetes Pod 归属
5. Kubernetes 容器过滤
6. Prometheus Target Labels
7. SkyWalking 下钻
8. 依赖组件
9. 应用依赖关系
10. 查看完整配置
```

只进入选中的分区，不重新输入其他分区。编辑完成后展示变更摘要，确认后原子写入并保留 `.bak` 备份；未确认时不修改正式 YAML。

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
- `platform` 的 `up` 查询只使用 `job` 与 `prometheus.targetLabels`，不会自动加入 application/namespace。
- 慢接口 Top 10 使用 `application + namespace + method + uri` 的 P95，并通过速率阈值（> 1/60 reqps）过滤低流量接口；OPTIONS 默认排除。
- 延迟统一以毫秒展示；PromQL 已乘以 1000，Grafana 单位固定为 `ms`。
- 组件查询使用 YAML 中的 Job、Instance 和额外标签精确筛选。
- 指标模板按应用启用范围生成：未启用 HTTP 的应用不会进入 HTTP 查询；未启用 JVM 的应用不会进入 JVM 查询。
- 站点级汇总只使用配置中的入口应用，不依赖应用选择变量；HTTP/JVM/Kubernetes 详细面板分别使用 `$http_app_selector`、`$jvm_app_selector`、`$k8s_app_selector`，不允许选择未启用对应 Profile 的应用。

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
| WARN | 指标存在但当前窗口无序列、可选标签缺失、查询无数据、Pod 归属不完整 |
| FAIL | 指标不存在、必需标签缺失、Selector 无效、PromQL 错误、Pod 归属失败、认证/连接失败 |
| SKIP | 用户关闭的指标模板；上游根因失败导致依赖检查无法判断；显式跳过 Prometheus |

指标存在性通过 `__name__` 标签值检查（近 7 天），序列与标签通过用户可配置的 `--lookback` 窗口检查。无数据不等于配置错误，因此单独归为 WARN。

报告会先输出根因摘要，再列出详细检查：

```text
检查摘要
--------
需要处理的主要问题：
1. Selector 未匹配到数据：应用 rtd-admin-dev
2. Kubernetes Pod 归属未建立：Pod 归属/rtd-admin

受影响的下游检查：
- 面板查询（PromQL）：12 项
- 指标与资源检查：10 项
```

指标存在但 Selector 无匹配时，报告会输出实际执行的完整 Selector，并用 Prometheus 实际返回的标签值区分“标签不存在”“标签值不存在”“最近 1h 未采集”：

```text
指标存在（近 7 天），但最近 1h 内未匹配到序列。

实际 Selector:
application="rtd-admin", namespace="dev", job="rtd-admin", cluster="test-cluster"

诊断结果:
1. application="rtd-admin"：存在
2. namespace="dev"：存在
3. job="rtd-admin"：存在
4. cluster="test-cluster"：不存在；实际存在值: "test", "prod"
```

Pod 归属失败后，下游 Kubernetes 覆盖检查和容器过滤检查记为 SKIP，不会重复输出相同 FAIL。HTTP/JVM 基础指标全部无匹配时，下游面板查询同样记为 SKIP；但基础序列匹配成功、单个查询无数据时仍独立报告 WARN。

`check` 在出现 FAIL 时返回非零退出码。`build` 默认不受实时数据影响；`build --strict-check` 在实时检查失败时不会覆盖已有 Dashboard。

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

各指标区域使用独立的应用选择变量，只包含启用了对应 Profile 的应用：

- `$datasource`：Prometheus 数据源。
- `$http_app_selector`：仅包含启用 `http` 的应用。
- `$jvm_app_selector`：仅包含启用 `jvm` 的应用。
- `$k8s_app_selector`：仅包含启用 `kubernetes` 且配置了 `podSelector` 的应用。
- `$jvm_pod` / `$k8s_pod`：分别由对应应用选择器关联的 Pod 集合产生。

每个选项文本为 `Application / Namespace`，值为唯一应用配置 ID；同名应用跨 Namespace 时可以独立选择。没有对应 Profile 的应用不会出现在该区域的选择列表中，因此不会因为变量范围不一致出现误导性的 No Data。

不生成 `$component`、`$instance` 等无实际查询作用的变量。Dashboard 校验会拒绝未被任何面板或变量查询引用的变量。

应用级查询在生成时把每个应用对象的完整精确 Selector 编译进 PromQL，并通过派生标签 `sitegen_app` 过滤对应的 Profile 选择变量。变量值使用 Grafana 的 JSON 转义格式插值，URL 手工覆盖既不能注入 PromQL，也无法匹配站点之外的应用，只会得到空结果。默认时间范围 1 小时，刷新 30 秒。

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
[WARN] Prometheus / 应用 rtd-admin-test/http_server_requests_seconds_count: 指标存在（近 7 天），但最近 1h 内未匹配到序列。
       实际 Selector:
       application="rtd-admin", namespace="test", job="rtd-admin"
       诊断结果:
       1. application="rtd-admin"：存在
       2. namespace="test"：存在
       3. job="rtd-admin"：存在
[SKIP] PromQL / 应用/rtd-admin-test/http/http-p95: 依赖的基础指标在近 1h 内没有匹配序列；根因已在上方指标检查中报告
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
- PromQL AST 改写：Range Selector 保持、无 Subquery、Histogram/TopK 聚合语义、多应用联合；
- 使用 Prometheus 官方 PromQL 引擎在可重复数据集上执行生成查询，校验 rate/increase、P95/P99、平均延迟与 Counter Reset 结果；
- `up` Selector 只使用 job/targetLabels，Checker 与 Builder 一致；
- 容器 Matcher operator、旧 map 兼容、Kubernetes 指标覆盖与容器过滤校验；
- Dashboard 确定性、按 Profile 的应用变量、面板 ID、布局、JSON 校验；
- Prometheus 客户端认证、401、超时/端点校验；
- 校验器对缺失指标、无数据、标签缺失、Pod 归属失败、PromQL 错误的分类；
- CLI 批量失败继续、`build` 与 `check` 分离、严格构建保留旧 Dashboard；
- 向导回退/重新编辑/跳过/退出草稿、草稿恢复、分区编辑只修改选中分区、输入重试、终端 `:b/:e/:s/:q` 命令；
- Selector 完整输出、真实标签值诊断、非 UTF-8 字段定位、PromQL 特殊字符转义；
- Pod 归属成功/失败、下游 SKIP 传播、Pod 正则、应用范围内容器过滤、kube-state-metrics 指标不被容器 Matcher 误过滤；
- 报告根因摘要与下游依赖计数。

---

## 10. 版本与限制

当前为 V1.0.3：

- 不提供 Web 后台、数据库、告警规则生成或自动发布；
- 不修改 Prometheus Operator / ServiceMonitor / ScrapeConfig；
- 不修改 SkyWalking 配置；
- SkyWalking 仅提供稳定的页面入口和 YAML 服务映射，不伪造数据源；
- JVM 只包含已确认的 Heap 指标，GC/Threads 待真实环境验证后加入；
- K8s 使用标准 cAdvisor / kube-state-metrics 指标，但仍应通过 `sitegen check` 在目标 Prometheus 上确认；
- Nacos 仅验证 exporter 抓取状态，Kafka 仅展示 Broker 数，后续再扩展性能指标；
- 应用由完整对象标识（application + namespace + job + 额外标签）；多应用聚合通过 PromQL AST 按对象逐一编译，不使用独立标签正则组合，也不会把 Range Selector 改为 Subquery；
- `up` 只使用 `job` + `prometheus.targetLabels`，不自动加入 application/namespace；
- 站点级外部请求汇总只统计域名 `entryApplications`，下游应用内部调用不会重复计入；
- Kubernetes 应用级面板要求显式 `kubernetes.podSelector`；无法建立 Pod 归属时跳过面板并输出 WARN，绝不退化为 Namespace 全量查询；
- 不同环境 cAdvisor/Java 标签结构差异较大，`containerMatchers` 必须根据目标 Prometheus 真实标签确认；
- Grafana 11.4 / 真实 Prometheus 的导入与查询验证必须在目标环境执行；没有真实 URL 时不能把 `generate` 或 `--skip-prometheus` 视为环境验收通过。

### V1.0.3 迁移提示

- `build` 默认不再执行实时 Prometheus 检查；需要环境验收请执行 `sitegen check`，CI 需要严格阻断时使用 `sitegen build --strict-check`。
- `containerMatchers` 现在明确禁止 `job`、`instance`、`namespace`、`pod`。应用 Prometheus Job 不等于 cAdvisor Job；容器过滤使用 `container`、`image`、`name`、`id`。
- `not_equal` 允许空值，例如 `image != ""` 用于排除缺失 image 标签的 Pod 父级 cgroup 序列；`equal`/`regex`/`not_regex` 仍要求非空值。
- 容器过滤只应用于 `container_*` cAdvisor 指标，不会过滤 `kube_pod_info`、`kube_pod_status_phase` 等 kube-state-metrics 指标。
- Pod 归属失败后，Kubernetes 覆盖检查和容器过滤检查记为 `SKIP`，不再重复 FAIL。
- 所有进入 PromQL 的用户字符串都会先做 UTF-8 校验；非法字节会在配置校验阶段定位到具体字段。
- `sitegen edit` 改为分区编辑，不再逐项询问所有配置；创建流程支持草稿恢复。

### V1.0.2 配置迁移提示

- Dashboard 变量拆分为 `$http_app_selector`、`$jvm_app_selector`、`$k8s_app_selector`，以及 `$jvm_pod` / `$k8s_pod`。重新生成后旧的全局 `$app_selector` / `$pod` 不再存在。
- `up` 指标默认只使用 `job`。如果采集目标需要额外区分，请配置 `prometheus.targetLabels`；不要把 `application`、`namespace` 放入 `targetLabels`。
- `containerMatchers` 支持 operator 列表写法；旧的 map 写法继续兼容，并按 `equal` 处理。
- 应用级 SkyWalking UI 配置使用 `skywalking.ui.baseURL` / `serviceURLTemplate` / `endpointURLTemplate`，并优先于全局 `links`。
- 仅当 `kubernetes.podSelector` 存在时才提示容器过滤风险；未配置 Pod 归属时只提示“应用级 Kubernetes 面板不生成”，不会产生重复警告。
- 旧 YAML 不会自动改写；`sitegen validate` 只输出提示，`sitegen create/edit` 才会在确认后写入。

推荐上线流程：

```bash
sitegen validate --site sites/<site>.yaml
sitegen check --site sites/<site>.yaml
sitegen build --site sites/<site>.yaml
# 在 Grafana 导入 dashboards/<site-id>.json
# 导入后在顶部“数据源”变量中选择 Prometheus，再检查应用实例/Pod 联动与链接
```
