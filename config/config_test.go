package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/amberpixels/f10-drivers/config"
)

func TestNotionID(t *testing.T) {
	const id = "1a2b3c4d00004000800000000000db01"

	for _, in := range []string{
		id,
		"1a2b3c4d-0000-4000-8000-00000000db01",
		"https://www.notion.so/team/Marketing-tasks-" + id + "?v=0123456789abcdef0123456789abcdef",
		"https://www.notion.so/" + id,
	} {
		got, ok := config.NotionID(in)
		if !ok || got != id {
			t.Errorf("NotionID(%q) = %q, %v; want %q", in, got, ok, id)
		}
	}

	if _, ok := config.NotionID("https://www.notion.so/Marketing-tasks"); ok {
		t.Error("a url without an id should not yield one")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Draft the Q4 campaign summary":                "draft-the-q4-campaign-summary",
		"Fix: émoji & punctuation!!":                   "fix-moji-punctuation",
		"one two three four five six seven eight nine": "one-two-three-four-five-six",
	} {
		if got := config.Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadDefaultsAndPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "driver.toml")

	err := os.WriteFile(path, []byte(`prefix = "MKT"
[notion]
database = "1a2b3c4d00004000800000000000db01"
[notion.properties]
id = "ID"
[notion.priority]
urgent = "Urgent"
high = ["High", "urgent-ish"]
`), 0o644)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Branch != config.DefaultBranch || cfg.Notion.TokenEnv != "NOTION_TOKEN" || !cfg.Notion.IsDone("Done") {
		t.Errorf("defaults not applied: %+v %+v", cfg, cfg.Notion)
	}

	for value, want := range map[string]string{"urgent": "urgent", "HIGH": "high", "urgent-ish": "high", "Someday": "", "MEDIUM": "medium", "low": "low"} {
		if got := cfg.Notion.PriorityOf(value); got != want {
			t.Errorf("PriorityOf(%q) = %q, want %q", value, got, want)
		}
	}

	if got := cfg.BranchName("MKT-3", "3", "Collect channel metrics"); got != "MKT-3/collect-channel-metrics" {
		t.Errorf("BranchName = %q", got)
	}

	if got := cfg.BranchName("MKT-3", "3", "Отчёт за квартал"); got != "MKT-3" {
		t.Errorf("BranchName with an empty slug = %q, want the bare id", got)
	}

	cfg.Branch = "feat/{id}-{slug}"
	if got := cfg.BranchName("MKT-3", "3", "🚀"); got != "feat/MKT-3" {
		t.Errorf("BranchName with an empty slug = %q, want no dangling separator", got)
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("want an error for a missing config")
	}
}
