package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var errConnectionChanged = errors.New("Connection changed after review; refresh and review the destination again")
var errUnverifiedTransfer = errors.New("This older transfer has no verified connection identity; choose the file and destination again")

// This opaque revision includes the private credential reference, never its
// value. Replacing credentials invalidates previously reviewed work too.
func connectionIdentity(p Profile) string {
	raw, _ := json.Marshal([]string{p.ID, p.Endpoint, p.Region, p.CAFile, credentialID(p)})
	if p.PathStyle {
		raw = append(raw, 1)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Caller holds profileMu. An empty expected identity is used only by internal
// callers accepting work now; HTTP mutations must supply their reviewed identity.
func (a *App) connectionLocked(profileID, expected string) (Profile, error) {
	for _, p := range a.profiles {
		if p.ID == profileID {
			if expected != "" && connectionIdentity(p) != expected {
				return Profile{}, errConnectionChanged
			}
			return p, nil
		}
	}
	return Profile{}, errors.New("Connection not found")
}

func (a *App) connectionIdentity(profileID string) (string, error) {
	a.profileMu.RLock()
	defer a.profileMu.RUnlock()
	p, err := a.connectionLocked(profileID, "")
	if err != nil {
		return "", err
	}
	return connectionIdentity(p), nil
}
