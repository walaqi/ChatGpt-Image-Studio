package config

import (
	"testing"
	"time"
)

func TestImageQueueAndQuotaRefreshDefaults(t *testing.T) {
	cfg := &Config{}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() returned error: %v", err)
	}

	maxConcurrency, queueLimit, queueTimeout := cfg.ImageQueueConfig()
	if maxConcurrency != 8 {
		t.Fatalf("maxConcurrency = %d, want 8", maxConcurrency)
	}
	if queueLimit != 32 {
		t.Fatalf("queueLimit = %d, want 32", queueLimit)
	}
	if queueTimeout != 20*time.Second {
		t.Fatalf("queueTimeout = %s, want 20s", queueTimeout)
	}
	if got := cfg.ImageTaskQueueTTL(); got != 600*time.Second {
		t.Fatalf("ImageTaskQueueTTL() = %s, want 600s", got)
	}
}

func TestImageQueueAndQuotaRefreshCustomValues(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			MaxImageConcurrency:      12,
			ImageQueueLimit:          48,
			ImageQueueTimeoutSeconds: 35,
			ImageTaskQueueTTLSeconds: 480,
		},
	}

	maxConcurrency, queueLimit, queueTimeout := cfg.ImageQueueConfig()
	if maxConcurrency != 12 {
		t.Fatalf("maxConcurrency = %d, want 12", maxConcurrency)
	}
	if queueLimit != 48 {
		t.Fatalf("queueLimit = %d, want 48", queueLimit)
	}
	if queueTimeout != 35*time.Second {
		t.Fatalf("queueTimeout = %s, want 35s", queueTimeout)
	}
	if got := cfg.ImageTaskQueueTTL(); got != 480*time.Second {
		t.Fatalf("ImageTaskQueueTTL() = %s, want 480s", got)
	}
}
