package metrics

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"site-observability/internal/config"
)

const (
	selectorPlaceholderName  = "__sitegen_selector__"
	selectorPlaceholderValue = "__sitegen_selector_placeholder__"
	podJSONSentinel          = "__SITEGEN_POD_JSON__"
	appSelectorJSONSentinel  = "__SITEGEN_APP_SELECTOR_JSON__"
)

var promQLParser = parser.NewParser(parser.Options{})

var grafanaJSONVarRE = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:[a-z]+\}`)
var grafanaVarRE = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)
var grafanaBareVarRE = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)

type queryScope int

const (
	scopeApplication queryScope = iota
	scopeNamespace
	scopeJob
)

type renderOptions struct {
	scope       queryScope
	filterByApp bool
	selectorVar string
	podVar      string
}

func podVarForSelector(selectorVar string) string {
	if selectorVar == "" {
		return "pod"
	}
	base := strings.TrimSuffix(selectorVar, "_app_selector")
	return base + "_pod"
}

// RenderApplicationQuery compiles an application-scope template for a set of
// application objects using the PromQL AST. Range Vector Selectors are never
// converted into subqueries: functions such as rate()/increase() are cloned per
// application and the resulting instant vectors are unioned, so the outer
// aggregation still runs globally.
func RenderApplicationQuery(query string, apps []config.Application, filterByApp bool, selectorVar string) (string, error) {
	if len(apps) == 0 {
		return query, nil
	}
	if len(apps) == 1 && !filterByApp {
		out := RenderQuery(query, ApplicationTargetSelector(apps[0]))
		if err := ValidateQuery(out); err != nil {
			return "", err
		}
		return out, nil
	}
	return renderWithApps(query, apps, renderOptions{
		scope:       scopeApplication,
		filterByApp: filterByApp,
		selectorVar: selectorVar,
		podVar:      podVarForSelector(selectorVar),
	})
}

// RenderNamespaceQuery compiles a Kubernetes query. Pod ownership is applied
// after the range-vector function, never by rewriting the range selector.
func RenderNamespaceQuery(query string, apps []config.Application, filterByApp bool, selectorVar string) (string, error) {
	if len(apps) == 0 {
		return query, nil
	}
	return renderWithApps(query, apps, renderOptions{
		scope:       scopeNamespace,
		filterByApp: filterByApp,
		selectorVar: selectorVar,
		podVar:      podVarForSelector(selectorVar),
	})
}

// RenderJobQuery compiles a target-scope (up) query. Only job and explicit
// targetLabels are used; application identity labels are never implied.
func RenderJobQuery(query string, apps []config.Application) (string, error) {
	var withJob []config.Application
	for _, app := range apps {
		if app.Prometheus.Job != "" {
			withJob = append(withJob, app)
		}
	}
	if len(withJob) == 0 {
		return query, nil
	}
	if len(withJob) == 1 {
		out := RenderQuery(query, TargetSelector(withJob[0]))
		if err := ValidateQuery(out); err != nil {
			return "", err
		}
		return out, nil
	}
	return renderWithApps(query, withJob, renderOptions{scope: scopeJob})
}

func renderWithApps(query string, apps []config.Application, opts renderOptions) (string, error) {
	prepared := prepareQuery(query)
	expr, err := promQLParser.ParseExpr(prepared)
	if err != nil {
		return "", fmt.Errorf("解析 PromQL 模板失败: %w", err)
	}
	rewritten, err := rewriteNode(expr, apps, opts)
	if err != nil {
		return "", err
	}
	if containsSelectorPlaceholder(rewritten) {
		return "", fmt.Errorf("生成的 PromQL 仍包含未展开的选择器占位符")
	}
	out := parser.Prettify(rewritten)
	out = restoreGrafanaTokens(out, opts.selectorVar, opts.podVar)
	if err := ValidateQuery(out); err != nil {
		return "", fmt.Errorf("生成的 PromQL 语法校验失败: %w", err)
	}
	return out, nil
}

// ValidateQuery verifies that a generated query is valid PromQL after
// substituting Grafana variable expressions with safe placeholders.
func ValidateQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("查询为空")
	}
	if strings.Contains(query, selectorToken) {
		return fmt.Errorf("查询仍包含未替换的 %s 占位符", selectorToken)
	}
	prepared := sanitizeGrafanaVars(query)
	if _, err := promQLParser.ParseExpr(prepared); err != nil {
		return fmt.Errorf("PromQL 解析失败: %w", err)
	}
	return nil
}

func prepareQuery(query string) string {
	q := replaceGrafanaVars(query, func(token string) string {
		if isPodVariable(token) {
			return QuoteLabelValue(podJSONSentinel)
		}
		return QuoteLabelValue(appSelectorJSONSentinel)
	})
	q = strings.ReplaceAll(q, selectorToken, selectorPlaceholderName+"="+QuoteLabelValue(selectorPlaceholderValue))
	return q
}

// replaceGrafanaVars replaces ${...} and $name tokens outside PromQL string
// literals. This prevents dollar signs inside label values from being
// mistaken for Grafana variables.
func replaceGrafanaVars(query string, fn func(string) string) string {
	var out strings.Builder
	for i := 0; i < len(query); {
		switch query[i] {
		case '"':
			start := i
			i++
			for i < len(query) {
				if query[i] == '\\' {
					i += 2
					continue
				}
				if query[i] == '"' {
					i++
					break
				}
				i++
			}
			out.WriteString(query[start:i])
		case '$':
			start := i
			if i+1 < len(query) && query[i+1] == '{' {
				if end := strings.IndexByte(query[i+2:], '}'); end >= 0 {
					i += 2 + end + 1
					out.WriteString(fn(query[start:i]))
					continue
				}
			} else if i+1 < len(query) && (isNameStart(query[i+1])) {
				i++
				for i < len(query) && isNameChar(query[i]) {
					i++
				}
				out.WriteString(fn(query[start:i]))
				continue
			}
			out.WriteByte(query[i])
			i++
		default:
			out.WriteByte(query[i])
			i++
		}
	}
	return out.String()
}

func isNameStart(ch byte) bool {
	return ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isNameChar(ch byte) bool {
	return isNameStart(ch) || (ch >= '0' && ch <= '9')
}

func isPodVariable(token string) bool {
	name := token
	name = strings.TrimPrefix(name, "$")
	name = strings.TrimPrefix(name, "{")
	name = strings.TrimSuffix(name, "}")
	if index := strings.IndexByte(name, ':'); index >= 0 {
		name = name[:index]
	}
	return name == "pod" || strings.HasSuffix(name, "_pod")
}

func restoreGrafanaTokens(query, selectorVar, podVar string) string {
	if podVar == "" {
		podVar = "pod"
	}
	if selectorVar == "" {
		selectorVar = "app_selector"
	}
	out := strings.ReplaceAll(query, QuoteLabelValue(podJSONSentinel), "${"+podVar+":json}")
	out = strings.ReplaceAll(out, QuoteLabelValue(appSelectorJSONSentinel), "${"+selectorVar+":json}")
	return out
}

func sanitizeGrafanaVars(query string) string {
	return replaceGrafanaVars(query, func(string) string {
		return QuoteLabelValue("__sitegen_var__")
	})
}

func rewriteNode(node parser.Expr, apps []config.Application, opts renderOptions) (parser.Expr, error) {
	switch n := node.(type) {
	case *parser.Call:
		if callHasMatrixPlaceholder(n) {
			return rewriteCall(n, apps, opts)
		}
		for i := range n.Args {
			rewritten, err := rewriteNode(n.Args[i], apps, opts)
			if err != nil {
				return nil, err
			}
			n.Args[i] = rewritten
		}
		return n, nil
	case *parser.AggregateExpr:
		rewritten, err := rewriteNode(n.Expr, apps, opts)
		if err != nil {
			return nil, err
		}
		n.Expr = rewritten
		if n.Param != nil {
			rewrittenParam, err := rewriteNode(n.Param, apps, opts)
			if err != nil {
				return nil, err
			}
			n.Param = rewrittenParam
		}
		return n, nil
	case *parser.BinaryExpr:
		lhs, err := rewriteNode(n.LHS, apps, opts)
		if err != nil {
			return nil, err
		}
		rhs, err := rewriteNode(n.RHS, apps, opts)
		if err != nil {
			return nil, err
		}
		n.LHS, n.RHS = lhs, rhs
		return n, nil
	case *parser.ParenExpr:
		rewritten, err := rewriteNode(n.Expr, apps, opts)
		if err != nil {
			return nil, err
		}
		n.Expr = rewritten
		return n, nil
	case *parser.UnaryExpr:
		rewritten, err := rewriteNode(n.Expr, apps, opts)
		if err != nil {
			return nil, err
		}
		n.Expr = rewritten
		return n, nil
	case *parser.SubqueryExpr:
		rewritten, err := rewriteNode(n.Expr, apps, opts)
		if err != nil {
			return nil, err
		}
		n.Expr = rewritten
		return n, nil
	case *parser.VectorSelector:
		if isPlaceholderSelector(n) {
			return rewriteInstantSelector(n, apps, opts)
		}
		return n, nil
	case *parser.MatrixSelector:
		if isPlaceholderExpr(n.VectorSelector) {
			return nil, fmt.Errorf("不支持的 PromQL 结构：Range Vector Selector 未被函数包裹")
		}
		return n, nil
	default:
		return node, nil
	}
}

func rewriteCall(call *parser.Call, apps []config.Application, opts renderOptions) (parser.Expr, error) {
	parts := make([]parser.Expr, 0, len(apps))
	for _, app := range apps {
		cloned, err := cloneCallForApp(call, app, opts.scope)
		if err != nil {
			return nil, err
		}
		wrapped, err := wrapPerApp(cloned, app, opts)
		if err != nil {
			return nil, err
		}
		parts = append(parts, wrapped)
	}
	union := orExprs(parts)
	if opts.filterByApp {
		union = addAppFilter(union)
	}
	return &parser.ParenExpr{Expr: union}, nil
}

func rewriteInstantSelector(selector *parser.VectorSelector, apps []config.Application, opts renderOptions) (parser.Expr, error) {
	parts := make([]parser.Expr, 0, len(apps))
	for _, app := range apps {
		cloned, err := buildVectorSelector(selector, app, opts.scope)
		if err != nil {
			return nil, err
		}
		wrapped, err := wrapPerApp(cloned, app, opts)
		if err != nil {
			return nil, err
		}
		parts = append(parts, wrapped)
	}
	union := orExprs(parts)
	if opts.filterByApp {
		union = addAppFilter(union)
	}
	return &parser.ParenExpr{Expr: union}, nil
}

func cloneCallForApp(call *parser.Call, app config.Application, scope queryScope) (*parser.Call, error) {
	args := make(parser.Expressions, len(call.Args))
	for i, arg := range call.Args {
		if matrix, ok := arg.(*parser.MatrixSelector); ok && isPlaceholderExpr(matrix.VectorSelector) {
			vector, ok := matrix.VectorSelector.(*parser.VectorSelector)
			if !ok {
				return nil, fmt.Errorf("不支持的 PromQL 结构：Range Selector 不是 Vector Selector")
			}
			clonedVector, err := buildVectorSelector(vector, app, scope)
			if err != nil {
				return nil, err
			}
			clonedMatrix := *matrix
			clonedMatrix.VectorSelector = clonedVector
			args[i] = &clonedMatrix
			continue
		}
		if containsSelectorPlaceholder(arg) {
			return nil, fmt.Errorf("不支持的 PromQL 结构：同一函数中混合 Range Vector 与即时 Vector 占位符")
		}
		args[i] = arg
	}
	return &parser.Call{Func: call.Func, Args: args, PosRange: call.PosRange}, nil
}

func buildVectorSelector(original *parser.VectorSelector, app config.Application, scope queryScope) (*parser.VectorSelector, error) {
	matchers, err := appMatchers(original.LabelMatchers, app, scope)
	if err != nil {
		return nil, err
	}
	cloned := *original
	cloned.LabelMatchers = matchers
	cloned.UnexpandedSeriesSet = nil
	cloned.Series = nil
	return &cloned, nil
}

func appMatchers(original []*labels.Matcher, app config.Application, scope queryScope) ([]*labels.Matcher, error) {
	out := make([]*labels.Matcher, 0, len(original)+8)
	for _, matcher := range original {
		if matcher.Name == selectorPlaceholderName && matcher.Value == selectorPlaceholderValue {
			continue
		}
		out = append(out, matcher)
	}
	addEqual := func(name, value string) error {
		matcher, err := labels.NewMatcher(labels.MatchEqual, name, value)
		if err != nil {
			return err
		}
		out = append(out, matcher)
		return nil
	}
	switch scope {
	case scopeApplication:
		if err := addEqual("application", app.Application); err != nil {
			return nil, err
		}
		if err := addEqual("namespace", app.Namespace); err != nil {
			return nil, err
		}
		if app.Prometheus.Job != "" {
			if err := addEqual("job", app.Prometheus.Job); err != nil {
				return nil, err
			}
		}
		for _, key := range sortedKeys(app.Prometheus.Labels) {
			if reservedAppLabel(key) {
				continue
			}
			if err := addEqual(key, app.Prometheus.Labels[key]); err != nil {
				return nil, err
			}
		}
	case scopeNamespace:
		if app.Kubernetes.PodSelector == nil {
			return nil, fmt.Errorf("应用 %s 未配置 kubernetes.podSelector", app.ID)
		}
		switch app.Kubernetes.PodSelector.Mode {
		case config.PodSelectorLabels:
		case config.PodSelectorPodRegex:
		default:
			return nil, fmt.Errorf("应用 %s 的 podSelector.mode 非法: %q", app.ID, app.Kubernetes.PodSelector.Mode)
		}
		if err := addEqual("namespace", app.Namespace); err != nil {
			return nil, err
		}
		podMatcher, err := labels.NewMatcher(labels.MatchRegexp, "pod", podJSONSentinel)
		if err != nil {
			return nil, err
		}
		out = append(out, podMatcher)
		if app.Kubernetes.PodSelector.Mode == config.PodSelectorPodRegex && app.Kubernetes.PodSelector.Pod != "" {
			regexMatcher, err := labels.NewMatcher(labels.MatchRegexp, "pod", app.Kubernetes.PodSelector.Pod)
			if err != nil {
				return nil, err
			}
			out = append(out, regexMatcher)
		}
		for _, matcher := range app.Kubernetes.ContainerMatchers {
			created, err := containerMatcher(matcher)
			if err != nil {
				return nil, err
			}
			out = append(out, created)
		}
	case scopeJob:
		if app.Prometheus.Job != "" {
			if err := addEqual("job", app.Prometheus.Job); err != nil {
				return nil, err
			}
		}
		for _, key := range sortedKeys(app.Prometheus.TargetLabels) {
			if err := addEqual(key, app.Prometheus.TargetLabels[key]); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("未知的查询范围")
	}
	return out, nil
}

func containerMatcher(matcher config.LabelMatcher) (*labels.Matcher, error) {
	var matchType labels.MatchType
	switch matcher.EffectiveOperator() {
	case config.MatchEqual:
		matchType = labels.MatchEqual
	case config.MatchNotEqual:
		matchType = labels.MatchNotEqual
	case config.MatchRegex:
		matchType = labels.MatchRegexp
	case config.MatchNotRegex:
		matchType = labels.MatchNotRegexp
	default:
		return nil, fmt.Errorf("不支持的容器过滤操作符 %q", matcher.Operator)
	}
	return labels.NewMatcher(matchType, matcher.Name, matcher.Value)
}

func wrapPerApp(expr parser.Expr, app config.Application, opts renderOptions) (parser.Expr, error) {
	switch opts.scope {
	case scopeApplication:
		if opts.filterByApp {
			return labelReplaceCall(expr, app.ID), nil
		}
		return expr, nil
	case scopeNamespace:
		if app.Kubernetes.PodSelector == nil {
			return nil, fmt.Errorf("应用 %s 未配置 kubernetes.podSelector", app.ID)
		}
		switch app.Kubernetes.PodSelector.Mode {
		case config.PodSelectorLabels:
			owner, err := buildPodOwnership(app)
			if err != nil {
				return nil, err
			}
			join := &parser.BinaryExpr{
				Op:  parser.MUL,
				LHS: expr,
				RHS: owner,
				VectorMatching: &parser.VectorMatching{
					Card:           parser.CardManyToOne,
					On:             true,
					MatchingLabels: []string{"namespace", "pod"},
					Include:        []string{config.SitegenAppLabel},
				},
			}
			return &parser.ParenExpr{Expr: join}, nil
		case config.PodSelectorPodRegex:
			if opts.filterByApp {
				return labelReplaceCall(expr, app.ID), nil
			}
			return expr, nil
		default:
			return nil, fmt.Errorf("应用 %s 的 podSelector.mode 非法: %q", app.ID, app.Kubernetes.PodSelector.Mode)
		}
	case scopeJob:
		return expr, nil
	default:
		return nil, fmt.Errorf("未知的查询范围")
	}
}

func buildPodOwnership(app config.Application) (parser.Expr, error) {
	selector := app.Kubernetes.PodSelector
	namespace, err := labels.NewMatcher(labels.MatchEqual, "namespace", app.Namespace)
	if err != nil {
		return nil, err
	}
	matchers := []*labels.Matcher{namespace}
	for _, key := range sortedKeys(selector.Matchers) {
		matcher, err := labels.NewMatcher(labels.MatchEqual, key, selector.Matchers[key])
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, matcher)
	}
	owner := labelReplaceCall(&parser.VectorSelector{
		Name:          selector.PodLabelMetric(),
		LabelMatchers: matchers,
	}, app.ID)
	return &parser.AggregateExpr{
		Op:       parser.MAX,
		Expr:     owner,
		Grouping: []string{"namespace", "pod", config.SitegenAppLabel},
	}, nil
}

func labelReplaceCall(expr parser.Expr, value string) *parser.Call {
	return &parser.Call{
		Func: parser.Functions["label_replace"],
		Args: parser.Expressions{
			expr,
			&parser.StringLiteral{Val: config.SitegenAppLabel},
			&parser.StringLiteral{Val: value},
			&parser.StringLiteral{Val: ""},
			&parser.StringLiteral{Val: ""},
		},
	}
}

func addAppFilter(expr parser.Expr) parser.Expr {
	vectorOne := &parser.Call{
		Func: parser.Functions["vector"],
		Args: parser.Expressions{&parser.NumberLiteral{Val: 1}},
	}
	filter := labelReplaceCall(vectorOne, appSelectorJSONSentinel)
	join := &parser.BinaryExpr{
		Op:  parser.MUL,
		LHS: &parser.ParenExpr{Expr: expr},
		RHS: filter,
		VectorMatching: &parser.VectorMatching{
			Card:           parser.CardManyToOne,
			On:             true,
			MatchingLabels: []string{config.SitegenAppLabel},
		},
	}
	return &parser.ParenExpr{Expr: join}
}

func orExprs(parts []parser.Expr) parser.Expr {
	if len(parts) == 0 {
		return nil
	}
	expr := parts[0]
	for _, part := range parts[1:] {
		expr = &parser.BinaryExpr{Op: parser.LOR, LHS: expr, RHS: part}
	}
	if len(parts) > 1 {
		return &parser.ParenExpr{Expr: expr}
	}
	return expr
}

func callHasMatrixPlaceholder(call *parser.Call) bool {
	for _, arg := range call.Args {
		if matrix, ok := arg.(*parser.MatrixSelector); ok && isPlaceholderExpr(matrix.VectorSelector) {
			return true
		}
	}
	return false
}

func isPlaceholderExpr(expr parser.Expr) bool {
	switch value := expr.(type) {
	case *parser.VectorSelector:
		return isPlaceholderSelector(value)
	case *parser.MatrixSelector:
		return isPlaceholderExpr(value.VectorSelector)
	default:
		return false
	}
}

func isPlaceholderSelector(selector *parser.VectorSelector) bool {
	for _, matcher := range selector.LabelMatchers {
		if matcher.Name == selectorPlaceholderName && matcher.Value == selectorPlaceholderValue {
			return true
		}
	}
	return false
}

func containsSelectorPlaceholder(node parser.Node) bool {
	found := false
	parser.Inspect(node, func(current parser.Node, _ []parser.Node) error {
		if selector, ok := current.(*parser.VectorSelector); ok && isPlaceholderSelector(selector) {
			found = true
		}
		return nil
	})
	return found
}

// BuildPodVariableExpression returns a PromQL expression whose pod values
// belong to the application selected by the profile-scoped selector variable.
// The expression is used inside Grafana's query_result() variable function.
func BuildPodVariableExpression(site *config.Site, profileID, selectorVar string) (string, error) {
	if site == nil {
		return "", nil
	}
	var parts []parser.Expr
	for _, app := range site.Spec.Applications {
		if !app.HasMetric(profileID) {
			continue
		}
		var expr parser.Expr
		switch profileID {
		case "jvm":
			selector, err := buildVectorSelector(&parser.VectorSelector{
				Name:          "jvm_memory_used_bytes",
				LabelMatchers: nil,
			}, app, scopeApplication)
			if err != nil {
				return "", err
			}
			expr = labelReplaceCall(selector, app.ID)
		case "kubernetes":
			if app.Kubernetes.PodSelector == nil {
				continue
			}
			selector := app.Kubernetes.PodSelector
			var err error
			switch selector.Mode {
			case config.PodSelectorLabels:
				expr, err = podLabelSelector(selector.PodLabelMetric(), app)
			case config.PodSelectorPodRegex:
				if selector.Pod == "" {
					continue
				}
				expr, err = podRegexSelector("container_memory_working_set_bytes", app, selector.Pod)
			default:
				continue
			}
			if err != nil {
				return "", err
			}
			expr = labelReplaceCall(expr, app.ID)
		default:
			continue
		}
		parts = append(parts, expr)
	}
	if len(parts) == 0 {
		return "", nil
	}
	union := orExprs(parts)
	union = addAppFilter(union)
	out := parser.Prettify(union)
	out = restoreGrafanaTokens(out, selectorVar, "")
	if err := ValidateQuery(out); err != nil {
		return "", fmt.Errorf("Pod 变量查询校验失败: %w", err)
	}
	return out, nil
}

func podLabelSelector(metric string, app config.Application) (parser.Expr, error) {
	namespace, err := labels.NewMatcher(labels.MatchEqual, "namespace", app.Namespace)
	if err != nil {
		return nil, err
	}
	matchers := []*labels.Matcher{namespace}
	for _, key := range sortedKeys(app.Kubernetes.PodSelector.Matchers) {
		matcher, err := labels.NewMatcher(labels.MatchEqual, key, app.Kubernetes.PodSelector.Matchers[key])
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, matcher)
	}
	return &parser.VectorSelector{Name: metric, LabelMatchers: matchers}, nil
}

func podRegexSelector(metric string, app config.Application, podRegex string) (parser.Expr, error) {
	namespace, err := labels.NewMatcher(labels.MatchEqual, "namespace", app.Namespace)
	if err != nil {
		return nil, err
	}
	pod, err := labels.NewMatcher(labels.MatchRegexp, "pod", podRegex)
	if err != nil {
		return nil, err
	}
	return &parser.VectorSelector{Name: metric, LabelMatchers: []*labels.Matcher{namespace, pod}}, nil
}
