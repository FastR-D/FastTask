package panelweather

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type weatherTransport func(*http.Request) (*http.Response, error)

func (f weatherTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func weatherClient(response func() (int, string)) *http.Client {
	return &http.Client{Transport: weatherTransport(func(r *http.Request) (*http.Response, error) {
		status, body := response()
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
}

func TestCurrentWeatherCacheAndStaleExpiry(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600))
	fail := false
	calls := 0
	client := weatherClient(func() (int, string) {
		calls++
		if fail {
			return http.StatusBadGateway, ""
		}
		return http.StatusOK, `{"timezone":"Asia/Shanghai","current":{"time":"2026-10-25T12:00","temperature_2m":22.3,"weather_code":0},"daily":{"time":["2026-10-25"],"temperature_2m_min":[15.9],"temperature_2m_max":[24.4]}}`
	})
	c := NewClient(client, endpoint, func() time.Time { return now })
	first := c.Get(context.Background())
	if first.Status != "ok" || first.Summary != "晴 22°C" || first.Location != "武汉市东西湖区" || first.FetchedAt == nil {
		_, diagnostic := c.fetch(context.Background(), now)
		t.Fatalf("unexpected current weather: %+v; fetch error: %v", first, diagnostic)
	}
	now = now.Add(14 * time.Minute)
	if got := c.Get(context.Background()); got.Status != "ok" || calls != 1 {
		t.Fatalf("cache was not used: %+v; calls=%d", got, calls)
	}
	fail = true
	now = now.Add(2 * time.Minute)
	stale := c.Get(context.Background())
	if stale.Status != "stale" || stale.Summary != first.Summary || stale.Message == "" || !stale.FetchedAt.Equal(*first.FetchedAt) {
		t.Fatalf("provider failure must label cached result: %+v", stale)
	}
	now = now.Add(6 * time.Hour)
	if got := c.Get(context.Background()); got.Status != "unavailable" || got.Temperature != nil || got.FetchedAt != nil {
		t.Fatalf("old weather must not be presented as current: %+v", got)
	}
}

func TestWeatherDoesNotReusePreviousDay(t *testing.T) {
	now := time.Date(2026, 10, 25, 23, 50, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600))
	fail := false
	client := weatherClient(func() (int, string) {
		if fail {
			return http.StatusServiceUnavailable, ""
		}
		return http.StatusOK, `{"timezone":"Asia/Shanghai","current":{"time":"2026-10-25T23:45","temperature_2m":20,"weather_code":2},"daily":{"time":["2026-10-25"],"temperature_2m_min":[15],"temperature_2m_max":[24]}}`
	})
	c := NewClient(client, endpoint, func() time.Time { return now })
	if got := c.Get(context.Background()); got.Status != "ok" {
		t.Fatalf("setup: %+v", got)
	}
	fail = true
	now = now.Add(11 * time.Minute)
	if got := c.Get(context.Background()); got.Status != "unavailable" || got.LocalDate != "2026-10-26" || got.Temperature != nil {
		t.Fatalf("yesterday cannot be used for today's weather: %+v", got)
	}
}

func TestWeatherRejectsIncompleteOrMisdatedProviderData(t *testing.T) {
	valid := `{"timezone":"Asia/Shanghai","current":{"time":"2026-10-25T12:00","temperature_2m":22,"weather_code":0},"daily":{"time":["2026-10-25"],"temperature_2m_min":[15],"temperature_2m_max":[24]}}`
	cases := map[string]string{
		"missing temperature": strings.Replace(valid, `"temperature_2m":22`, `"temperature_2m":null`, 1),
		"old observation":     strings.Replace(valid, `T12:00`, `T09:00`, 1),
		"future observation":  strings.Replace(valid, `T12:00`, `T13:00`, 1),
		"wrong timezone":      strings.Replace(valid, `Asia/Shanghai`, `UTC`, 1),
		"wrong daily date":    strings.Replace(valid, `["2026-10-25"]`, `["2026-10-24"]`, 1),
		"wrong range":         strings.Replace(valid, `"temperature_2m_max":[24]`, `"temperature_2m_max":[14]`, 1),
		"invalid JSON":        `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			client := weatherClient(func() (int, string) { return http.StatusOK, body })
			c := NewClient(client, endpoint, func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600)) })
			got := c.Get(context.Background())
			if got.Status != "unavailable" || got.Temperature != nil || got.Code != nil || got.Message == "" {
				t.Fatalf("bad provider data escaped: %+v", got)
			}
		})
	}
}
