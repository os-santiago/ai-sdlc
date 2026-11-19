package modelbroker

import "testing"

func TestInstallationTimeoutIsBoundedAndDefaultUnchanged(t *testing.T) {
	for value, want := range map[string]int{"": 0, "30": 30, "330": 330, "600": 600} {
		if got, err := GatewayTimeoutSeconds(value); err != nil || got != want {
			t.Fatal(value, got, err)
		}
	}
	for _, value := range []string{"29", "601", "abc", "060", "-1", " 60"} {
		if _, err := GatewayTimeoutSeconds(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
	base := ProviderConfig{Endpoint: "http://127.0.0.1:8787/v1/chat/completions", BearerToken: "0123456789abcdef0123456789abcdef", AllowLoopbackHTTP: true}
	for _, seconds := range []int{0, 30, 600} {
		p, err := NewProvider(ProviderConfig{Endpoint: base.Endpoint, BearerToken: base.BearerToken, AllowLoopbackHTTP: true, TimeoutSeconds: seconds})
		if err != nil {
			t.Fatal(seconds, err)
		}
		want := seconds
		if want == 0 {
			want = 30
		}
		if int(p.client.Timeout.Seconds()) != want {
			t.Fatal("timeout", seconds, p.client.Timeout)
		}
	}
	for _, seconds := range []int{1, 29, 601, -5} {
		if _, err := NewProvider(ProviderConfig{Endpoint: base.Endpoint, BearerToken: base.BearerToken, AllowLoopbackHTTP: true, TimeoutSeconds: seconds}); err == nil {
			t.Fatal("unbounded timeout accepted", seconds)
		}
	}
}
