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
			return strings.TrimSpace(strings.TrimRight(line, "\r\n")), nil
		}
		return "", ErrCancelled
	}
	return strings.TrimSpace(strings.TrimRight(line, "\r\n")), nil
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
