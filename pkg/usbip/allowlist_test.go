// SPDX-FileCopyrightText: Copyright The Lima Authors
// SPDX-License-Identifier: Apache-2.0

package usbip

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestReadAllowlistMissing(t *testing.T) {
	entries, err := ReadAllowlist(t.TempDir())
	assert.NilError(t, err)
	assert.Equal(t, len(entries), 0)
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := []AllowEntry{
		{Name: "alfa", VendorID: "0bda", ProductID: "8812", Busid: "20-3"},
		{VendorID: "9ac4", ProductID: "4b8f"},
	}
	assert.NilError(t, WriteAllowlist(dir, want))
	got, err := ReadAllowlist(dir)
	assert.NilError(t, err)
	assert.DeepEqual(t, got, want)
}

func TestAllowedMatching(t *testing.T) {
	list := []AllowEntry{{VendorID: "0BDA", ProductID: "8812"}}
	assert.Equal(t, Allowed(list, AllowEntry{VendorID: "0bda", ProductID: "8812"}), true)
	assert.Equal(t, Allowed(list, AllowEntry{VendorID: "1234", ProductID: "8812"}), false)

	// Busid is not part of identity: a replug (new busid) still matches.
	bl := []AllowEntry{{VendorID: "0bda", ProductID: "8812", Busid: "20-3"}}
	assert.Equal(t, Allowed(bl, AllowEntry{VendorID: "0bda", ProductID: "8812", Busid: "20-4"}), true)

	// busAddr disambiguates two devices sharing a vendor/product.
	dl := []AllowEntry{{VendorID: "0bda", ProductID: "8812", BusAddr: "20-3"}}
	assert.Equal(t, Allowed(dl, AllowEntry{VendorID: "0bda", ProductID: "8812", BusAddr: "20-7"}), false)
	assert.Equal(t, Allowed(dl, AllowEntry{VendorID: "0bda", ProductID: "8812", BusAddr: "20-3"}), true)
}

func TestAddEntryReplaces(t *testing.T) {
	list := []AllowEntry{{VendorID: "0bda", ProductID: "8812", Busid: "20-3"}}
	list = AddEntry(list, AllowEntry{VendorID: "0bda", ProductID: "8812", Busid: "20-5"})
	assert.Equal(t, len(list), 1)
	assert.Equal(t, list[0].Busid, "20-5")

	list = AddEntry(list, AllowEntry{VendorID: "1111", ProductID: "2222"})
	assert.Equal(t, len(list), 2)
}

func TestRemoveEntry(t *testing.T) {
	list := []AllowEntry{
		{VendorID: "0bda", ProductID: "8812"},
		{VendorID: "1111", ProductID: "2222"},
	}
	list, removed := RemoveEntry(list, AllowEntry{VendorID: "0bda", ProductID: "8812"})
	assert.Equal(t, removed, true)
	assert.Equal(t, len(list), 1)
	assert.Equal(t, list[0].VendorID, "1111")

	_, removed = RemoveEntry(list, AllowEntry{VendorID: "dead", ProductID: "beef"})
	assert.Equal(t, removed, false)
}
