package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"site-observability/internal/config"
	"site-observability/internal/output"
)

// Draft is the resumable state of an unfinished create wizard. The stage ID
// lets the wizard reopen at the next step instead of starting over.
type Draft struct {
	Stage string       `yaml:"stage"`
	Site  *config.Site `yaml:"site"`
}

// DraftDir returns the hidden draft directory under the sites directory.
func DraftDir(sitesDir string) string {
	return filepath.Join(sitesDir, ".drafts")
}

// DraftPath returns the draft file for a site ID.
func DraftPath(sitesDir, siteID string) string {
	return filepath.Join(DraftDir(sitesDir), siteID+".yaml.draft")
}

// SaveDraft writes the current state atomically. Drafts never take part in
// dashboard generation and never overwrite the formal YAML.
func SaveDraft(sitesDir string, site *config.Site, stage string) (string, error) {
	if sitesDir == "" || site == nil || site.Metadata.ID == "" {
		return "", nil
	}
	path := DraftPath(sitesDir, site.Metadata.ID)
	data, err := yaml.Marshal(Draft{Stage: stage, Site: site})
	if err != nil {
		return "", fmt.Errorf("序列化草稿: %w", err)
	}
	if err := output.WriteAtomic(path, data, 0o644, false); err != nil {
		return "", fmt.Errorf("写入草稿 %s: %w", path, err)
	}
	return path, nil
}

// LoadDraft reads a draft file. It is intentionally permissive: a partially
// edited draft must still be recoverable.
func LoadDraft(path string) (*Draft, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var draft Draft
	if err := yaml.Unmarshal(data, &draft); err != nil {
		return nil, fmt.Errorf("解析草稿 %s: %w", path, err)
	}
	if draft.Site == nil {
		return nil, fmt.Errorf("草稿 %s 缺少 site 内容", path)
	}
	if draft.Stage == "" {
		draft.Stage = "metadata"
	}
	config.Normalize(draft.Site)
	return &draft, nil
}

// FindDrafts returns draft files in deterministic order.
func FindDrafts(sitesDir string) ([]string, error) {
	dir := DraftDir(sitesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".draft") {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// DeleteDraft removes one draft, ignoring an already-missing file.
func DeleteDraft(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
