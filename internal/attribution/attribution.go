// Package attribution estimates which container/category causes a given
// power draw, by time correlation with Beszel's CPU metrics. It's a
// heuristic estimate, never presented as an exact measurement (see CLAUDE.md).
package attribution

import (
	"encoding/json"
	"fmt"
	"os"
)

const (
	CategorySystem  = "system"
	CategoryUser    = "user"
	CategoryUnknown = "unknown"

	// BaselineContainer is the conventional name for the power share below
	// the idle baseline, not attributable to any specific container.
	BaselineContainer = "__baseline__"
)

// ContainerEntry maps a container to a UI-readable category.
type ContainerEntry struct {
	Category string `json:"category"`
	Label    string `json:"label,omitempty"`
}

// Config is the static container->category mapping, loaded from a file
// (never hardcoded in Go, see CLAUDE.md).
type Config struct {
	Containers      map[string]ContainerEntry `json:"containers"`
	DefaultCategory string                    `json:"default_category"`
}

// LoadConfig reads the mapping from a JSON file. An empty path means no
// mapping configured: everything falls back to DefaultCategory ("unknown").
func LoadConfig(path string) (Config, error) {
	if path == "" {
		return Config{DefaultCategory: CategoryUnknown}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading attribution config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing attribution config %s: %w", path, err)
	}
	if cfg.DefaultCategory == "" {
		cfg.DefaultCategory = CategoryUnknown
	}
	return cfg, nil
}

// CategoryFor returns the mapped category for the container, or
// DefaultCategory if it's not in the config.
func (c Config) CategoryFor(container string) string {
	if e, ok := c.Containers[container]; ok && e.Category != "" {
		return e.Category
	}
	return c.DefaultCategory
}

// ContainerLoad is a container's average load in an hourly window.
type ContainerLoad struct {
	Name   string
	CpuAvg float64
}

// Bucket is one row of attribution_buckets.
type Bucket struct {
	Container      string
	Category       string
	WattsAllocated float64
}

// Allocate implements the heuristic from PLAN.md: the power delta above the
// idle baseline is allocated to the container with the highest CPU peak in
// the window; the share below baseline stays a "__baseline__"/system bucket.
// Always returns rows whose WattsAllocated sums to wattsAvg.
func Allocate(wattsAvg float64, containers []ContainerLoad, baselineWatts float64, cfg Config) []Bucket {
	if baselineWatts < 0 {
		baselineWatts = 0
	}
	delta := wattsAvg - baselineWatts
	if delta <= 0 || len(containers) == 0 {
		return []Bucket{{Container: BaselineContainer, Category: CategorySystem, WattsAllocated: wattsAvg}}
	}

	peak := containers[0]
	for _, c := range containers[1:] {
		if c.CpuAvg > peak.CpuAvg {
			peak = c
		}
	}

	return []Bucket{
		{Container: BaselineContainer, Category: CategorySystem, WattsAllocated: baselineWatts},
		{Container: peak.Name, Category: cfg.CategoryFor(peak.Name), WattsAllocated: delta},
	}
}
