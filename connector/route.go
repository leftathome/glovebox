package connector

type Route struct {
	Match       string `json:"match"`
	Destination string `json:"destination"`
}

type Router struct {
	routes []Route
}

func NewRouter(routes []Route) *Router {
	return &Router{routes: routes}
}

func (r *Router) Match(key string) (string, bool) {
	for _, route := range r.routes {
		if route.Match == key || route.Match == "*" {
			return route.Destination, true
		}
	}
	return "", false
}
