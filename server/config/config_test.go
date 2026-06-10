package config

import (
	"testing"
)

func TestConfig_Load(t *testing.T) {
	t.Setenv("JPRQ_DOMAIN", "tulki.uz")
	t.Setenv("JPRQ_TLS_KEY", "key.pem")
	t.Setenv("JPRQ_TLS_CERT", "cert.pem")

	config := &Config{}
	if err := config.Load(); err != nil {
		t.Fatalf("Error while loading the config: %v", err.Error())
	}
}

func TestConfig_loadEmptyEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		errText string
	}{
		{
			name:    "missing domain",
			env:     map[string]string{"JPRQ_TLS_KEY": "k", "JPRQ_TLS_CERT": "c"},
			errText: "jprq domain env is not set",
		},
		{
			name:    "missing tls key",
			env:     map[string]string{"JPRQ_DOMAIN": "tulki.uz", "JPRQ_TLS_CERT": "c"},
			errText: "TLS key/cert file is missing",
		},
		{
			name:    "missing tls cert",
			env:     map[string]string{"JPRQ_DOMAIN": "tulki.uz", "JPRQ_TLS_KEY": "k"},
			errText: "TLS key/cert file is missing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure unrelated envs are cleared for this subtest.
			for _, k := range []string{"JPRQ_DOMAIN", "JPRQ_TLS_KEY", "JPRQ_TLS_CERT"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			config := &Config{}
			err := config.Load()
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.errText)
			}
			if err.Error() != tc.errText {
				t.Fatalf("expected %q, got %q", tc.errText, err.Error())
			}
		})
	}
}
