package metrics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"

	"gopkg.in/yaml.v3"

	templateassets "site-observability/templates"
)

// Registry is the authoritative set of metric profiles.
type Registry struct {
	Version  string
	Digest   string
	Profiles map[string]*Profile
	Order    []string
}

// LoadDefault loads the embedded, versioned metric templates.
func LoadDefault() (*Registry, error) {
	return LoadFS(templateassets.FS, "metrics")
}

// LoadFS loads and validates all *.yaml metric profile files in dir.
func LoadFS(fsys fs.FS, dir string) (*Registry, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("查找指标模板: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("未找到任何指标模板")
	}
	sort.Strings(files)

	reg := &Registry{Profiles: map[string]*Profile{}}
	var digest bytes.Buffer
	version := ""
	for _, file := range files {
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("读取指标模板 %s: %w", file, err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var tf templateFile
		if err := dec.Decode(&tf); err != nil {
			return nil, fmt.Errorf("解析指标模板 %s: %w", file, err)
		}
		if err := ensureEOF(dec); err != nil {
			return nil, fmt.Errorf("解析指标模板 %s: %w", file, err)
		}
		if !versionRE.MatchString(tf.Version) {
			return nil, fmt.Errorf("指标模板 %s: version 必须为语义版本（如 1.0.0）", file)
		}
		if version == "" {
			version = tf.Version
		} else if version != tf.Version {
			return nil, fmt.Errorf("指标模板 %s: version %s 与注册表版本 %s 不一致", file, tf.Version, version)
		}
		if len(tf.Profiles) == 0 {
			return nil, fmt.Errorf("指标模板 %s: profiles 不能为空", file)
		}
		for i := range tf.Profiles {
			p := &tf.Profiles[i]
			if err := validateProfile(p); err != nil {
				return nil, fmt.Errorf("指标模板 %s: %w", file, err)
			}
			if _, exists := reg.Profiles[p.ID]; exists {
				return nil, fmt.Errorf("指标模板 %s: profile %q 重复定义", file, p.ID)
			}
			reg.Profiles[p.ID] = p
			reg.Order = append(reg.Order, p.ID)
		}
		digest.WriteString(file)
		digest.WriteByte('\n')
		digest.Write(data)
		digest.WriteByte('\n')
	}
	reg.Version = version
	sum := sha256.Sum256(digest.Bytes())
	reg.Digest = hex.EncodeToString(sum[:8])
	return reg, nil
}

func ensureEOF(dec *yaml.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == nil {
		return fmt.Errorf("只允许一个 YAML 文档")
	}
	if err != io.EOF {
		return err
	}
	return nil
}

// Profile returns a profile by ID.
func (r *Registry) Profile(id string) (*Profile, bool) {
	p, ok := r.Profiles[id]
	return p, ok
}

// ProfilesForApplication returns selectable profiles for applications.
func (r *Registry) ProfilesForApplication() []*Profile {
	var out []*Profile
	for _, id := range r.Order {
		p := r.Profiles[id]
		if p.Auto || p.Target != targetApplication {
			continue
		}
		out = append(out, p)
	}
	return out
}

// ProfilesForComponent returns selectable profiles for a component type.
func (r *Registry) ProfilesForComponent(componentType string) []*Profile {
	var out []*Profile
	for _, id := range r.Order {
		p := r.Profiles[id]
		if p.Auto || p.Target != targetComponent {
			continue
		}
		if len(p.ComponentTypes) > 0 && !contains(p.ComponentTypes, componentType) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// AutoProfile returns an embedded automatic profile.
func (r *Registry) AutoProfile(id string) (*Profile, bool) {
	p, ok := r.Profiles[id]
	if !ok || !p.Auto {
		return nil, false
	}
	return p, true
}

// TemplateVersionTag is embedded in generated dashboards.
func (r *Registry) TemplateVersionTag() string {
	return "sitegen-template:" + r.Version + "+" + r.Digest
}
