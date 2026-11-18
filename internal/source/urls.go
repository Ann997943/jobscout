package source

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/Ann997943/jobscout/internal/model"
)

// ParsedURL is what we can learn about a posting from its public URL alone.
// Used to discover boards from third-party lists and to match their entries
// against our database by ATS posting id (much more reliable than titles).
type ParsedURL struct {
	ATS        string
	Board      string // may be empty (e.g. company-hosted page with ?gh_jid=)
	ExternalID string // may be empty (board root URL)
}

var (
	ghPath     = regexp.MustCompile(`^/([\w.-]+)(?:/jobs/(\d+))?`)
	uuidSegRe  = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
	digitsOnly = regexp.MustCompile(`^\d+$`)
)

// ParseATSURL recognises Greenhouse, Lever and Ashby posting URLs.
func ParseATSURL(raw string) (ParsedURL, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ParsedURL{}, false
	}
	host := strings.ToLower(u.Host)
	q := u.Query()
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch {
	case strings.HasSuffix(host, "greenhouse.io"):
		p := ParsedURL{ATS: model.ATSGreenhouse}
		if strings.HasPrefix(u.Path, "/embed/") {
			p.Board = q.Get("for")
			p.ExternalID = q.Get("token")
		} else if m := ghPath.FindStringSubmatch(u.Path); m != nil {
			p.Board, p.ExternalID = m[1], m[2]
		}
		if p.ExternalID == "" {
			p.ExternalID = q.Get("gh_jid")
		}
		if p.ExternalID != "" && !digitsOnly.MatchString(p.ExternalID) {
			p.ExternalID = ""
		}
		return p, p.Board != "" || p.ExternalID != ""
	case host == "jobs.lever.co" || host == "jobs.eu.lever.co":
		if len(segs) == 0 || segs[0] == "" {
			return ParsedURL{}, false
		}
		p := ParsedURL{ATS: model.ATSLever, Board: segs[0]}
		if len(segs) > 1 && uuidSegRe.MatchString(segs[1]) {
			p.ExternalID = segs[1]
		}
		return p, true
	case host == "jobs.ashbyhq.com":
		if len(segs) == 0 || segs[0] == "" {
			return ParsedURL{}, false
		}
		board, _ := url.PathUnescape(segs[0])
		p := ParsedURL{ATS: model.ATSAshby, Board: board}
		if len(segs) > 1 && uuidSegRe.MatchString(segs[1]) {
			p.ExternalID = segs[1]
		}
		return p, true
	}
	// Company-hosted pages that embed Greenhouse (e.g. stripe.com/jobs?gh_jid=123).
	if id := q.Get("gh_jid"); digitsOnly.MatchString(id) {
		return ParsedURL{ATS: model.ATSGreenhouse, ExternalID: id}, true
	}
	return ParsedURL{}, false
}
