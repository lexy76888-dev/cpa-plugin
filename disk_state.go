package main

import (
	"os"
	"path/filepath"
)

type PluginDiskState struct {
	Config   Config                       `json:"config"`
	Accounts map[string]AccountAnnotation `json:"accounts,omitempty"`
	Groups   map[string]GroupAnnotation   `json:"groups,omitempty"`
}

// PersistentState is deliberately separate from PluginDiskState: the former
// contains only scheduler-control hashes and epochs, never host auth material.

var defaultStatePath = resolveDefaultStatePath

func resolveDefaultStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "CLIProxyAPI", PluginID, "state.json")
}

func normalizePluginDiskState(state PluginDiskState) PluginDiskState {
	cfg := state.Config
	if cfg.QuotaRefreshInterval <= 0 && cfg.StaleAfter <= 0 && cfg.MonthlyMode == "" && len(cfg.ProbeSendWindows) == 0 && len(cfg.ProbeTriggerMinutes) == 0 {
		cfg = DefaultConfig()
	}
	cfg = NormalizeConfig(cfg)
	if _, err := validateQuotaEndpoint(cfg.QuotaEndpoint); err != nil {
		cfg.QuotaEndpoint = DefaultConfig().QuotaEndpoint
	}
	annotations := NormalizeAnnotationState(AnnotationState{
		Accounts: state.Accounts,
		Groups:   state.Groups,
	})
	state.Config = cfg
	state.Accounts = annotations.Accounts
	state.Groups = annotations.Groups
	return state
}

func diskStateFromStore(store *PluginState) PluginDiskState {
	if store == nil {
		return PluginDiskState{Config: DefaultConfig()}
	}
	annotations := store.Annotations()
	return PluginDiskState{
		Config:   store.Config(),
		Accounts: annotations.Accounts,
		Groups:   annotations.Groups,
	}
}
