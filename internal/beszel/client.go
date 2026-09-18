// Package beszel periodically reads host/container metrics from Beszel
// (PocketBase) and writes them to resource_samples.
package beszel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Config are the connection parameters, read from the environment by the caller.
type Config struct {
	URL           string // e.g. http://beszel:8090
	AdminEmail    string
	AdminPassword string
	PollInterval  time.Duration // default 30s if zero
}

// Client polls Beszel periodically and writes to resource_samples.
type Client struct {
	cfg  Config
	http *http.Client
	db   *sql.DB

	token string
}

func New(db *sql.DB, cfg Config) *Client {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30 * time.Second
	}
	return &Client{cfg: cfg, db: db, http: &http.Client{Timeout: 10 * time.Second}}
}

// CheckConnection verifies login and reading systems; useful for diagnostics/health checks.
func (c *Client) CheckConnection(ctx context.Context) ([]string, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	systems, err := c.listSystems(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(systems))
	for i, s := range systems {
		names[i] = s.Name
	}
	return names, nil
}

// Start polls periodically until ctx is canceled. Never fails silently:
// every error (login, network, parsing) is logged and the next cycle retries.
func (c *Client) Start(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	c.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

func (c *Client) pollOnce(ctx context.Context) {
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			log.Println("beszel: login failed:", err)
			return
		}
	}

	systems, err := c.listSystems(ctx)
	if err != nil {
		if isAuthError(err) {
			c.token = "" // force a fresh login next cycle
		}
		log.Println("beszel: reading systems failed:", err)
		return
	}

	now := time.Now().Unix()
	for _, sys := range systems {
		if host, ok, err := c.latestHostStats(ctx, sys.ID); err != nil {
			log.Println("beszel: reading system_stats failed for", sys.Name, ":", err)
		} else if ok {
			if err := insertResourceSample(c.db, now, "__host__", host.cpu, host.memUsed, host.netSent, host.netRecv, &host.diskIO); err != nil {
				log.Println("beszel: writing host sample failed:", err)
			}
		}

		containers, err := c.latestContainerStats(ctx, sys.ID)
		if err != nil {
			log.Println("beszel: reading container_stats failed for", sys.Name, ":", err)
			continue
		}
		for _, ct := range containers {
			if err := insertResourceSample(c.db, now, ct.Name, ct.Cpu, ct.Mem, ct.NetSent, ct.NetRecv, nil); err != nil {
				log.Println("beszel: writing container sample failed:", err)
			}
		}
	}
}

type authError struct{ status int }

func (e *authError) Error() string { return fmt.Sprintf("unauthorized (http %d)", e.status) }
func isAuthError(err error) bool   { _, ok := err.(*authError); return ok }

