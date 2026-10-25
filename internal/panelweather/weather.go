// Package panelweather provides a fixed-location, cached public weather snapshot.
package panelweather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"
)

const endpoint = "https://api.open-meteo.com/v1/forecast?latitude=30.6199&longitude=114.137&current=temperature_2m,weather_code&daily=weather_code,temperature_2m_min,temperature_2m_max&timezone=Asia%2FShanghai&forecast_days=1"

type Snapshot struct {
	Status      string     `json:"status"`
	Location    string     `json:"location"`
	LocalDate   string     `json:"local_date"`
	Summary     string     `json:"summary"`
	Temperature *float64   `json:"temperature_c"`
	Minimum     *float64   `json:"min_c"`
	Maximum     *float64   `json:"max_c"`
	Code        *int       `json:"weather_code"`
	ObservedAt  string     `json:"observed_at"`
	FetchedAt   *time.Time `json:"fetched_at"`
	Source      string     `json:"source"`
	SourceURL   string     `json:"source_url"`
	Message     string     `json:"message,omitempty"`
}

type Client struct {
	http    *http.Client
	url     string
	now     func() time.Time
	mu      sync.Mutex
	cached  Snapshot
	checked time.Time
}

var Default = NewClient(&http.Client{Timeout: 10 * time.Second}, endpoint, time.Now)

func NewClient(client *http.Client, url string, now func() time.Time) *Client {
	return &Client{http: client, url: url, now: now}
}

func base(now time.Time) Snapshot {
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	return Snapshot{Status: "unavailable", Location: "武汉市东西湖区", LocalDate: now.In(zone).Format("2006-01-02"), Summary: "天气暂不可用", Source: "Open-Meteo（模型估算）", SourceURL: "https://open-meteo.com/", Message: "天气查询暂时失败，可稍后重试或手动纠正显示。"}
}

// Get never substitutes a fabricated forecast. Only a same-day cached snapshot
// up to six hours old may be returned on a provider failure, marked stale.
func (c *Client) Get(ctx context.Context) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	blank := base(now)
	if c.cached.LocalDate == blank.LocalDate && !c.checked.IsZero() {
		ttl := 15 * time.Minute
		if c.cached.Status != "ok" {
			ttl = time.Minute
		}
		if now.Sub(c.checked) >= 0 && now.Sub(c.checked) < ttl {
			return c.cached
		}
	}
	previous := c.cached
	snapshot, err := c.fetch(ctx, now)
	c.checked = now
	if err != nil {
		if previous.LocalDate == blank.LocalDate && previous.FetchedAt != nil && now.Sub(*previous.FetchedAt) >= 0 && now.Sub(*previous.FetchedAt) <= 6*time.Hour {
			previous.Status = "stale"
			previous.Message = "天气源暂时不可用，正在显示今天的上次查询结果。"
			c.cached = previous
		} else {
			c.cached = blank
		}
		return c.cached
	}
	c.cached = snapshot
	return snapshot
}

func (c *Client) fetch(ctx context.Context, now time.Time) (Snapshot, error) {
	result := base(now)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return result, err
	}
	req.Header.Set("User-Agent", "FastTask-Panel/1.0")
	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("weather provider status %d", resp.StatusCode)
	}
	var data struct {
		Timezone string `json:"timezone"`
		Current  struct {
			Time        string   `json:"time"`
			Temperature *float64 `json:"temperature_2m"`
			Code        *int     `json:"weather_code"`
		} `json:"current"`
		Daily struct {
			Time    []string   `json:"time"`
			Minimum []*float64 `json:"temperature_2m_min"`
			Maximum []*float64 `json:"temperature_2m_max"`
		} `json:"daily"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&data); err != nil {
		return result, err
	}
	zone := time.FixedZone("Asia/Shanghai", 8*3600)
	observed, err := time.ParseInLocation("2006-01-02T15:04", data.Current.Time, zone)
	if err != nil || data.Timezone != "Asia/Shanghai" || observed.Format("2006-01-02") != result.LocalDate || now.Sub(observed) > 2*time.Hour || observed.Sub(now) > 30*time.Minute {
		return result, fmt.Errorf("weather timestamp is not current")
	}
	if data.Current.Temperature == nil || data.Current.Code == nil || len(data.Daily.Time) != 1 || data.Daily.Time[0] != result.LocalDate || len(data.Daily.Minimum) != 1 || len(data.Daily.Maximum) != 1 || data.Daily.Minimum[0] == nil || data.Daily.Maximum[0] == nil {
		return result, fmt.Errorf("weather data missing")
	}
	for _, value := range []*float64{data.Current.Temperature, data.Daily.Minimum[0], data.Daily.Maximum[0]} {
		if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < -80 || *value > 70 {
			return result, fmt.Errorf("weather temperature out of range")
		}
	}
	if *data.Daily.Minimum[0] > *data.Daily.Maximum[0] {
		return result, fmt.Errorf("weather range reversed")
	}
	result.Status = "ok"
	result.Message = ""
	result.Temperature = data.Current.Temperature
	result.Code = data.Current.Code
	result.Minimum = data.Daily.Minimum[0]
	result.Maximum = data.Daily.Maximum[0]
	result.ObservedAt = observed.Format(time.RFC3339)
	result.FetchedAt = &now
	result.Summary = fmt.Sprintf("%s %.0f°C", Description(*data.Current.Code), *data.Current.Temperature)
	return result, nil
}

func Description(code int) string {
	switch code {
	case 0:
		return "晴"
	case 1:
		return "晴间多云"
	case 2:
		return "多云"
	case 3:
		return "阴"
	case 45, 48:
		return "雾"
	case 51, 53, 55:
		return "毛毛雨"
	case 56, 57, 66, 67:
		return "冻雨"
	case 61:
		return "小雨"
	case 63:
		return "中雨"
	case 65:
		return "大雨"
	case 71, 73, 75, 77, 85, 86:
		return "雪"
	case 80, 81, 82:
		return "阵雨"
	case 95, 96, 99:
		return "雷雨"
	default:
		return "天气变化"
	}
}
