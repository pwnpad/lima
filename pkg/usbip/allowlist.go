// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lima-vm/lima/v2/pkg/limatype/filenames"
)

// AllowEntry is one device the host USB/IP server is permitted to export.
type AllowEntry struct {
	Name      string `json:"name,omitempty"`
	VendorID  string `json:"vendorID"`
	ProductID string `json:"productID"`
	BusAddr   string `json:"busAddr,omitempty"`
	Busid     string `json:"busid,omitempty"`
}

func AllowlistPath(instDir string) string {
	return filepath.Join(instDir, filenames.USBIPAllowlist)
}

func ReadAllowlist(instDir string) ([]AllowEntry, error) {
	b, err := os.ReadFile(AllowlistPath(instDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []AllowEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parsing usbip allowlist: %w", err)
	}
	return entries, nil
}

func WriteAllowlist(instDir string, entries []AllowEntry) error {
	if entries == nil {
		entries = []AllowEntry{}
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	path := AllowlistPath(instDir)
	tmp, err := os.CreateTemp(instDir, filenames.USBIPAllowlist+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// sameDevice reports whether two entries refer to the same device by
// vendor/product (case-insensitive) and busAddr (when both set). Busid is not
// part of identity — it can change across replug.
func sameDevice(a, b AllowEntry) bool {
	if !strings.EqualFold(a.VendorID, b.VendorID) || !strings.EqualFold(a.ProductID, b.ProductID) {
		return false
	}
	if a.BusAddr != "" && b.BusAddr != "" {
		return a.BusAddr == b.BusAddr
	}
	return true
}

func Allowed(list []AllowEntry, dev AllowEntry) bool {
	for _, e := range list {
		if sameDevice(e, dev) {
			return true
		}
	}
	return false
}

func AddEntry(list []AllowEntry, entry AllowEntry) []AllowEntry {
	out := make([]AllowEntry, 0, len(list)+1)
	for _, e := range list {
		if !sameDevice(e, entry) {
			out = append(out, e)
		}
	}
	return append(out, entry)
}

func RemoveEntry(list []AllowEntry, dev AllowEntry) ([]AllowEntry, bool) {
	out := make([]AllowEntry, 0, len(list))
	removed := false
	for _, e := range list {
		if sameDevice(e, dev) {
			removed = true
			continue
		}
		out = append(out, e)
	}
	return out, removed
}
