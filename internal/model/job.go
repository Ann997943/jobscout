// Package model holds the normalized job representation shared by every
// component (sources, store, extractor, notifier, benchmark).
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Supported applicant tracking systems.
const (
	ATSGreenhouse = "greenhouse"
	ATSLever      = "lever"
	ATSAshby      = "ashby"
)

// Board is one company's public job board on one ATS.
type Board struct {
	ATS   string `yaml:"ats"`
	Token string `yaml:"token"`
	Name  string `yaml:"name"`
}

func (b Board) ID() string { return b.ATS + ":" + b.Token }

// Job is a single posting, normalized across ATS providers.
type Job struct {
	ATS        string
	Board      string // board token
	ExternalID string // the ATS's own posting id
	Company    string
	Title      string
	Locations  []string
	URL        string
	// Description is plain text. Some sources (Greenhouse list endpoint) leave
	// it empty and it is filled in later by Fetcher.Detail.
	Description string
	PostedAt    time.Time // when the ATS says the posting was published (zero if unknown)
	// Hints are structured employment-type fields from the ATS (Greenhouse
	// custom fields, Lever commitment, Ashby employmentType). Not persisted.
	Hints []string
	// Version changes whenever the posting changes. Sources compute it from
	// whatever cheap signal they have (updated_at, or a content hash).
	Version string
}

// Key is globally unique: ATS posting ids are unique within an ATS.
func (j Job) Key() string { return j.ATS + ":" + j.ExternalID }

// HashContent is a helper for sources that have no updated_at field.
func HashContent(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (j Job) LocationString() string { return strings.Join(j.Locations, "; ") }
