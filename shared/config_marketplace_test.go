package shared

import (
	"testing"
	"time"
)

func TestLoadAPI_MarketplaceDefaultsEnabledWithOneMinuteSweep(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("MARKETPLACE_ENABLED", "")
	t.Setenv("MARKETPLACE_SWEEP_INTERVAL", "")
	t.Setenv("MARKETPLACE_SWEEP_BATCH", "")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	if !cfg.MarketplaceEnabled {
		t.Fatal("marketplace must default enabled")
	}
	if cfg.MarketplaceSweepInterval != time.Minute {
		t.Fatalf("sweep interval = %s, want 1m", cfg.MarketplaceSweepInterval)
	}
	if cfg.MarketplaceSweepBatch != 100 {
		t.Fatalf("sweep batch = %d, want 100", cfg.MarketplaceSweepBatch)
	}
}

func TestLoadAPI_MarketplaceCanBeDisabledAndTuned(t *testing.T) {
	setRequiredAPIEnv(t)
	t.Setenv("MARKETPLACE_ENABLED", "false")
	t.Setenv("MARKETPLACE_SWEEP_INTERVAL", "15s")
	t.Setenv("MARKETPLACE_SWEEP_BATCH", "25")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	if cfg.MarketplaceEnabled {
		t.Fatal("MARKETPLACE_ENABLED=false must disable marketplace")
	}
	if cfg.MarketplaceSweepInterval != 15*time.Second || cfg.MarketplaceSweepBatch != 25 {
		t.Fatalf("sweep config = %s/%d", cfg.MarketplaceSweepInterval, cfg.MarketplaceSweepBatch)
	}
}

func TestLoadAPI_MarketplaceRejectsInvalidConfig(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		setRequiredAPIEnv(t)
		t.Setenv("MARKETPLACE_ENABLED", "sometimes")
		if _, err := LoadAPI(); err == nil {
			t.Fatal("LoadAPI() must reject invalid MARKETPLACE_ENABLED")
		}
	})
	t.Run("interval", func(t *testing.T) {
		setRequiredAPIEnv(t)
		t.Setenv("MARKETPLACE_SWEEP_INTERVAL", "nope")
		if _, err := LoadAPI(); err == nil {
			t.Fatal("LoadAPI() must reject invalid MARKETPLACE_SWEEP_INTERVAL")
		}
	})
	t.Run("batch", func(t *testing.T) {
		setRequiredAPIEnv(t)
		t.Setenv("MARKETPLACE_SWEEP_BATCH", "nope")
		if _, err := LoadAPI(); err == nil {
			t.Fatal("LoadAPI() must reject invalid MARKETPLACE_SWEEP_BATCH")
		}
	})
}