func (c *Client) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"identity": c.cfg.AdminEmail,
		"password": c.cfg.AdminPassword,
	})

	// PocketBase >= 0.23 renamed the admin collection to _superusers; keep a
	// fallback to the older endpoint for compatibility with older images.
	for _, path := range []string{"/api/collections/_superusers/auth-with-password", "/api/admins/auth-with-password"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		var out struct {
			Token string `json:"token"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK && decodeErr == nil && out.Token != "" {
			c.token = out.Token
			return nil
		}
		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("login at %s: http %d", path, resp.StatusCode)
		}
	}
	return fmt.Errorf("no admin login endpoint found")
}

func (c *Client) apiGet(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &authError{status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type system struct {
	ID   string
	Name string
}

// System is the exported variant of system, for callers (e.g. the backfill
// tool) that need to resolve a name to a system ID.
type System struct {
	ID   string
	Name string
}

// ListSystems exposes the monitored systems (id + name). Logs in if needed.
func (c *Client) ListSystems(ctx context.Context) ([]System, error) {
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}
	systems, err := c.listSystems(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]System, len(systems))
	for i, s := range systems {
		out[i] = System{ID: s.ID, Name: s.Name}
	}
	return out, nil
}

// pbTimeLayout is the "created"/"updated" format in PocketBase responses.
const pbTimeLayout = "2006-01-02 15:04:05.000Z"

// HistoryPoint is a historical host metrics reading at one point in time.
type HistoryPoint struct {
	Time                                   time.Time
	Cpu, MemUsed, NetSent, NetRecv, DiskIO float64
}

// FetchSystemStatsHistory reads host metrics history for systemID starting
// at since, at the given resolution ("1m", "10m", "20m", "120m", "480m" —
// see Beszel). For a multi-day backfill, use a resolution coarser than "1m",
// which Beszel only retains briefly.
func (c *Client) FetchSystemStatsHistory(ctx context.Context, systemID, resolution string, since time.Time) ([]HistoryPoint, error) {
	var all []HistoryPoint
	for page := 1; ; page++ {
		var out struct {
			Items []struct {
				Created string           `json:"created"`
				Stats   hostStatsPayload `json:"stats"`
			} `json:"items"`
			TotalPages int `json:"totalPages"`
		}
		if err := c.apiGet(ctx, historyPath("system_stats", systemID, resolution, since, page), &out); err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			t, err := time.Parse(pbTimeLayout, it.Created)
			if err != nil {
				continue // unparsable timestamp: skip rather than abort the whole backfill
			}
			s := it.Stats
			all = append(all, HistoryPoint{
				Time: t, Cpu: s.Cpu, MemUsed: s.MemUsed,
				NetSent: float64(s.Bandwidth[0]), NetRecv: float64(s.Bandwidth[1]),
				DiskIO: float64(s.DiskIO[0] + s.DiskIO[1]),
			})
		}
		if page >= out.TotalPages || len(out.Items) == 0 {
			break
		}
	}
	return all, nil
}

// ContainerHistoryPoint is a historical container metrics reading.
type ContainerHistoryPoint struct {
	Time             time.Time
	Name             string
	Cpu, Mem         float64
	NetSent, NetRecv float64
}

// FetchContainerStatsHistory is FetchSystemStatsHistory's counterpart for
// containers: each Beszel record already holds every container of that
// system at that instant, flattened here into one point per container.
func (c *Client) FetchContainerStatsHistory(ctx context.Context, systemID, resolution string, since time.Time) ([]ContainerHistoryPoint, error) {
	var all []ContainerHistoryPoint
	for page := 1; ; page++ {
		var out struct {
			Items []struct {
				Created string                `json:"created"`
				Stats   []containerStatsEntry `json:"stats"`
			} `json:"items"`
			TotalPages int `json:"totalPages"`
		}
		if err := c.apiGet(ctx, historyPath("container_stats", systemID, resolution, since, page), &out); err != nil {
			return nil, err
		}
		for _, it := range out.Items {
			t, err := time.Parse(pbTimeLayout, it.Created)
			if err != nil {
				continue
			}
			for _, e := range it.Stats {
				all = append(all, ContainerHistoryPoint{
					Time: t, Name: e.Name, Cpu: e.Cpu, Mem: e.Mem,
					NetSent: float64(e.Bandwidth[0]), NetRecv: float64(e.Bandwidth[1]),
				})
			}
		}
		if page >= out.TotalPages || len(out.Items) == 0 {
			break
		}
	}
	return all, nil
}

func historyPath(collection, systemID, resolution string, since time.Time, page int) string {
	q := url.Values{}
	q.Set("filter", fmt.Sprintf("(system='%s' && type='%s' && created>='%s')", systemID, resolution, since.UTC().Format(pbTimeLayout)))
	q.Set("sort", "created")
	q.Set("perPage", "200")
	q.Set("page", strconv.Itoa(page))
	return "/api/collections/" + collection + "/records?" + q.Encode()
}

func (c *Client) listSystems(ctx context.Context) ([]system, error) {
	var out struct {
		Items []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := c.apiGet(ctx, "/api/collections/systems/records?perPage=200", &out); err != nil {
		return nil, err
	}
	systems := make([]system, len(out.Items))
	for i, it := range out.Items {
		systems[i] = system{ID: it.ID, Name: it.Name}
	}
	return systems, nil
}

type hostStats struct {
	cpu, memUsed, netSent, netRecv, diskIO float64
}

// hostStatsPayload mirrors the relevant fields of system.Stats (Beszel), see
// https://github.com/henrygd/beszel internal/entities/system/system.go
type hostStatsPayload struct {
	Cpu       float64   `json:"cpu"`
	MemUsed   float64   `json:"mu"`
	Bandwidth [2]uint64 `json:"b"` // [sent, recv] bytes
	DiskIO    [2]uint64 `json:"dio"`
}

func (c *Client) latestHostStats(ctx context.Context, systemID string) (hostStats, bool, error) {
	var out struct {
		Items []struct {
			Stats hostStatsPayload `json:"stats"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/api/collections/system_stats/records?filter=(system='%s'%%26%%26type='1m')&sort=-created&perPage=1", systemID)
	if err := c.apiGet(ctx, path, &out); err != nil {
		return hostStats{}, false, err
	}
	if len(out.Items) == 0 {
		return hostStats{}, false, nil
	}
	s := out.Items[0].Stats
	return hostStats{
		cpu:     s.Cpu,
		memUsed: s.MemUsed,
		netSent: float64(s.Bandwidth[0]),
		netRecv: float64(s.Bandwidth[1]),
		diskIO:  float64(s.DiskIO[0] + s.DiskIO[1]),
	}, true, nil
}

type containerStat struct {
	Name             string
	Cpu, Mem         float64
	NetSent, NetRecv float64
}

// containerStatsEntry mirrors container.Stats (Beszel).
type containerStatsEntry struct {
	Name      string    `json:"n"`
	Cpu       float64   `json:"c"`
	Mem       float64   `json:"m"`
	Bandwidth [2]uint64 `json:"b"`
}

func (c *Client) latestContainerStats(ctx context.Context, systemID string) ([]containerStat, error) {
	var out struct {
		Items []struct {
			Stats []containerStatsEntry `json:"stats"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/api/collections/container_stats/records?filter=(system='%s'%%26%%26type='1m')&sort=-created&perPage=1", systemID)
	if err := c.apiGet(ctx, path, &out); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, nil
	}
	entries := out.Items[0].Stats
	result := make([]containerStat, len(entries))
	for i, e := range entries {
		result[i] = containerStat{
			Name:    e.Name,
			Cpu:     e.Cpu,
			Mem:     e.Mem,
			NetSent: float64(e.Bandwidth[0]),
			NetRecv: float64(e.Bandwidth[1]),
		}
	}
	return result, nil
}

func insertResourceSample(db *sql.DB, ts int64, container string, cpuPct, memUsed, netSent, netRecv float64, diskIO *float64) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO resource_samples (ts, container, cpu_pct, mem_used, net_sent_bytes, net_recv_bytes, disk_io_bytes) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ts, container, cpuPct, memUsed, netSent, netRecv, diskIO,
	)
	return err
}
