package httpapi

import (
	"context"
	"github.com/FastR-D/FastTask/internal/panelweather"
	"github.com/danielgtaylor/huma/v2"
	"net/http"
)

// This is public fixed-location weather only; there are no user data, keys,
// arbitrary upstream URLs, or client-controlled coordinates on this route.
func (s panelRoutes) registerPanelWeather(api huma.API) {
	register(api, "get-panel-weather", http.MethodGet, "/panel/weather", "Read cached Dongxihu public weather", publicSecurity(), func(ctx context.Context, _ *struct{}) (*itemResponse[panelweather.Snapshot], error) {
		return &itemResponse[panelweather.Snapshot]{Body: panelweather.Default.Get(ctx)}, nil
	})
}
