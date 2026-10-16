package main

import "testing"

func TestProxyBaseURL(t *testing.T) {
	cases := []struct{ api, proxy, want string }{
		{"", "", "https://llm.promptster.ai"},                                    // hosted: through the WAF-proof edge
		{"https://self.example", "", "https://self.example"},                     // self-hosted serves its own proxy
		{"", "https://edge.example", "https://edge.example"},                     // explicit override wins
		{"https://self.example", "https://edge.example", "https://edge.example"}, // even over a self-hosted API
	}
	for _, c := range cases {
		t.Setenv("PROMPTSTER_API_URL", c.api)
		t.Setenv("PROMPTSTER_PROXY_URL", c.proxy)
		if got := proxyBaseURL(); got != c.want {
			t.Errorf("api=%q proxy=%q: got %q, want %q", c.api, c.proxy, got, c.want)
		}
	}
}
