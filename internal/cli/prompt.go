package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrCancelled is returned when the user explicitly aborts or input ends.
var ErrCancelled = errors.New("用户取消")

// ErrDraftSaved indicates that the wizard stopped and persisted a draft.
var ErrDraftSaved = errors.New("草稿已保存")

// ErrViewOnly indicates the user chose "view full configuration" in edit mode.
var ErrViewOnly = errors.New("仅查看配置")

// Navigation sentinels accepted inside any prompt as ":" commands.
var (
	ErrStepBack = errors.New("返回上一步")
	ErrStepEdit = errors.New("重新编辑当前步骤")
	ErrStepSkip = errors.New("跳过当前步骤")
	ErrStepQuit = errors.New("退出并保存草稿")
)

// StepAction is the result of the step-level navigation menu.
type StepAction int

const (
	StepNext StepAction = iota
	StepBack
	StepEdit
	StepSkip
	StepQuit
)

// Navigator is implemented by interactive prompters that can show the
// step-level menu. Test prompters may omit it; the wizard then advances.
type Navigator interface {
	ChooseStepAction(title string, index, total int, canBack, optional bool) (StepAction, error)
}

// Prompter isolates the interactive workflow from terminal I/O so the wizard
// and commands can be unit tested with scripted answers.
type Prompter interface {
	Input(label, defaultValue string, validate func(string) error) (string, error)
	Confirm(label string, defaultValue bool) (bool, error)
	Select(label string, options []string, defaultIndex int) (int, error)
	MultiSelect(label string, options []string, defaults []bool) ([]int, error)
}

// TerminalPrompter is a dependency-free line-oriented interactive prompt.
type TerminalPrompter struct {
	reader *bufio.Reader
	out    io.Writer
}

func NewTerminalPrompter(in io.Reader, out io.Writer) *TerminalPrompter {
	return &TerminalPrompter{reader: bufio.NewReader(in), out: out}
}

func (p *TerminalPrompter) readLine() (string, error) {
	line, err := p.reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) != "" {
			line = strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		} else {
			return "", ErrCancelled
		}
	} else {
		line = strings.TrimSpace(strings.TrimRight(line, "\r\n"))
	}
	// ":" prefixed commands work in every prompt without colliding with
	// ordinary values such as an application named "b".
	switch line {
	case ":b":
		return "", ErrStepBack
	case ":e":
		return "", ErrStepEdit
	case ":s":
		return "", ErrStepSkip
	case ":q":
		return "", ErrStepQuit
	}
	return line, nil
}

// ChooseStepAction prints the navigation menu required at every wizard stage.
func (p *TerminalPrompter) ChooseStepAction(title string, index, total int, canBack, optional bool) (StepAction, error) {
	fmt.Fprintf(p.out, "\n========== 步骤 %d/%d: %s ==========\n", index+1, total, title)
	fmt.Fprintln(p.out, "[Enter] 继续  [e] 重新编辑当前步骤  [b] 返回上一步  [s] 跳过  [q] 退出并保存草稿")
	fmt.Fprintln(p.out, "提示：在任何输入处可用 :b / :e / :s / :q 执行相同操作。")
	for {
		fmt.Fprint(p.out, "操作: ")
		raw, err := p.readLine()
		if err != nil {
			return StepNext, err
		}
		switch strings.ToLower(raw) {
		case "":
			return StepNext, nil
		case "e":
			return StepEdit, nil
		case "b":
			if !canBack {
				fmt.Fprintln(p.out, "已经是第一步，无法返回。")
				continue
			}
			return StepBack, nil
		case "s":
			if !optional {
				fmt.Fprintln(p.out, "该步骤为必填，不能跳过。")
				continue
			}
			return StepSkip, nil
		case "q":
			return StepQuit, nil
		default:
			fmt.Fprintln(p.out, "请输入 Enter、e、b、s 或 q。")
		}
	}
}

func (p *TerminalPrompter) Input(label, defaultValue string, validate func(string) error) (string, error) {
	for {
		if defaultValue != "" {
			fmt.Fprintf(p.out, "%s [%s]: ", label, defaultValue)
		} else {
			fmt.Fprintf(p.out, "%s: ", label)
		}
		raw, err := p.readLine()
		if err != nil {
			fmt.Fprintln(p.out)
			return "", err
		}
		if raw == "" {
			raw = defaultValue
		}
		if validate != nil {
			if err := validate(raw); err != nil {
				fmt.Fprintf(p.out, "输入无效: %v\n", err)
				continue
			}
		}
		return raw, nil
	}
}

func (p *TerminalPrompter) Confirm(label string, defaultValue bool) (bool, error) {
	suffix := "[y/N]"
	if defaultValue {
		suffix = "[Y/n]"
	}
	for {
		fmt.Fprintf(p.out, "%s %s: ", label, suffix)
		raw, err := p.readLine()
		if err != nil {
			fmt.Fprintln(p.out)
			return false, err
		}
		switch strings.ToLower(raw) {
		case "":
			return defaultValue, nil
		case "y", "yes", "是":
			return true, nil
		case "n", "no", "否":
			return false, nil
		default:
			fmt.Fprintln(p.out, "请输入 y 或 n。")
		}
	}
}

func (p *TerminalPrompter) Select(label string, options []string, defaultIndex int) (int, error) {
	if len(options) == 0 {
		return 0, fmt.Errorf("没有可选项")
	}
	fmt.Fprintln(p.out, label)
	for i, option := range options {
		marker := " "
		if i == defaultIndex {
			marker = "*"
		}
		fmt.Fprintf(p.out, "  %s %d) %s\n", marker, i+1, option)
	}
	for {
		fmt.Fprintf(p.out, "选择 [%d]: ", defaultIndex+1)
		raw, err := p.readLine()
		if err != nil {
			fmt.Fprintln(p.out)
			return 0, err
		}
		if raw == "" {
			return defaultIndex, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > len(options) {
			fmt.Fprintf(p.out, "请输入 1-%d 之间的编号。\n", len(options))
			continue
		}
		return n - 1, nil
	}
}

func (p *TerminalPrompter) MultiSelect(label string, options []string, defaults []bool) ([]int, error) {
	if len(options) == 0 {
		return nil, nil
	}
	fmt.Fprintln(p.out, label)
	for i, option := range options {
		checked := ""
		if i < len(defaults) && defaults[i] {
			checked = "x"
		}
		fmt.Fprintf(p.out, "  [%s] %d) %s\n", checked, i+1, option)
	}
	fmt.Fprintln(p.out, "输入编号（逗号分隔），0 表示全不选，回车使用默认值。")
	for {
		fmt.Fprint(p.out, "选择: ")
		raw, err := p.readLine()
		if err != nil {
			fmt.Fprintln(p.out)
			return nil, err
		}
		if raw == "" {
			var selected []int
			for i, def := range defaults {
				if i < len(options) && def {
					selected = append(selected, i)
				}
			}
			return selected, nil
		}
		if raw == "0" {
			return nil, nil
		}
		parts := strings.Split(raw, ",")
		seen := map[int]bool{}
		var selected []int
		valid := true
		for _, part := range parts {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(options) {
				fmt.Fprintf(p.out, "无效编号 %q，请输入 1-%d。\n", strings.TrimSpace(part), len(options))
				valid = false
				break
			}
			if !seen[n-1] {
				seen[n-1] = true
				selected = append(selected, n-1)
			}
		}
		if valid {
			return selected, nil
		}
	}
}
