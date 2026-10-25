package controller

import "testing"

func TestGatewayConfigValidation(t *testing.T) {
	cases := map[string]struct {
		config GatewayConfig
		valid  bool
	}{
		"url only":               {GatewayConfig{AdminURL: "http://kawa-gateway.kawa.svc:8080"}, true},
		"client credentials":     {GatewayConfig{AdminURL: "https://kawa", ClientID: "kawa-operator", ClientSecretFile: "/s"}, true},
		"no url":                 {GatewayConfig{}, false},
		"relative url":           {GatewayConfig{AdminURL: "kawa-gateway:8080"}, false},
		"other scheme":           {GatewayConfig{AdminURL: "ftp://kawa"}, false},
		"client id without file": {GatewayConfig{AdminURL: "http://kawa", ClientID: "kawa-operator"}, false},
		"secret file without id": {GatewayConfig{AdminURL: "http://kawa", ClientSecretFile: "/s"}, false},
	}
	for name, c := range cases {
		if err := c.config.Validate(); (err == nil) != c.valid {
			t.Errorf("%s: Validate() = %v, want valid=%v", name, err, c.valid)
		}
	}
}
