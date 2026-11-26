package store

import (
	"bytes"
	"testing"
)

func TestLinkPreviewsCacheCRUD(t *testing.T) {
	db := openTest(t)

	// Missing entry returns ok=false, err=nil
	if got, ok, err := db.GetLinkPreview("https://example.com"); err != nil || ok || got.URL != "" {
		t.Fatalf("missing GetLinkPreview = %+v, %v, %v", got, ok, err)
	}

	// Put 'ok' entry with thumbnail
	okEntry := LinkPreview{
		URL:       "https://example.com/post",
		Status:    "ok",
		Canonical: "https://example.com/post?ref=canon",
		Title:     "Example Post",
		Desc:      "A short description",
		Thumb:     []byte{0xff, 0xd8, 0xff, 0xe0},
		FetchedAt: 1700000000,
	}
	if err := db.PutLinkPreview(okEntry); err != nil {
		t.Fatalf("PutLinkPreview ok: %v", err)
	}
	got, ok, err := db.GetLinkPreview(okEntry.URL)
	if err != nil || !ok {
		t.Fatalf("GetLinkPreview ok = %+v, %v, %v", got, ok, err)
	}
	if got.URL != okEntry.URL || got.Status != "ok" || got.Canonical != okEntry.Canonical ||
		got.Title != okEntry.Title || got.Desc != okEntry.Desc ||
		!bytes.Equal(got.Thumb, okEntry.Thumb) || got.FetchedAt != okEntry.FetchedAt {
		t.Fatalf("GetLinkPreview ok mismatch: got %+v, want %+v", got, okEntry)
	}

	// Put 'none' entry (nil Thumb should bind cleanly as empty)
	noneEntry := LinkPreview{
		URL:       "https://example.com/empty",
		Status:    "none",
		FetchedAt: 1700000100,
	}
	if err := db.PutLinkPreview(noneEntry); err != nil {
		t.Fatalf("PutLinkPreview none: %v", err)
	}
	gotNone, ok, err := db.GetLinkPreview(noneEntry.URL)
	if err != nil || !ok {
		t.Fatalf("GetLinkPreview none = %+v, %v, %v", gotNone, ok, err)
	}
	if gotNone.Status != "none" || len(gotNone.Thumb) != 0 || gotNone.FetchedAt != 1700000100 {
		t.Fatalf("GetLinkPreview none mismatch: %+v", gotNone)
	}

	// Upsert updates existing entry
	updated := LinkPreview{
		URL:       noneEntry.URL,
		Status:    "ok",
		Canonical: "https://example.com/empty-resolved",
		Title:     "Now Found",
		Desc:      "Updated description",
		Thumb:     []byte{1, 2, 3},
		FetchedAt: 1700000200,
	}
	if err := db.PutLinkPreview(updated); err != nil {
		t.Fatalf("PutLinkPreview upsert: %v", err)
	}
	gotUpdated, ok, err := db.GetLinkPreview(noneEntry.URL)
	if err != nil || !ok {
		t.Fatalf("GetLinkPreview updated = %+v, %v, %v", gotUpdated, ok, err)
	}
	if gotUpdated.Status != "ok" || gotUpdated.Canonical != updated.Canonical ||
		gotUpdated.Title != updated.Title || gotUpdated.Desc != updated.Desc ||
		!bytes.Equal(gotUpdated.Thumb, updated.Thumb) || gotUpdated.FetchedAt != updated.FetchedAt {
		t.Fatalf("GetLinkPreview updated mismatch: got %+v, want %+v", gotUpdated, updated)
	}

	// ClearLinkPreviews deletes all rows
	if err := db.ClearLinkPreviews(); err != nil {
		t.Fatalf("ClearLinkPreviews: %v", err)
	}
	for _, u := range []string{okEntry.URL, noneEntry.URL} {
		if _, ok, err := db.GetLinkPreview(u); err != nil || ok {
			t.Fatalf("after ClearLinkPreviews, %q still present: ok=%v err=%v", u, ok, err)
		}
	}
}
