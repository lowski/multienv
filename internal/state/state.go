// Package state persists multienv's mutable settings and the history
// of services that have used each accessory. The file lives at
// ~/.multienv/state.json and is optional: a missing file is treated as
// empty state, and write failures are non-fatal at reconcile time.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// CurrentVersion is the schema version multienv writes today.
const CurrentVersion = 1

// State is the on-disk shape of ~/.multienv/state.json.
type State struct {
	Version     int                       `json:"version"`
	Accessories map[string]AccessoryState `json:"accessories,omitempty"`
	Services    map[string]ServiceState   `json:"services,omitempty"`
}

// AccessoryState is multienv's mutable settings for one accessory.
//
// HostPort uses 0 to mean "explicitly disabled". The absence of an
// AccessoryState entry for an accessory means "no setting recorded —
// use the accessory's own default".
type AccessoryState struct {
	HostPort uint16 `json:"host_port"`
}

// ServiceState records when we last observed a service and which
// accessories it was using, with the relevant labels.
type ServiceState struct {
	LastSeenAt  time.Time                    `json:"last_seen_at"`
	Accessories map[string]map[string]string `json:"accessories,omitempty"`
}

// PathEnv is the environment variable that overrides the default state
// file location. Useful for tests and for users with non-standard home
// layouts.
const PathEnv = "MULTIENV_STATE_FILE"

// Path returns the absolute path to the state file. The MULTIENV_STATE_FILE
// environment variable takes precedence; otherwise it falls back to
// ~/.multienv/state.json.
func Path() string {
	if p := os.Getenv(PathEnv); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".multienv/state.json"
	}
	return filepath.Join(home, ".multienv", "state.json")
}

// Empty returns a fresh State value at the current schema version.
func Empty() *State {
	return &State{Version: CurrentVersion}
}

// Load reads the state file from disk. A missing file yields an empty
// state with no error so callers can treat absence as default.
// Other I/O or parse errors are returned alongside an empty state, so
// the caller can decide whether to surface them.
func Load() (*State, error) {
	data, err := os.ReadFile(Path())
	if errors.Is(err, fs.ErrNotExist) {
		return Empty(), nil
	}
	if err != nil {
		return Empty(), fmt.Errorf("read state: %w", err)
	}
	s := Empty()
	if err := json.Unmarshal(data, s); err != nil {
		return Empty(), fmt.Errorf("parse state: %w", err)
	}
	if s.Version == 0 {
		s.Version = CurrentVersion
	}
	return s, nil
}

// Save writes the state atomically: write to a temp file in the same
// directory, then rename. Creates the parent directory if needed.
func Save(s *State) error {
	if s == nil {
		return errors.New("state.Save: nil state")
	}
	s.Version = CurrentVersion
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// AccessoryHostPort returns (port, set). When set is false, the caller
// should fall back to the accessory's declared default.
func (s *State) AccessoryHostPort(name string) (uint16, bool) {
	if s == nil || s.Accessories == nil {
		return 0, false
	}
	a, ok := s.Accessories[name]
	if !ok {
		return 0, false
	}
	return a.HostPort, true
}

// SetAccessoryHostPort records the explicit host-port setting for an
// accessory. A port of 0 means "off".
func (s *State) SetAccessoryHostPort(name string, port uint16) {
	if s.Accessories == nil {
		s.Accessories = map[string]AccessoryState{}
	}
	a := s.Accessories[name]
	a.HostPort = port
	s.Accessories[name] = a
}

// RecordService stores or refreshes the entry for a service that was
// just observed during reconciliation. accessoryConfigs is keyed by
// accessory name and carries the per-accessory label map.
func (s *State) RecordService(key string, accessoryConfigs map[string]map[string]string) {
	if key == "" {
		return
	}
	if s.Services == nil {
		s.Services = map[string]ServiceState{}
	}
	entry := s.Services[key]
	entry.LastSeenAt = time.Now().UTC()
	// Replace the accessory map wholesale so labels that disappear are
	// reflected; we always trust the latest observation.
	if len(accessoryConfigs) > 0 {
		entry.Accessories = map[string]map[string]string{}
		for k, v := range accessoryConfigs {
			cp := make(map[string]string, len(v))
			maps.Copy(cp, v)
			entry.Accessories[k] = cp
		}
	} else {
		entry.Accessories = nil
	}
	s.Services[key] = entry
}

// ServiceUsage is the join of a service key with its per-accessory
// config, as returned by ServicesUsingAccessory.
type ServiceUsage struct {
	Key        string
	LastSeenAt time.Time
	Config     map[string]string
}

// ServicesUsingAccessory returns the recorded services that ever used
// the named accessory, sorted by key for stable output.
func (s *State) ServicesUsingAccessory(name string) []ServiceUsage {
	if s == nil || s.Services == nil {
		return nil
	}
	var out []ServiceUsage
	for key, svc := range s.Services {
		cfg, ok := svc.Accessories[name]
		if !ok {
			continue
		}
		out = append(out, ServiceUsage{Key: key, LastSeenAt: svc.LastSeenAt, Config: cfg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
