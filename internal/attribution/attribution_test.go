package attribution

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigParsesMappingAndDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "attribution.json")
	content := `{
		"containers": {
			"immich_server": {"category": "user", "label": "Immich"},
			"beszel": {"category": "system"}
		},
		"default_category": "unknown"
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.CategoryFor("immich_server") != CategoryUser {
		t.Errorf("immich_server should be user")
	}
	if cfg.CategoryFor("beszel") != CategorySystem {
		t.Errorf("beszel should be system")
	}
	if cfg.CategoryFor("something_unmapped") != CategoryUnknown {
		t.Errorf("an unmapped container should fall back to default_category")
	}
}

func TestLoadConfigEmptyPath(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\"): %v", err)
	}
	if cfg.CategoryFor("anything") != CategoryUnknown {
		t.Errorf("without a config, everything should fall back to unknown")
	}
}

func TestAllocateBelowBaselineIsAllSystem(t *testing.T) {
	buckets := Allocate(30, []ContainerLoad{{Name: "immich_server", CpuAvg: 50}}, 40, Config{DefaultCategory: CategoryUnknown})
	if len(buckets) != 1 {
		t.Fatalf("expected 1 row below baseline, got %d", len(buckets))
	}
	if buckets[0].Container != BaselineContainer || buckets[0].WattsAllocated != 30 {
		t.Errorf("unexpected row: %+v", buckets[0])
	}
}

func TestAllocateAboveBaselinePicksPeakCpu(t *testing.T) {
	cfg := Config{Containers: map[string]ContainerEntry{
		"immich_server": {Category: CategoryUser},
		"beszel":        {Category: CategorySystem},
	}, DefaultCategory: CategoryUnknown}

	buckets := Allocate(50, []ContainerLoad{
		{Name: "beszel", CpuAvg: 1.2},
		{Name: "immich_server", CpuAvg: 42.0},
	}, 20, cfg)

	if len(buckets) != 2 {
		t.Fatalf("expected 2 rows (baseline + peak), got %d: %+v", len(buckets), buckets)
	}
	if buckets[0].Container != BaselineContainer || buckets[0].WattsAllocated != 20 {
		t.Errorf("unexpected baseline row: %+v", buckets[0])
	}
	if buckets[1].Container != "immich_server" || buckets[1].Category != CategoryUser || buckets[1].WattsAllocated != 30 {
		t.Errorf("unexpected peak row: %+v", buckets[1])
	}
}

func TestAllocateSumsToWattsAvg(t *testing.T) {
	buckets := Allocate(73.5, []ContainerLoad{{Name: "x", CpuAvg: 10}}, 15, Config{DefaultCategory: CategoryUnknown})
	var sum float64
	for _, b := range buckets {
		sum += b.WattsAllocated
	}
	if sum != 73.5 {
		t.Errorf("allocated sum = %v, want 73.5", sum)
	}
}
