package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileError identifies which configuration file failed to parse.
type FileError struct {
	File string
	Err  error
}

func (e *FileError) Error() string {
	return fmt.Sprintf("%s: %v", e.File, e.Err)
}

func (e *FileError) Unwrap() error { return e.Err }

// DecodeStrict decodes a single YAML document into out and rejects unknown
// fields with a full field path. Map values remain extensible by design.
func DecodeStrict(data []byte, filename string, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return &FileError{File: filename, Err: errors.New("配置文件为空")}
		}
		return &FileError{File: filename, Err: err}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
		return &FileError{File: filename, Err: err}
	} else if err == nil {
		return &FileError{File: filename, Err: errors.New("只允许一个 YAML 文档")}
	}

	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return &FileError{File: filename, Err: errors.New("配置文件为空")}
		}
		root := doc.Content[0]
		t := reflect.TypeOf(out)
		if t == nil || t.Kind() != reflect.Pointer {
			return &FileError{File: filename, Err: errors.New("内部错误：解码目标不是指针")}
		}
		var unknown []string
		walkKnownFields(root, t.Elem(), "", &unknown)
		if len(unknown) > 0 {
			return &FileError{File: filename, Err: fmt.Errorf("未知字段: %s", strings.Join(unknown, "; "))}
		}
	}

	if err := doc.Decode(out); err != nil {
		return &FileError{File: filename, Err: err}
	}
	return nil
}

func walkKnownFields(node *yaml.Node, typ reflect.Type, path string, unknown *[]string) {
	if node == nil {
		return
	}
	for node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	typ = indirectType(typ)

	switch node.Kind {
	case yaml.MappingNode:
		switch typ.Kind() {
		case reflect.Struct:
			fields := yamlStructFields(typ)
			for i := 0; i+1 < len(node.Content); i += 2 {
				keyNode := node.Content[i]
				valueNode := node.Content[i+1]
				key := keyNode.Value
				field, ok := fields[key]
				if !ok {
					*unknown = append(*unknown, fmt.Sprintf("%s (行 %d)", childPath(path, key), keyNode.Line))
					continue
				}
				walkKnownFields(valueNode, field.Type, childPath(path, key), unknown)
			}
		case reflect.Map:
			for i := 0; i+1 < len(node.Content); i += 2 {
				keyNode := node.Content[i]
				valueNode := node.Content[i+1]
				walkKnownFields(valueNode, typ.Elem(), fmt.Sprintf("%s[%s]", path, keyNode.Value), unknown)
			}
		}
	case yaml.SequenceNode:
		if typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			for i, item := range node.Content {
				walkKnownFields(item, typ.Elem(), fmt.Sprintf("%s[%d]", path, i), unknown)
			}
		}
	}
}

func indirectType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func yamlStructFields(t reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := f.Tag.Get("yaml")
		name := strings.Split(tag, ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields[name] = f
	}
	return fields
}

func childPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}
