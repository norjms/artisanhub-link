// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Site is a paired printer site.
type Site struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	PublicKey string    `json:"public_key"`
	PairedAt  time.Time `json:"paired_at"`
}

// state is what the hub keeps across restarts: the paired sites and the overlay
// address of every printer it has seen, so addresses never move.
type state struct {
	Sites    []Site            `json:"sites"`
	Overlays map[string]string `json:"overlays"` // site|printer identity -> overlay address
}

func loadState(path string) (*state, error) {
	st := &state{Overlays: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Overlays == nil {
		st.Overlays = map[string]string{}
	}
	return st, nil
}

// save writes the state atomically: a new file, then a rename.
func (s *state) save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
