// Package homeassistant periodically reads the PUN price from the
// pun_sensor integration's entity via Home Assistant's REST API.
package homeassistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config are the connection parameters, read from the environment by the caller.
type Config struct {
	URL          string // e.g. http://homeassistant.example.lan:8123
	Token        string // long-lived access token
	PunEntityID  string // e.g. sensor.pun_orario
	PollInterval time.Duration
	HTTPTimeout  time.Duration // default 10s; a multi-day history query may need more
}

// Client polls HA periodically and writes the PUN price to pun_prices.
type Client struct {
	cfg  Config
	http *http.Client
	db   *sql.DB
}

func New(db *sql.DB, cfg Config) *Client {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Hour
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	return &Client{cfg: cfg, db: db, http: &http.Client{Timeout: cfg.HTTPTimeout}}
}

// Start polls periodically until ctx is canceled. If the PUN entity isn't
// configured or doesn't exist, it says so clearly in the logs and never
// writes a made-up price: the pricing engine falls back to fully custom
// periods until the PUN becomes available again.
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
	if c.cfg.PunEntityID == "" {
		log.Println("homeassistant: HA_PUN_ENTITY_ID not configured, PUN unavailable")
		return
	}

	value, err := c.fetchState(ctx, c.cfg.PunEntityID)
	if err != nil {
		log.Println("homeassistant: reading PUN entity failed:", err)
		return
	}

	now := time.Now().Unix()
	if err := insertPunPrice(c.db, now, value); err != nil {
		log.Println("homeassistant: writing pun_prices failed:", err)
	}
}

func (c *Client) fetchState(ctx context.Context, entityID string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+"/api/states/"+entityID, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return 0, fmt.Errorf("entity %s not found on Home Assistant (pun_sensor not installed/configured?)", entityID)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("http %d: %s", resp.StatusCode, string(b))
	}

	var out struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if out.State == "unavailable" || out.State == "unknown" {
		return 0, fmt.Errorf("entity %s is %q on Home Assistant", entityID, out.State)
	}

	v, err := strconv.ParseFloat(out.State, 64)
	if err != nil {
		return 0, fmt.Errorf("state %q is not numeric: %w", out.State, err)
	}
	return v, nil
}

// StatePoint is a historical reading of an HA entity.
type StatePoint struct {
	Time  time.Time
	Value float64
}

// FetchHistory reads the history of several entities from since to now (HA's
// /api/history/period API), for a one-off backfill. Non-numeric states
// (unavailable/unknown) are dropped, never turned into a made-up zero.
//
// end_time is explicit: without it, HA's API defaults to only 1 day past
// since, silently truncating the rest (found running a real backfill, not
// documented — hence the parameter).
func (c *Client) FetchHistory(ctx context.Context, entityIDs []string, since time.Time) (map[string][]StatePoint, error) {
	q := url.Values{}
	q.Set("filter_entity_id", strings.Join(entityIDs, ","))
	q.Set("end_time", time.Now().UTC().Format(time.RFC3339))
	path := fmt.Sprintf("/api/history/period/%s?%s", since.UTC().Format(time.RFC3339), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(b))
	}

	// Not minimal_response: every state carries its own entity_id, so we
	// group them ourselves instead of relying on group order.
	var raw [][]struct {
		EntityID    string `json:"entity_id"`
		State       string `json:"state"`
		LastChanged string `json:"last_changed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	result := map[string][]StatePoint{}
	for _, group := range raw {
		for _, s := range group {
			if s.EntityID == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, s.LastChanged)
			if err != nil {
				continue
			}
			v, err := strconv.ParseFloat(s.State, 64)
			if err != nil {
				continue // unavailable/unknown/non-numeric: dropped, never a made-up value
			}
			result[s.EntityID] = append(result[s.EntityID], StatePoint{Time: t, Value: v})
		}
	}
	return result, nil
}

func insertPunPrice(db *sql.DB, ts int64, punEurKwh float64) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO pun_prices (ts, pun_eur_kwh) VALUES (?, ?)`,
		ts, punEurKwh,
	)
	return err
}
